package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/auth"
	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/history"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/modelcheck"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/settings"
	gsync "github.com/JeremiahM37/grimoire/go/internal/sync"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// The memory oracle. A seeded sequence of writes, forgets, dispute
// resolutions, reindexes, restarts and as_of probes is applied to the real
// server and to the reference model in internal/modelcheck. After every step
// the runner compares the note file, the default and private recall, and
// checks the conservation invariants on the store alone. The first divergence
// is shrunk to a minimal sequence and printed as a regression test.
//
// Time is virtual: each step that writes advances vault.Now by one minute, so
// stamps and as_of instants are exact. Tests that set vault.Now must not run
// in parallel, and this one does not.

const oracleNote = "memory/ops.md"

// oracleOneSubject restricts the universe to one subject, so a divergence on
// the cross-subject rule does not stop a sequence before later rules are
// reached. It is set by GRIMOIRE_ORACLE_ONE_SUBJECT=1 for measurement.
var oracleOneSubject = os.Getenv("GRIMOIRE_ORACLE_ONE_SUBJECT") == "1"

// oracleSequences and oracleLength are the default budget. A larger run is
// GRIMOIRE_ORACLE_SEQS=N, which keeps the default test time bounded.
const (
	oracleSequences = 120
	oracleLength    = 30
)

type oracle struct {
	s       *Server
	h       http.Handler
	closeDB func()
	root    string
	m       *modelcheck.Model
	real    map[int]string // model fact ordinal -> real entry id
	model   map[string]int // real entry id -> model fact ordinal
	base    time.Time
}

// openOracleServer is testServer over a caller-owned directory, so a restart
// can reopen the same vault. It returns the handler and a closer for the DB.
func openOracleServer(root string) (*Server, http.Handler, func(), error) {
	v, err := vault.New(root)
	if err != nil {
		return nil, nil, nil, err
	}
	gdir := root + "/.grimoire"
	database, err := db.Open(gdir + "/index.db")
	if err != nil {
		return nil, nil, nil, err
	}
	vaultSecrets := secrets.New(gdir)
	st := settings.New(gdir)
	ix := index.New(database, v, embed.Hash{})
	crdt := crdtstore.New(gdir)
	s := &Server{
		Index: ix, Vault: v, Settings: st, History: history.New(gdir),
		Secrets: vaultSecrets, Broker: secrets.NewBroker(vaultSecrets, database),
		CRDT: crdt, AI: ai.New(st, vaultSecrets.Get), Auth: auth.New(database),
		Sync: gsync.New(ix, v, crdt), DailyDir: "journal", InboxDir: "inbox",
	}
	ix.Spaces = s
	return s, s.Routes(), func() { database.Close() }, nil
}

func (o *oracle) open() error {
	s, h, closeDB, err := openOracleServer(o.root)
	if err != nil {
		return err
	}
	o.s, o.h, o.closeDB = s, h, closeDB
	return nil
}

func (o *oracle) call(method, path, agent string, body any) (int, []byte) {
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if agent != "" {
		req.Header.Set("X-Grimoire-Agent", agent)
	}
	w := httptest.NewRecorder()
	o.h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// stamp maps a model step to the instant the store is told it is.
func (o *oracle) stamp(step int) time.Time {
	return o.base.Add(time.Duration(step) * time.Minute)
}

// runSequence applies ops to a fresh store and the model, returning the first
// divergence as text, or "" when they agree on every step.
func runSequence(ops []modelcheck.Op) (msg string) {
	root, err := os.MkdirTemp("", "oracle-")
	if err != nil {
		return "tempdir: " + err.Error()
	}
	defer os.RemoveAll(root)
	o := &oracle{root: root, m: &modelcheck.Model{}, real: map[int]string{},
		model: map[string]int{}, base: time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local)}
	prev := vault.Now
	vault.Now = func() time.Time { return o.stamp(o.m.Clock) }
	defer func() { vault.Now = prev }()
	if err := o.open(); err != nil {
		return "open: " + err.Error()
	}
	defer func() { o.closeDB() }()

	for i, op := range ops {
		if oracleOneSubject {
			// Measurement only: every op names the first subject, so the
			// cross-subject rule (F1) cannot fire and cannot hide the rest.
			op.Slot = 0
		}
		if d := o.apply(op); d != "" {
			return fmt.Sprintf("step %d (%s): %s", i, op.GoLiteral(), d)
		}
		if d := o.check(); d != "" {
			return fmt.Sprintf("step %d (%s): %s", i, op.GoLiteral(), d)
		}
	}
	return ""
}

// target resolves an op's ledger ordinal, or -1 when nothing exists yet.
func (o *oracle) target(n int) int {
	if len(o.m.Facts) == 0 {
		return -1
	}
	return n % len(o.m.Facts)
}

func (o *oracle) apply(op modelcheck.Op) string {
	w := modelcheck.Writers[op.Writer]
	switch op.Kind {
	case modelcheck.KRemember:
		return o.remember(op, w)
	case modelcheck.KForget, modelcheck.KHardForget:
		t := o.target(op.Target)
		if t < 0 {
			return ""
		}
		hard := op.Kind == modelcheck.KHardForget
		live := o.m.Facts[t].State == modelcheck.Live
		var ok bool
		if hard {
			ok = o.m.HardForget(op.Writer, t)
		} else {
			ok = o.m.Forget(op.Writer, t)
		}
		agent := ""
		if w.Auth != modelcheck.Human {
			agent = w.Name
		}
		// A pulled writer declares its connector origin, as remember does, so the
		// store can place it on the lattice's lowest rung.
		body0 := map[string]any{"path": oracleNote, "id": o.real[t], "hard": hard, "agent": agent}
		if w.Origin != "" {
			body0["origin"] = w.Origin
		}
		code, body := o.call("POST", "/api/memory/forget", "", body0)
		if got := code == http.StatusOK; got != ok {
			tag := "[forget-refused]"
			switch {
			case ok:
			case live && !ok:
				tag = "[lattice-forget]"
			default:
				tag = "[dead-forget]"
			}
			return fmt.Sprintf("%s forget by %s of fact %d (hard=%v): model accepted=%v, store answered %d: %s",
				tag, w.Name, t, hard, ok, code, body)
		}
	case modelcheck.KResolve:
		t := o.target(op.Target)
		if t < 0 {
			return ""
		}
		disputed := o.m.Disputed(t)
		var chal int = -1
		if len(disputed) > 0 {
			chal = disputed[op.Challenger%len(disputed)].ID
		}
		resolution := "keep"
		wire := "keep"
		if op.Accept {
			resolution, wire = "accept", "accept_challenger"
		}
		ok := o.m.Resolve(op.Writer, t, resolution, chal)
		body := map[string]any{"id": o.real[t], "path": oracleNote, "resolution": wire}
		if op.Accept && chal >= 0 {
			body["challenger"] = o.real[chal]
		}
		agent := ""
		if w.Auth != modelcheck.Human {
			agent = w.Name
		}
		code, resp := o.call("POST", "/api/memory/disputes/resolve", agent, body)
		if got := code == http.StatusOK; got != ok {
			return fmt.Sprintf("[resolve-mismatch] resolve (%s) by %s of fact %d: model accepted=%v, store answered %d: %s",
				wire, w.Name, t, ok, code, resp)
		}
	case modelcheck.KReindex:
		o.s.suppressAllTombstoned()
		if _, err := o.s.Index.Reindex(); err != nil {
			return "reindex: " + err.Error()
		}
	case modelcheck.KRestart:
		o.closeDB()
		if err := o.open(); err != nil {
			return "restart: " + err.Error()
		}
	case modelcheck.KProbe:
		return o.probeAsOf(op)
	}
	return ""
}

func (o *oracle) remember(op modelcheck.Op, w modelcheck.Writer) string {
	d := o.m.Remember(op.Slot, op.Value, op.Writer, op.Private)
	body := map[string]any{
		"text":  modelcheck.Slots[op.Slot] + " runs on port " + modelcheck.Values[op.Value],
		"topic": "ops", "agent": w.Name,
	}
	if w.Auth == modelcheck.Human {
		body["human"] = true
	}
	if w.Origin != "" {
		body["origin"] = w.Origin
	}
	if op.Private {
		body["visibility"] = "private"
	}
	code, raw := o.call("POST", "/api/memory", "", body)
	if code != http.StatusCreated {
		return fmt.Sprintf("remember by %s answered %d: %s", w.Name, code, raw)
	}
	var out struct {
		Results []struct {
			Op         string `json:"op"`
			ID         string `json:"id"`
			Target     string `json:"target"`
			Text       string `json:"text"`
			Challenges string `json:"challenges"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Results) != 1 {
		return fmt.Sprintf("remember by %s: unexpected response %s", w.Name, raw)
	}
	res := out.Results[0]
	if res.Op != d.Op {
		// An ADD where the store supersedes is a value change; it is the
		// subject-blind rule (F1) only when the superseded fact has another
		// subject.
		if d.Op == "ADD" && res.Op == "UPDATE" && d.Ch >= 0 {
			// The model records this write alongside a fact that outranks it.
			// The store decided on a same-rung contender it found first and
			// never looked at the one above it: F4, the multi-contender case.
			return fmt.Sprintf("[multi-contender] remember by %s: model records alongside fact %d, store supersedes %q", w.Name, d.Ch, res.Target)
		}
		if d.Op == "ADD" && res.Op == "UPDATE" {
			if id, ok := o.model[res.Target]; ok && o.m.Facts[id].Slot != op.Slot {
				return fmt.Sprintf("[subject-slot] remember by %s of %q: model says ADD (different subject), store supersedes %q",
					w.Name, res.Text, res.Target)
			}
		}
		return fmt.Sprintf("[op-mismatch] remember by %s: model says %s, store says %s (%s)", w.Name, d.Op, res.Op, raw)
	}
	switch d.Op {
	case "NOOP":
		if res.Target != o.real[d.Target] {
			return fmt.Sprintf("[harness] NOOP names %s, model names fact %d", res.Target, d.Target)
		}
		return ""
	case "ADD", "UPDATE":
		o.real[d.Target] = res.ID
		o.model[res.ID] = d.Target
	}
	if d.Ch >= 0 && res.Challenges != o.real[d.Ch] {
		// The rule says a write is recorded alongside a live fact that
		// outranks it; it does not say which one, when several do. Accept any
		// live outranking fact, and adopt the store's choice for later checks.
		id, ok := o.model[res.Challenges]
		if !ok || !o.m.OutranksWriter(id, op.Writer) {
			return fmt.Sprintf("[challenge] model says fact %d is contested, store says %q, which does not outrank %s",
				d.Ch, res.Challenges, w.Name)
		}
		o.m.Facts[d.Target].Ch = id
	}
	if d.Ch < 0 && res.Challenges != "" {
		// A challenge across subjects is the same rule as a cross-subject
		// supersession: the store compares values without the subject.
		if id, ok := o.model[res.Challenges]; ok && o.m.Facts[id].Slot != op.Slot {
			return fmt.Sprintf("[subject-slot] remember by %s of %q is recorded as contesting %q (a different subject)",
				w.Name, res.Text, res.Challenges)
		}
		return fmt.Sprintf("[challenge] store records a challenge %q the model does not expect (%s)", res.Challenges, d.Op)
	}
	return ""
}

// recallIDs returns the real ids a recall returns for a slot, restricted to
// that slot's text.
func (o *oracle) recallIDs(slot int, query string) ([]string, string) {
	q := "q=" + strings.ReplaceAll(modelcheck.Slots[slot]+" runs on port", " ", "+") + query
	code, raw := o.call("GET", "/api/memory?limit=200&"+q, "", nil)
	if code != http.StatusOK {
		return nil, fmt.Sprintf("recall answered %d: %s", code, raw)
	}
	var hits []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &hits); err != nil {
		return nil, "recall: " + err.Error()
	}
	var ids []string
	for _, h := range hits {
		if strings.HasPrefix(h.Text, modelcheck.Slots[slot]+" ") {
			ids = append(ids, h.ID)
		}
	}
	return ids, ""
}

// wantIDs turns model facts into the real ids they should appear as.
func (o *oracle) wantIDs(facts []*modelcheck.Fact) []string {
	var out []string
	for _, f := range facts {
		out = append(out, o.real[f.ID])
	}
	return out
}

func oracleSameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	a, b = append([]string(nil), a...), append([]string(nil), b...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (o *oracle) probeAsOf(op modelcheck.Op) string {
	step := op.Sample % (o.m.Clock + 1)
	at := o.stamp(step).Format(time.RFC3339)
	ids, err := o.recallIDs(op.Slot, "&as_of="+at+"&include_challenges=1")
	if err != "" {
		return err
	}
	var want []*modelcheck.Fact
	for _, f := range o.m.BelievedAt(op.Slot, step) {
		if !f.Private {
			want = append(want, f)
		}
	}
	if !oracleSameSet(ids, o.wantIDs(want)) {
		return fmt.Sprintf("as_of step %d for %s: store returns %d, model believes %d",
			step, modelcheck.Slots[op.Slot], len(ids), len(want))
	}
	return ""
}

// check compares the store with the model after a step.
func (o *oracle) check() string {
	note, err := o.s.Vault.Read(oracleNote)
	body := ""
	if err == nil {
		body = note.Body
	}
	entries := memory.Parse(body)
	byID := map[string]memory.Entry{}
	for _, e := range entries {
		if _, dup := byID[e.ID]; dup {
			return "fork: entry " + e.ID + " appears twice in the note"
		}
		byID[e.ID] = e
		if _, known := o.model[e.ID]; !known {
			return "store holds an entry the model never wrote: " + e.Text
		}
	}
	for _, f := range o.m.Facts {
		id := o.real[f.ID]
		e, present := byID[id]
		if f.State == modelcheck.Forgotten {
			if present {
				return fmt.Sprintf("fact %d was forgotten but is still in the note", f.ID)
			}
			continue
		}
		if !present {
			return fmt.Sprintf("fact %d (%s) is %s in the model but missing from the note", f.ID, f.Text(), f.State)
		}
		switch f.State {
		case modelcheck.Live:
			if e.SupersededBy != "" {
				return fmt.Sprintf("[supersession] fact %d: model says live, store says %q", f.ID, e.SupersededBy)
			}
		case modelcheck.Superseded:
			if e.SupersededBy != o.real[f.By] {
				return fmt.Sprintf("[supersession] fact %d: model says superseded by fact %d, store says %q", f.ID, f.By, e.SupersededBy)
			}
		case modelcheck.Retracted:
			if e.SupersededBy != "retracted:"+f.Who {
				return fmt.Sprintf("[retraction] fact %d: model says retracted by %s, store says %q", f.ID, f.Who, e.SupersededBy)
			}
		}
		if e.Human != f.Human {
			return fmt.Sprintf("fact %d: model human=%v, store human=%v", f.ID, f.Human, e.Human)
		}
		if f.State == modelcheck.Live {
			want := ""
			if f.Ch >= 0 {
				want = o.real[f.Ch]
			}
			if e.Challenges != want {
				return fmt.Sprintf("fact %d: model challenges %q, store challenges %q", f.ID, want, e.Challenges)
			}
		}
		if (f.Private) != (e.Visibility == memory.VisPrivate) {
			return fmt.Sprintf("fact %d: model private=%v, store visibility %q", f.ID, f.Private, e.Visibility)
		}
	}
	if d := o.checkInvariants(byID); d != "" {
		return d
	}
	if d := o.checkForgottenAbsent(body); d != "" {
		return d
	}
	for slot := range modelcheck.Slots {
		ids, err := o.recallIDs(slot, "&include_challenges=1")
		if err != "" {
			return err
		}
		if want := o.wantIDs(o.m.Believed(slot, false)); !oracleSameSet(ids, want) {
			return fmt.Sprintf("default recall of %s: store %v, model %v", modelcheck.Slots[slot], ids, want)
		}
		ids, err = o.recallIDs(slot, "&include_challenges=1&include_private=1")
		if err != "" {
			return err
		}
		if want := o.wantIDs(o.m.Believed(slot, true)); !oracleSameSet(ids, want) {
			return fmt.Sprintf("private recall of %s: store %v, model %v", modelcheck.Slots[slot], ids, want)
		}
	}
	return ""
}

// checkForgottenAbsent: after a hard forget the text is nowhere a reader can
// get it: not in the note file, not in an export, and not in any recall, even
// one that asks for superseded, challenged, expired and private facts. A text
// that some other fact still carries is exempt, because that fact is the one
// that legitimately says it.
func (o *oracle) checkForgottenAbsent(body string) string {
	for _, f := range o.m.Facts {
		if f.State != modelcheck.Forgotten {
			continue
		}
		text := f.Text()
		shared := false
		for _, g := range o.m.Facts {
			if g.ID != f.ID && g.State != modelcheck.Forgotten && g.Text() == text {
				shared = true
			}
		}
		if shared {
			continue
		}
		if strings.Contains(body, text) {
			return fmt.Sprintf("[forgotten-text] fact %d was forgotten but its text is still in the note", f.ID)
		}
		if _, raw := o.call("GET", "/api/memory/export", "", nil); strings.Contains(string(raw), text) {
			return fmt.Sprintf("[forgotten-text] fact %d was forgotten but its text is in the export", f.ID)
		}
		slotQuery := strings.ReplaceAll(modelcheck.Slots[f.Slot]+" runs on port", " ", "+")
		code, raw := o.call("GET", "/api/memory?limit=200&include_superseded=1&include_challenges=1&include_expired=1&include_private=1&q="+slotQuery, "", nil)
		if code != http.StatusOK {
			return fmt.Sprintf("recall answered %d: %s", code, raw)
		}
		// Compare the id field, not the whole body: a predecessor's
		// superseded_by may name a forgotten successor, and that pointer is
		// an id, not the fact's text.
		var hits []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &hits); err != nil {
			return "recall: " + err.Error()
		}
		for _, h := range hits {
			if h.ID == o.real[f.ID] {
				return fmt.Sprintf("[forgotten-text] fact %d was forgotten but recall still returns its id", f.ID)
			}
		}
	}
	return ""
}

// checkInvariants are the conservation rules that hold on the store alone,
// whatever the model says. Each entry that ever existed is exactly one of
// live, superseded by an entry that exists, retracted with attribution, or
// forgotten with a tombstone. Chains never cycle, and a human fact is never
// superseded or retracted by a non-human writer.
func (o *oracle) checkInvariants(byID map[string]memory.Entry) string {
	forgotten := 0
	for _, f := range o.m.Facts {
		if f.State == modelcheck.Forgotten {
			forgotten++
		}
	}
	// Every tombstone line counts, whether or not this session can verify it:
	// a locked vault writes unsigned lines that Load refuses to return.
	_, rep, err := o.s.tombLog().Load(o.s.tombKey())
	if total := rep.Verified + rep.Unverified + rep.Unsigned + rep.Rejected; err == nil && total != forgotten {
		return fmt.Sprintf("[tombstone] conservation: %d hard-forgotten facts but %d tombstone lines", forgotten, total)
	}
	for id, e := range byID {
		seen := map[string]bool{id: true}
		cur := e
		for cur.SupersededBy != "" && !strings.HasPrefix(cur.SupersededBy, "retracted:") {
			next, ok := byID[cur.SupersededBy]
			if !ok {
				break // successor was hard-forgotten; the tombstone accounts for it
			}
			if seen[next.ID] {
				return "chain cycles through " + next.ID
			}
			seen[next.ID] = true
			if e.Human && !next.Human {
				return fmt.Sprintf("human fact %q superseded by a non-human fact %q", e.Text, next.Text)
			}
			cur = next
		}
		if e.Human && strings.HasPrefix(e.SupersededBy, "retracted:") && e.SupersededBy != "retracted:human" {
			return fmt.Sprintf("human fact %q retracted by %s", e.Text, e.SupersededBy)
		}
	}
	return ""
}

// oracleBudget reads the sequence count, so a larger run is one env var.
func oracleBudget(t *testing.T) (seqs, length int) {
	t.Helper()
	seqs, length = oracleSequences, oracleLength
	if v, err := strconv.Atoi(os.Getenv("GRIMOIRE_ORACLE_SEQS")); err == nil && v > 0 {
		seqs = v
	}
	return seqs, length
}

// runOracle is the entry point a regression test calls with a fixed sequence.
func runOracle(t *testing.T, ops []modelcheck.Op) {
	t.Helper()
	if msg := runSequence(ops); msg != "" {
		t.Fatalf("model and store diverge: %s", msg)
	}
}

// oracleKnownFindings are divergences the oracle has found in the product and
// that are documented in docs/lab/oracle.md. Keyed by signature (see
// signature), so a new kind of divergence still fails the run. Each one has a
// skipped regression test that pins the documented behaviour.
var oracleKnownFindings = map[string]string{
	"[subject-slot]":    "F1: the value rule compares slots without the subject (memory_oracle_findings_test.go)",
	"[supersession]":    "F4: a correction supersedes one contender and leaves another believed",
	"[multi-contender]": "F4: the store decides on the first contender it finds, not on every contender",
}

// signature names a divergence by its kind, with the numbers and quoted
// texts removed, so every seed that hits the same rule groups together.
func signature(msg string) string {
	// Messages begin "step N (op): "; the op never contains "): ".
	if strings.HasPrefix(msg, "step ") {
		if i := strings.Index(msg, "): "); i >= 0 {
			msg = msg[i+3:]
		}
	}
	if strings.HasPrefix(msg, "[") {
		if i := strings.Index(msg, "]"); i > 0 {
			return msg[:i+1]
		}
	}
	var b strings.Builder
	for _, r := range msg {
		if r >= '0' && r <= '9' {
			continue
		}
		b.WriteRune(r)
		if r == ':' {
			break
		}
	}
	return b.String()
}

type oracleFinding struct {
	sig        string
	seed, hits int
	msg        string
	regression string
}

func TestMemoryOracleModelAgreesWithStore(t *testing.T) {
	seqs, length := oracleBudget(t)
	found := map[string]*oracleFinding{}
	var order []string
	for seed := 1; seed <= seqs; seed++ {
		ops := modelcheck.Generate(int64(seed), length)
		msg := runSequence(ops)
		if msg == "" {
			continue
		}
		sig := signature(msg)
		f, ok := found[sig]
		if !ok {
			same := func(c []modelcheck.Op) bool {
				m := runSequence(c)
				return m != "" && signature(m) == sig
			}
			minimal := modelcheck.Shrink(ops, same)
			f = &oracleFinding{sig: sig, seed: seed, msg: runSequence(minimal),
				regression: modelcheck.RegressionTest(fmt.Sprintf("%d", len(found)+1), minimal)}
			found[sig] = f
			order = append(order, sig)
		}
		f.hits++
	}
	for _, sig := range order {
		f := found[sig]
		if note, known := oracleKnownFindings[sig]; known {
			t.Logf("known finding (%s): %d of %d sequences hit %q; minimal: %s", note, f.hits, seqs, sig, f.msg)
			continue
		}
		t.Errorf("new divergence %q hit %d of %d sequences (first seed %d)\nminimal: %s\n\n%s",
			sig, f.hits, seqs, f.seed, f.msg, f.regression)
	}
}
