package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
	"github.com/JeremiahM37/grimoire/go/internal/anchor/anchorlog"
	"github.com/JeremiahM37/grimoire/go/internal/cues"
	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/identity"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Anchors: a claim names the thing it is about, and the store checks it.
//
// A write may carry probes (memory.Entry.Anchors). They are accepted only from
// a loopback peer that is not a remote or OAuth caller, because a probe result
// leaks a bit about the server's files. Each accepted anchor is validated and
// run once; only those that pass, or that cannot be checked from here, are
// stored. A failing anchor never fails the write: the response says which one
// failed so the writer can fix it.
//
// Recall gates on the stored anchors only behind exp "anchor". With the flag
// off nothing here runs and recall output is unchanged.

const (
	anchorFlag = "anchor"
	// maxAnchorsPerWrite bounds the probes one write may run.
	maxAnchorsPerWrite = 16
	// anchorCacheTTL is how long a recall reuses a fact's probe verdict.
	anchorCacheTTL = 10 * time.Minute
	// anchorCacheMax bounds the cache. Expired entries are dropped first.
	anchorCacheMax = 10000
)

func anchorsOn() bool { return exp.On(anchorFlag) }

// anchorRecallOn reports whether a recall runs the checks a fact carries when
// the experiment flag is off. On unless GRIMOIRE_ANCHOR_RECALL=0.
func anchorRecallOn() bool { return os.Getenv("GRIMOIRE_ANCHOR_RECALL") != "0" }

// checkedHere reports whether every check a fact carries runs on this machine.
func checkedHere(e memory.Entry) bool {
	if e.Anchors == "" {
		return false
	}
	local := strings.ToLower(hostname())
	if e.Host != "" && e.Host != local {
		return false
	}
	list, err := anchor.Decode(e.Anchors)
	if err != nil {
		return true
	}
	for _, a := range list {
		if h := strings.ToLower(a.Args["host"]); h != "" && h != local {
			return false
		}
	}
	return true
}

// anchorReport is one anchor's outcome in a write's response.
type anchorReport struct {
	Type    string            `json:"type"`
	Args    map[string]string `json:"args,omitempty"`
	Result  string            `json:"result"`
	Message string            `json:"message,omitempty"`
	// probe is the run behind this report, kept for the result history. It is
	// unexported, so it never reaches a response.
	probe *anchorObs
}

// errTooManyAnchors refuses a write that asks for more probes than allowed.
var errTooManyAnchors = errors.New("at most 16 anchors per write")

// loopbackPeer reports whether the connection itself came from this machine.
// It reads the peer address, not X-Forwarded-For, because a forwarded address
// is caller-supplied.
func loopbackPeer(r *http.Request) bool {
	addr, ok := identity.PeerAddr(r)
	return ok && addr.IsLoopback()
}

// checkWriteAnchors decides what a write's anchors do. It returns the encoded
// anchors to store, one report per anchor, and why they were ignored ("" when
// they were considered). An ignored set is not an error: the write proceeds.
func checkWriteAnchors(r *http.Request, m memoryIn) (enc string, reports []anchorReport, ignored string, err error) {
	if len(m.Anchors) == 0 {
		return "", nil, "", nil
	}
	if len(m.Anchors) > maxAnchorsPerWrite {
		return "", nil, "", errTooManyAnchors
	}
	switch {
	case m.Event:
		return "", []anchorReport{}, "event", nil
	case m.Visibility == memory.VisSealed:
		return "", []anchorReport{}, "sealed fact", nil
	case !localCaller(r, m):
		return "", []anchorReport{}, "remote caller", nil
	}
	enc, reports = runAnchorSet(r.Context(), m.Anchors, m.storeHost)
	return enc, reports, "", nil
}

// runAnchorSet validates and runs a caller's anchors, the way a write and an
// annotate both need them. An anchor that names no host takes the fact's host
// (anchor.Inherit). It returns the encoded set to store, which holds the anchors
// that passed or could not be checked here, and one report per anchor.
func runAnchorSet(ctx context.Context, raws []map[string]any, factHost string) (string, []anchorReport) {
	reports := []anchorReport{}
	var parsed []anchor.Anchor
	type slot struct {
		convErr string
		idx     int
	}
	slots := make([]slot, 0, len(raws))
	for _, raw := range raws {
		a, convErr := anchor.FromMap(raw)
		if convErr != nil {
			slots = append(slots, slot{convErr: convErr.Error(), idx: -1})
			continue
		}
		slots = append(slots, slot{idx: len(parsed)})
		parsed = append(parsed, a)
	}
	parsed = anchor.Inherit(parsed, factHost)
	var kept []anchor.Anchor
	for _, sl := range slots {
		if sl.idx < 0 {
			reports = append(reports, anchorReport{Result: string(anchor.Error), Message: sl.convErr})
			continue
		}
		a := parsed[sl.idx]
		rep := anchorReport{Type: a.Type, Args: a.Args}
		if !anchor.Storable(a.Type) {
			rep.Result, rep.Message = string(anchor.Error), "not a stored probe type"
		} else {
			res := anchor.Run(ctx, a)
			rep.Result, rep.Message = string(res.Status), res.Message
			rep.probe = &anchorObs{a: a, res: res}
			if res.Status == anchor.Pass || res.Status == anchor.Skipped {
				kept = append(kept, a)
			}
		}
		reports = append(reports, rep)
	}
	return anchor.Encode(kept), reports
}

// maxAutoCandidates bounds how many references in one claim's text are probed
// for automatic anchors, and maxAutoStored how many of them a fact keeps.
const (
	maxAutoCandidates = 16
	maxAutoStored     = 4
)

// autoAnchorsOn is the off switch: GRIMOIRE_ANCHOR_AUTO=0 stops the store from
// deriving anchors from a claim's text. It is on by default.
func autoAnchorsOn() bool { return os.Getenv("GRIMOIRE_ANCHOR_AUTO") != "0" }

// autoEligible says whether a write may have anchors derived from its text: an
// accepted local claim that sent no anchors of its own. Events, preferences and
// rules, sealed facts, remote writes and restores are left alone.
func autoEligible(r *http.Request, m memoryIn) bool {
	return autoAnchorsOn() && len(m.Anchors) == 0 && !m.Event && m.restore == nil &&
		m.Visibility != memory.VisSealed && !isPrefKindName(m.Kind) &&
		!isPrefKindName(m.Category) && localCaller(r, m)
}

// autoAnchors probes the paths and units a claim names and keeps the ones that
// pass, marked auto=1. Nothing is stored when none pass. The list is returned
// for the response as well as the encoded set.
func autoAnchors(ctx context.Context, text, factHost string) (string, []anchorReport, []anchorObs) {
	cands := anchor.ExtractRefs(text)
	if len(cands) > maxAutoCandidates {
		cands = cands[:maxAutoCandidates]
	}
	cands = anchor.Inherit(cands, factHost)
	var kept []anchor.Anchor
	reports := []anchorReport{}
	var obs []anchorObs
	for _, c := range cands {
		if len(kept) >= maxAutoStored {
			break
		}
		a := anchor.AutoAnchor(c)
		res := anchor.Run(ctx, a)
		obs = append(obs, anchorObs{a: a, res: res})
		if res.Status == anchor.Pass {
			kept = append(kept, a)
			reports = append(reports, anchorReport{Type: a.Type, Args: a.Args, Result: string(res.Status)})
		}
	}
	return anchor.Encode(kept), reports, obs
}

// Advice is what a writer is told about a claim it just stored. It never blocks
// or changes a write, and it is computed without a model.
const (
	adviceNoAnchors = "names nothing checkable: add an anchor for the file, service, port or commit this is about, or set event:true if it is history"
	adviceEvent     = "reads like something that happened: set event:true so it is kept as history"
)

// pastTenseRE is deliberately narrow: a sentence that opens by reporting an
// action, or that names a date, is probably history.
var pastTenseRE = regexp.MustCompile(`^(?:Fixed|Added|Removed|Implemented|Ran|Verified|Tested|Completed|Merged|Deployed|Updated|Created|Investigated|Found that)\b|on 20\d\d-\d\d-\d\d`)

// writeAdvice returns the advice for one accepted write. storedEnc is the
// anchor set the write stores.
func writeAdvice(m memoryIn, text, storedEnc string) []string {
	if m.restore != nil || m.Event || isPrefKindName(m.Kind) || isPrefKindName(m.Category) {
		return nil
	}
	var out []string
	if storedEnc == "" {
		out = append(out, adviceNoAnchors)
	}
	if isPastReport(text) {
		out = append(out, adviceEvent)
	}
	return out
}

// isPastReport is the one test for a claim that reads like history: a sentence
// that opens by reporting an action, or that names a date. Writer advice and
// automatic event marking both use it.
func isPastReport(text string) bool {
	return pastTenseRE.MatchString(strings.TrimSpace(text))
}

// eventAutoOn is the off switch: GRIMOIRE_EVENT_AUTO=0 stops the store from
// marking history on its own. It is on by default.
func eventAutoOn() bool { return os.Getenv("GRIMOIRE_EVENT_AUTO") != "0" }

// autoEvent reports whether one fact of a write is marked as an event without
// the caller asking. It needs all of: the switch on; the caller did not send
// event; the write is split into several facts or is a handoff or checkpoint;
// the fact reads as history; and the write stored no anchors (a fact that
// carries a check is a claim, never history). Preferences, rules and restores
// are left alone.
func autoEvent(m memoryIn, nFacts int, fact string) bool {
	if !eventAutoOn() || m.eventSent || m.Event || m.restore != nil || m.anchorsEnc != "" {
		return false
	}
	if isPrefKindName(m.Kind) || isPrefKindName(m.Category) {
		return false
	}
	cat := strings.ToLower(strings.TrimSpace(m.Category))
	if nFacts < 2 && cat != "handoff" && cat != "checkpoint" {
		return false
	}
	return isPastReport(fact)
}

// dropString returns in without the given string.
func dropString(in []string, drop string) []string {
	out := in[:0:0]
	for _, v := range in {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

// anchorVerdict is what the probes said about one fact, as recall needs it.
type anchorVerdict struct {
	at      time.Time
	failed  bool   // some anchor did not hold
	errored bool   // the first problem was a probe that could not answer
	kind    string // type of the first anchor that failed or could not answer
	what    string // what the first failing anchor was about (see subject)
	passed  int    // anchors that held
	obs     []anchorObs
}

// anchorCache holds verdicts for anchorCacheTTL, keyed by fact and its anchors.
type anchorCache struct {
	mu sync.Mutex
	m  map[string]anchorVerdict
}

// verdictFor is the cached probe verdict for a fact's stored anchors.
func (s *Server) verdictFor(e memory.Entry, origin string) anchorVerdict {
	key := e.ID + "\x00" + e.Host + "\x00" + e.Anchors
	now := time.Now()
	s.anchors.mu.Lock()
	if v, ok := s.anchors.m[key]; ok && now.Sub(v.at) < anchorCacheTTL {
		s.anchors.mu.Unlock()
		return v
	}
	s.anchors.mu.Unlock()

	v := probeStored(context.Background(), e.Anchors, e.Host)
	v.at = now
	recordObs(s.anchorLog(), e.ID, origin, v.obs)
	v.obs = nil

	s.anchors.mu.Lock()
	if s.anchors.m == nil {
		s.anchors.m = map[string]anchorVerdict{}
	}
	if len(s.anchors.m) >= anchorCacheMax {
		for k, old := range s.anchors.m {
			if now.Sub(old.at) >= anchorCacheTTL {
				delete(s.anchors.m, k)
			}
		}
		if len(s.anchors.m) >= anchorCacheMax {
			s.anchors.m = map[string]anchorVerdict{}
		}
	}
	s.anchors.m[key] = v
	s.anchors.mu.Unlock()
	return v
}

// probeStored runs a fact's stored anchors and summarises them. Stored anchors
// that cannot be decoded are a failure: something edited them outside the
// write path, and nothing about the claim can be vouched for.
func probeStored(ctx context.Context, enc, factHost string) anchorVerdict {
	list, err := anchor.Decode(enc)
	if err != nil {
		return anchorVerdict{failed: true, kind: ""}
	}
	var v anchorVerdict
	for _, a := range anchor.Inherit(list, factHost) {
		res := anchor.Run(ctx, a)
		v.obs = append(v.obs, anchorObs{a: a, res: res})
		switch res.Status {
		case anchor.Pass:
			v.passed++
		case anchor.Fail, anchor.Error:
			if !v.failed {
				v.failed, v.kind, v.what = true, a.Type, subject(a)
				v.errored = res.Status == anchor.Error
			}
		}
	}
	return v
}

// applyAnchors gates a recall assessment on the fact's stored anchors. A failing
// anchor sends the fact to verify, naming the anchor's type and what it was
// about; a fact whose writer's anchors all hold is use. Anchors that could only
// be skipped leave it alone, and so do automatic anchors that hold: an automatic
// anchor shows that a named file or unit exists, not that the claim is right,
// so the model's verdict stands.
func (s *Server) applyAnchors(a *memory.Assessment, e memory.Entry) {
	if e.Anchors == "" {
		return
	}
	v := s.verdictFor(e, anchorlog.OriginRecall)
	switch {
	case v.failed && v.kind == "":
		a.Action, a.Reason = memory.ActionVerify, "stored anchors are unreadable"
	case v.failed && v.errored:
		a.Action, a.Reason = memory.ActionVerify, "anchor "+v.kind+" could not be checked"
	case v.failed:
		a.Action, a.Reason = memory.ActionVerify, "anchor "+v.kind+" does not hold"+about(v.what)
	case v.passed > 0 && writerAnchored(e.Anchors):
		a.Action, a.Reason = memory.ActionUse, "stored anchors hold"
	}
}

// applyStaleness is the part of freshness that needs the anchor flag. A claim
// with wording that goes stale and no writer's anchor is asked to be checked:
// nothing says what would show it is still true. Events and preferences are
// not claims about the present, so they are never floored. Where names the
// machine a claim is about, when that is not this one, so an agent knows the
// claim is not about the box it is running on.
func applyStaleness(a *memory.Assessment, e memory.Entry) {
	if !e.Event && !isPreferenceKind(e) && !writerAnchored(e.Anchors) &&
		memory.FloorShape(e.Text) && a.Action != memory.ActionVerify {
		a.Action, a.Reason = memory.ActionVerify, "volatile wording and no anchor"
	}
	if e.Host != "" && e.Host != strings.ToLower(hostname()) {
		a.Where = e.Host
	}
}

// writerAnchored reports whether a stored anchor set has at least one anchor the
// writer supplied. One that cannot be decoded counts as writer-supplied, so the
// floor does not second-guess a fact the anchor check already flagged.
func writerAnchored(enc string) bool {
	if enc == "" {
		return false
	}
	list, err := anchor.Decode(enc)
	if err != nil {
		return true
	}
	return hasWriterAnchor(list)
}

// subject names what an anchor probed, for a reason a reader can act on: the
// path, unit, port, URL, binary or repository. It never returns the literal a
// file_contains probe looked for.
func subject(a anchor.Anchor) string {
	for _, k := range []string{"path", "unit", "port", "url", "bin", "repo"} {
		if v := a.Args[k]; v != "" {
			return v
		}
	}
	return ""
}

// about formats subject for a reason, or nothing when there is none.
func about(what string) string {
	if what == "" {
		return ""
	}
	return ": " + what
}

// anchorTally is the admin report over every live fact's anchors.
// AnchoredClaims counts claims with at least one anchor the writer supplied;
// AutoAnchoredClaims counts claims whose anchors are all derived by the store.
type anchorTally struct {
	Facts              int                  `json:"facts"`
	Events             int                  `json:"events"`
	Preferences        int                  `json:"preferences"`
	Claims             int                  `json:"claims"`
	AnchoredClaims     int                  `json:"anchored_claims"`
	AutoAnchoredClaims int                  `json:"auto_anchored_claims"`
	Anchors            int                  `json:"anchors"`
	Pass               int                  `json:"pass"`
	Fail               int                  `json:"fail"`
	Error              int                  `json:"error"`
	Skipped            int                  `json:"skipped"`
	SkippedReasons     map[string]int       `json:"skipped_reasons"`
	ByHost             map[string]hostTally `json:"by_host"`
	Failing            []failingAnchor      `json:"failing"`
	Procedures         procedureTally       `json:"procedures"`
}

// hostTally is the claim count for one machine a claim is about. A claim with
// no recorded host is counted under "unrecorded".
type hostTally struct {
	Claims         int `json:"claims"`
	AnchoredClaims int `json:"anchored_claims"`
}

// unrecordedHost is the by_host key for facts that name no machine.
const unrecordedHost = "unrecorded"

// failingAnchor names one anchor that did not hold. It carries no fact text.
type failingAnchor struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Anchor  string `json:"anchor"`
	Result  string `json:"result"`
	Message string `json:"message,omitempty"`
}

// isPreferenceKind reports whether a fact is a preference or a rule. Kind is
// stored as the category when no category is given (see remember).
func isPreferenceKind(e memory.Entry) bool {
	return e.Category == "preference" || e.Category == "rule"
}

// memoryAnchors runs every stored anchor and reports the tally. Admin only.
// It returns no fact text: a failing anchor is named by its type and path.
func (s *Server) memoryAnchors(w http.ResponseWriter, r *http.Request) {
	rels, err := s.Vault.WalkDir(memory.Dir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := vault.Now()
	t := anchorTally{Failing: []failingAnchor{}, ByHost: map[string]hostTally{},
		SkippedReasons: map[string]int{"remote": 0, "host_not_allowlisted": 0, "unsafe_argument": 0, "other": 0}}
	for _, rel := range rels {
		note, err := s.Vault.Read(rel)
		if err != nil {
			continue
		}
		for _, e := range memory.Parse(note.Body) {
			if !e.Live(now) {
				continue
			}
			t.Facts++
			claim := false
			switch {
			case e.Event:
				t.Events++
			case isPreferenceKind(e):
				t.Preferences++
			default:
				t.Claims++
				claim = true
			}
			if e.Anchors == "" {
				if claim {
					t.tallyHost(e.Host, false)
				}
				continue
			}
			list, err := anchor.Decode(e.Anchors)
			if err != nil {
				// Anchors that cannot be read were written by something other than
				// the store; they count as the writer's and as failing.
				if claim {
					t.tallyHost(e.Host, true)
				}
				t.Anchors++
				t.Fail++
				t.Failing = append(t.Failing, failingAnchor{ID: e.ID, Path: rel,
					Result: string(anchor.Fail), Message: "stored anchors are unreadable"})
				continue
			}
			if claim {
				t.tallyHost(e.Host, hasWriterAnchor(list))
				if !hasWriterAnchor(list) {
					t.AutoAnchoredClaims++
				}
			}
			if claim && hasWriterAnchor(list) {
				t.AnchoredClaims++
			}
			var obs []anchorObs
			for _, a := range anchor.Inherit(list, e.Host) {
				res := anchor.Run(r.Context(), a)
				obs = append(obs, anchorObs{a: a, res: res})
				t.Anchors++
				switch res.Status {
				case anchor.Pass:
					t.Pass++
				case anchor.Skipped:
					t.Skipped++
					t.SkippedReasons[anchor.SkipReason(res)]++
				case anchor.Fail:
					t.Fail++
				default:
					t.Error++
				}
				if res.Status == anchor.Fail || res.Status == anchor.Error {
					t.Failing = append(t.Failing, failingAnchor{ID: e.ID, Path: rel,
						Anchor: a.Type, Result: string(res.Status), Message: res.Message})
				}
			}
			recordObs(s.anchorLog(), e.ID, anchorlog.OriginCLI, obs)
		}
	}
	t.Procedures = s.procedureTallyOf(r.Context())
	writeJSON(w, http.StatusOK, t)
}

// tallyHost counts one claim under the machine it is about. anchored says
// whether the writer supplied an anchor for it.
func (t *anchorTally) tallyHost(host string, anchored bool) {
	if host == "" {
		host = unrecordedHost
	}
	ht := t.ByHost[host]
	ht.Claims++
	if anchored {
		ht.AnchoredClaims++
	}
	t.ByHost[host] = ht
}

// hasWriterAnchor reports whether any anchor in a set was supplied by a writer
// rather than derived by the store.
func hasWriterAnchor(list []anchor.Anchor) bool {
	for _, a := range list {
		if !anchor.IsAuto(a) {
			return true
		}
	}
	return false
}

// deadRef is one reference in a note that no longer resolves.
type deadRef struct {
	Path string `json:"path"`
	Ref  string `json:"ref"`
}

// memoryRefs scans every note for dead references. Admin only. It reports the
// reference, never the note's text.
func (s *Server) memoryRefs(w http.ResponseWriter, r *http.Request) {
	paths, err := s.Vault.Walk()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	notes, refs, errs := 0, 0, 0
	dead := []deadRef{}
	for _, rel := range paths {
		note, err := s.Vault.Read(rel)
		if err != nil {
			continue
		}
		notes++
		for _, a := range anchor.ExtractRefs(note.Body) {
			refs++
			res := anchor.Run(r.Context(), a)
			switch res.Status {
			case anchor.Fail:
				ref := a.Args["path"]
				if a.Type == anchor.RefUnit {
					ref = a.Args["unit"]
				}
				dead = append(dead, deadRef{Path: rel, Ref: ref})
			case anchor.Error:
				errs++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"notes": notes, "refs": refs, "dead_total": len(dead), "errors": errs, "dead": dead,
	})
}

// localCaller is the test checkWriteAnchors and writeHost share: the write
// came from this machine's own loopback, and did not say it was remote.
func localCaller(r *http.Request, m memoryIn) bool {
	return !m.CallerRemote && loopbackPeer(r)
}

// Host labels. A claim says how a machine IS, and facts did not record which
// machine that was, so a path or unit named in one could not be checked on the
// right host. A label is a machine name in the same character set as a DNS
// name, so it can be shown and used as a map key but never becomes a command.
var hostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.\-]{0,62}$`)

// hostname is this server's short, lowercased machine name. It is the same
// name the check runner compares a host label with, so a fact stamped here is
// always checked here.
func hostname() string { return anchor.LocalHost() }

// writeHost decides the host= a write stores. An explicit host must match
// hostRE. Without one, a local claim (not an event, not a preference or rule)
// is stamped with this server's name; anything else is stored with no host.
func writeHost(r *http.Request, m memoryIn) (string, error) {
	if m.Host != "" {
		if !hostRE.MatchString(m.Host) {
			return "", errors.New("host must be a machine name: letters, digits, dots and dashes, at most 63 characters")
		}
		return strings.ToLower(m.Host), nil
	}
	if m.Event || isPrefKindName(m.Kind) || isPrefKindName(m.Category) || !localCaller(r, m) {
		return "", nil
	}
	if h := hostname(); hostRE.MatchString(h) {
		return h, nil
	}
	return "", nil
}

// isPrefKindName is isPreferenceKind for a kind or category as the caller
// named it, before it is stored.
func isPrefKindName(s string) bool {
	s = strings.TrimSpace(s)
	return s == "preference" || s == "rule"
}

// Retrofit: annotate facts that are already stored, without changing their
// text. A claim written before anchors existed, or on this machine when it
// was about another one, can be given its probes and its host afterwards. Only
// the trailer of the fact's own line changes: the text, the id, the stamp and
// every other trailer field stay as they were.

// maxAnnotateItems bounds one annotate request.
const maxAnnotateItems = 200

// annotateItem names one stored fact and what to change on it. A nil field is
// left alone; a set empty host clears the host.
type annotateItem struct {
	ID      string           `json:"id"`
	Path    string           `json:"path"`
	Event   *bool            `json:"event,omitempty"`
	Host    *string          `json:"host,omitempty"`
	Anchors []map[string]any `json:"anchors,omitempty"`
}

type annotateIn struct {
	Items        []annotateItem `json:"items"`
	DryRun       bool           `json:"dry_run"`
	CallerRemote bool           `json:"caller_remote"`
}

// annotateAnchor is one anchor's outcome for an annotated fact.
type annotateAnchor struct {
	Anchor  string `json:"anchor"`
	Result  string `json:"result"`
	Message string `json:"message,omitempty"`
}

// annotateOut is what one item did. Error says why it did nothing.
type annotateOut struct {
	ID      string           `json:"id"`
	Changed bool             `json:"changed"`
	Anchors []annotateAnchor `json:"anchors"`
	Error   string           `json:"error,omitempty"`
}

// memoryAnnotate changes the trailers of stored facts. Admin only, and only
// for a loopback caller that is not remote. dry_run reports without writing;
// anchors are still run so the answer says what they would do.
//
// Items are applied in order against an in-memory copy of each note, and every
// note the request touches is written and indexed once at the end. A run of
// 177 items over 10 notes is 10 note writes, not 177. notes_written says how
// many were written.
func (s *Server) memoryAnnotate(w http.ResponseWriter, r *http.Request) {
	var in annotateIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.CallerRemote || !loopbackPeer(r) {
		writeErr(w, http.StatusForbidden, "annotate is for the local agent only")
		return
	}
	if len(in.Items) == 0 || len(in.Items) > maxAnnotateItems {
		writeErr(w, http.StatusBadRequest, "items must hold 1 to 200 facts")
		return
	}
	b := &annotateBatch{s: s, notes: map[string]*annotateNote{}}
	out := make([]annotateOut, 0, len(in.Items))
	rels := make([]string, 0, len(in.Items))
	for _, item := range in.Items {
		o, rel := s.annotateOne(r, item, b, in.DryRun)
		out = append(out, o)
		rels = append(rels, rel)
	}
	written := 0
	if !in.DryRun {
		var errs map[string]error
		written, errs = b.flush()
		for i := range out {
			if err, bad := errs[rels[i]]; bad && out[i].Changed {
				out[i].Changed = false
				out[i].Error = "could not write the note: " + err.Error()
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dry_run": in.DryRun, "notes_written": written, "items": out})
}

// annotateNote is one memory note as an annotate request sees it: read once,
// parsed once, and changed in memory until the request's single write.
type annotateNote struct {
	note    *vault.Note
	err     error
	entries []memory.Entry
	touched map[int]bool // indexes into entries whose trailer changed
}

// annotateBatch holds the notes one annotate request touches.
type annotateBatch struct {
	s     *Server
	notes map[string]*annotateNote
	order []string // first-touch order, so the writes are deterministic
}

// load returns the cached note at rel, reading it on first use. A failed read
// is cached too, so every item on a missing note reports the same thing.
func (b *annotateBatch) load(rel string) *annotateNote {
	if n, ok := b.notes[rel]; ok {
		return n
	}
	n := &annotateNote{touched: map[int]bool{}}
	if note, err := b.s.Vault.Read(rel); err != nil {
		n.err = err
	} else {
		n.note = note
		n.entries = memory.Parse(note.Body)
	}
	b.notes[rel] = n
	b.order = append(b.order, rel)
	return n
}

// flush writes and indexes each note that changed, once. It returns how many
// notes were written and, by path, the error that stopped a note's write.
func (b *annotateBatch) flush() (int, map[string]error) {
	errs := map[string]error{}
	written := 0
	for _, rel := range b.order {
		n := b.notes[rel]
		if n.note == nil || len(n.touched) == 0 {
			continue
		}
		var edited []memory.Entry
		for i := range n.entries {
			if n.touched[i] {
				edited = append(edited, n.entries[i])
			}
		}
		b.s.History.Snapshot(rel, n.note.Body)
		body := memory.Rewrite(n.note.Body, edited)
		if _, err := b.s.Vault.Write(rel, body, n.note.Frontmatter.Clone()); err != nil {
			errs[rel] = err
			continue
		}
		if _, err := b.s.Index.Upsert(rel); err != nil {
			errs[rel] = err
			continue
		}
		written++
	}
	return written, errs
}

// annotateOne works out and, unless dry, stages one item's change in the batch.
// It returns the note path it looked at, so the caller can attach a write
// failure to the right items.
func (s *Server) annotateOne(r *http.Request, item annotateItem, b *annotateBatch, dry bool) (annotateOut, string) {
	res := annotateOut{ID: item.ID, Anchors: []annotateAnchor{}}
	fail := func(msg string) (annotateOut, string) { res.Error = msg; return res, "" }

	rel := normPath(item.Path)
	if item.ID == "" || !index.IsMemoryPath(rel) {
		return fail("id and a memory note path are required")
	}
	n := b.load(rel)
	if n.err != nil {
		return fail("note not found")
	}
	var hits []int
	for i, e := range n.entries {
		if e.ID == item.ID {
			hits = append(hits, i)
		}
	}
	switch {
	case len(hits) == 0:
		return fail("fact not found")
	case len(hits) > 1:
		return fail("more than one fact has this id")
	}
	idx := hits[0]
	cur := n.entries[idx]
	if cur.HandWritten {
		return fail("hand-written fact: its trailer is edited by hand")
	}

	next := cur
	if item.Event != nil {
		next.Event = *item.Event
		if next.Event {
			// An event carries no anchors, as it would not from a write.
			next.Anchors = ""
		}
	}
	if item.Host != nil {
		h := strings.ToLower(strings.TrimSpace(*item.Host))
		if h != "" && !hostRE.MatchString(h) {
			return fail("host must be a machine name: letters, digits, dots and dashes, at most 63 characters")
		}
		next.Host = h
	}
	if len(item.Anchors) > 0 {
		switch {
		case len(item.Anchors) > maxAnchorsPerWrite:
			return fail(errTooManyAnchors.Error())
		case next.Event:
			return fail("an event carries no anchors")
		case cur.Visibility == memory.VisSealed:
			return fail("sealed fact: its text and anchors are not readable here")
		}
		enc, reports := runAnchorSet(r.Context(), item.Anchors, next.Host)
		recordObs(s.anchorLog(), item.ID, anchorlog.OriginAnnotate, reportObs(reports))
		for _, rep := range reports {
			res.Anchors = append(res.Anchors, annotateAnchor{Anchor: rep.Type, Result: rep.Result, Message: rep.Message})
		}
		// Existing anchors are replaced only when at least one new one holds.
		if anyPassed(reports) {
			next.Anchors = enc
		}
	}
	res.Changed = next != cur
	if !res.Changed || dry {
		return res, rel
	}
	// Later items on this fact see this change, as they would after a write.
	n.entries[idx] = next
	n.touched[idx] = true
	return res, rel
}

// anyPassed reports whether an anchor run held at least once.
func anyPassed(reports []anchorReport) bool {
	for _, rep := range reports {
		if rep.Result == string(anchor.Pass) {
			return true
		}
	}
	return false
}

// arbitrateOn is the off switch for anchor arbitration: GRIMOIRE_ANCHOR_ARBITRATE=0
// lets a new write replace an anchored fact whatever its anchors say. It is on
// by default.
func arbitrateOn() bool { return os.Getenv("GRIMOIRE_ANCHOR_ARBITRATE") != "0" }

// isHumanCorrection reports whether a write comes from a person, who may
// replace any fact the rules let them, anchored or not. Authority is decided
// by the same rule the rest of the write path uses (memory.AuthorityOf), so a
// pulled origin cannot claim to be a person here either.
func isHumanCorrection(m memoryIn) bool {
	return memory.AuthorityOf(strings.TrimSpace(m.Origin), m.Human) == memory.AuthorityHuman
}

// anchorsStillHold reports whether an existing fact's stored anchors say it is
// still true: at least one ran and none failed. A fact with only automatic
// anchors, or with none, gives no information, so it reports false and the
// update proceeds as it always has. Probes are the same cached runner recall
// uses, and they inherit the fact's host.
func (s *Server) anchorsStillHold(old memory.Entry) bool {
	if old.Anchors == "" {
		return false
	}
	list, err := anchor.Decode(old.Anchors)
	if err != nil {
		return false
	}
	writerAnchored := false
	for _, a := range list {
		if !anchor.IsAuto(a) {
			writerAnchored = true
		}
	}
	if !writerAnchored {
		return false
	}
	v := s.verdictFor(old, anchorlog.OriginWrite)
	return v.passed > 0 && !v.failed
}

// maxAnchorCues bounds the action cues one fact gets from its anchors.
const maxAnchorCues = 6

// anchorTarget is the file or unit a probe is about, the thing an agent's
// action names when it touches it. Ports and URLs are not action keys: a shell
// command rarely names them the way it names a path.
func anchorTarget(a anchor.Anchor) string {
	switch a.Type {
	case anchor.FileExists, anchor.DirExists, anchor.FileContains, anchor.PathAbsent,
		anchor.SymlinkTo, anchor.RefPath:
		return a.Args["path"]
	case anchor.UnitActive, anchor.UnitInactive, anchor.UnitEnabled, anchor.RefUnit:
		return a.Args["unit"]
	}
	return ""
}

// addAnchorCues gives a new fact an action cue for each file or unit its anchors
// are about, behind the anchor flag. A fact anchored to a file then comes to
// mind when the agent is about to touch that file, even if the fact text never
// spells the path. This reuses the action-stage cue match that writer cues use,
// with the same specificity limit, so a path many facts name fires for none of
// them. Restores, untrusted writes and anchors that name another machine are
// left alone.
func (s *Server) addAnchorCues(factID, enc string, m memoryIn) {
	if !anchorsOn() || factID == "" || enc == "" || m.restore != nil {
		return
	}
	if o := strings.TrimSpace(m.Origin); o != "" && (memory.Entry{Origin: o}).Untrusted() {
		return
	}
	st := s.cues()
	list, err := anchor.Decode(enc)
	if st == nil || err != nil {
		return
	}
	local := strings.ToLower(hostname())
	var cs []cues.Cue
	seen := map[string]bool{}
	for _, a := range list {
		if len(cs) == maxAnchorCues {
			break
		}
		if h := strings.ToLower(a.Args["host"]); h != "" && h != local {
			continue
		}
		t := anchorTarget(a)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		cs = append(cs, cues.Cue{Target: cues.FactTarget(factID), Kind: cues.Action,
			Text: t, Source: cues.Agent})
	}
	if len(cs) == 0 {
		return
	}
	if _, err := st.Add(cs); err != nil {
		log.Printf("cues: anchor add: %v", err)
	}
}
