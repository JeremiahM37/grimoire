package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/identity"
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

// anchorReport is one anchor's outcome in a write's response.
type anchorReport struct {
	Type    string            `json:"type"`
	Args    map[string]string `json:"args,omitempty"`
	Result  string            `json:"result"`
	Message string            `json:"message,omitempty"`
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
	case m.CallerRemote || !loopbackPeer(r):
		return "", []anchorReport{}, "remote caller", nil
	}
	var kept []anchor.Anchor
	reports = []anchorReport{}
	ctx := r.Context()
	for _, raw := range m.Anchors {
		a, convErr := anchor.FromMap(raw)
		if convErr != nil {
			reports = append(reports, anchorReport{Result: string(anchor.Error), Message: convErr.Error()})
			continue
		}
		rep := anchorReport{Type: a.Type, Args: a.Args}
		switch {
		case !anchor.Storable(a.Type):
			rep.Result, rep.Message = string(anchor.Error), "not a stored probe type"
		default:
			res := anchor.Run(ctx, a)
			rep.Result, rep.Message = string(res.Status), res.Message
			if res.Status == anchor.Pass || res.Status == anchor.Skipped {
				kept = append(kept, a)
			}
		}
		reports = append(reports, rep)
	}
	return anchor.Encode(kept), reports, "", nil
}

// anchorVerdict is what the probes said about one fact, as recall needs it.
type anchorVerdict struct {
	at      time.Time
	failed  bool   // some anchor did not hold
	errored bool   // the first problem was a probe that could not answer
	kind    string // type of the first anchor that failed or could not answer
	passed  int    // anchors that held
}

// anchorCache holds verdicts for anchorCacheTTL, keyed by fact and its anchors.
type anchorCache struct {
	mu sync.Mutex
	m  map[string]anchorVerdict
}

// verdictFor is the cached probe verdict for a fact's stored anchors.
func (s *Server) verdictFor(e memory.Entry) anchorVerdict {
	key := e.ID + "\x00" + e.Anchors
	now := time.Now()
	s.anchors.mu.Lock()
	if v, ok := s.anchors.m[key]; ok && now.Sub(v.at) < anchorCacheTTL {
		s.anchors.mu.Unlock()
		return v
	}
	s.anchors.mu.Unlock()

	v := probeStored(context.Background(), e.Anchors)
	v.at = now

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
func probeStored(ctx context.Context, enc string) anchorVerdict {
	list, err := anchor.Decode(enc)
	if err != nil {
		return anchorVerdict{failed: true, kind: ""}
	}
	var v anchorVerdict
	for _, a := range list {
		res := anchor.Run(ctx, a)
		switch res.Status {
		case anchor.Pass:
			v.passed++
		case anchor.Fail, anchor.Error:
			if !v.failed {
				v.failed, v.kind = true, a.Type
				v.errored = res.Status == anchor.Error
			}
		}
	}
	return v
}

// applyAnchors gates a recall assessment on the fact's stored anchors. A failing
// anchor sends the fact to verify, naming the anchor's type; a fact whose
// anchors all hold is use. Anchors that could only be skipped leave it alone.
func (s *Server) applyAnchors(a *memory.Assessment, e memory.Entry) {
	if e.Anchors == "" {
		return
	}
	v := s.verdictFor(e)
	switch {
	case v.failed && v.kind == "":
		a.Action, a.Reason = memory.ActionVerify, "stored anchors are unreadable"
	case v.failed && v.errored:
		a.Action, a.Reason = memory.ActionVerify, "anchor "+v.kind+" could not be checked"
	case v.failed:
		a.Action, a.Reason = memory.ActionVerify, "anchor "+v.kind+" does not hold"
	case v.passed > 0:
		a.Action, a.Reason = memory.ActionUse, "stored anchors hold"
	}
}

// anchorTally is the admin report over every live fact's anchors.
type anchorTally struct {
	Facts          int             `json:"facts"`
	Events         int             `json:"events"`
	Preferences    int             `json:"preferences"`
	Claims         int             `json:"claims"`
	AnchoredClaims int             `json:"anchored_claims"`
	Anchors        int             `json:"anchors"`
	Pass           int             `json:"pass"`
	Fail           int             `json:"fail"`
	Error          int             `json:"error"`
	Skipped        int             `json:"skipped"`
	Failing        []failingAnchor `json:"failing"`
}

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
	t := anchorTally{Failing: []failingAnchor{}}
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
			switch {
			case e.Event:
				t.Events++
			case isPreferenceKind(e):
				t.Preferences++
			default:
				t.Claims++
				if e.Anchors != "" {
					t.AnchoredClaims++
				}
			}
			if e.Anchors == "" {
				continue
			}
			list, err := anchor.Decode(e.Anchors)
			if err != nil {
				t.Anchors++
				t.Fail++
				t.Failing = append(t.Failing, failingAnchor{ID: e.ID, Path: rel,
					Result: string(anchor.Fail), Message: "stored anchors are unreadable"})
				continue
			}
			for _, a := range list {
				res := anchor.Run(r.Context(), a)
				t.Anchors++
				switch res.Status {
				case anchor.Pass:
					t.Pass++
				case anchor.Skipped:
					t.Skipped++
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
		}
	}
	writeJSON(w, http.StatusOK, t)
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
