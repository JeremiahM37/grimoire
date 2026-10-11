package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Anchors over the API: who may attach them, what a write stores, what recall
// does with them behind the flag, and the two admin reports.

// postFrom is do() with a chosen peer address, so a test can be a loopback
// caller (the local agent) or a remote one.
func postFrom(t *testing.T, h http.Handler, peer, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = peer
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

const anchorLoopback = "127.0.0.1:50000"
const anchorRemote = "192.0.2.10:50000"

// litFile is a temp file holding one literal, for a passing file_contains.
func litFile(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "lan-config.txt")
	if err := os.WriteFile(p, []byte("ROUTER_IP="+text+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func rememberFrom(t *testing.T, h http.Handler, peer string, body map[string]any) map[string]any {
	t.Helper()
	w := postFrom(t, h, peer, "POST", "/api/memory", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", w.Code, w.Body)
	}
	var out map[string]any
	decode(t, w, &out)
	return out
}

func TestLoopbackWriteStoresAPassingAnchor(t *testing.T) {
	_, h := testServer(t)
	lan := litFile(t, "192.168.0.1")
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the router is 192.168.0.1", "anchors": []map[string]any{
			{"type": "file_contains", "path": lan, "text": "ROUTER_IP=192.168.0.1"}}})
	reps := out["anchors"].([]any)
	if len(reps) != 1 || reps[0].(map[string]any)["result"] != "pass" || out["anchors_stored"] != float64(1) {
		t.Fatalf("response = %v", out)
	}
	if body := noteBody(t, h, "memory/net.md"); !strings.Contains(body, "anc=file_contains;path=") {
		t.Fatalf("passing anchor not stored:\n%s", body)
	}
}

func TestAFailingAnchorDoesNotFailTheWriteButIsNotStored(t *testing.T) {
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the router is 10.9.9.9", "anchors": []map[string]any{
			{"type": "file_contains", "path": litFile(t, "192.168.0.1"), "text": "10.9.9.9"}}})
	if out["id"] == nil || out["anchors_stored"] != float64(0) {
		t.Fatalf("a failing anchor changed the write: %v", out)
	}
	rep := out["anchors"].([]any)[0].(map[string]any)
	if rep["result"] != "fail" {
		t.Fatalf("result = %v, want fail", rep["result"])
	}
	if body := noteBody(t, h, "memory/net.md"); strings.Contains(body, "anc=") {
		t.Fatalf("a failing anchor was stored:\n%s", body)
	}
}

func TestRemoteCallersGetNoProbesAndTheResponseSaysWhy(t *testing.T) {
	_, h := testServer(t)
	lan := litFile(t, "192.168.0.1")
	anchors := []map[string]any{{"type": "file_contains", "path": lan, "text": "192.168.0.1"}}

	out := rememberFrom(t, h, anchorRemote, map[string]any{"topic": "net", "infer": false,
		"text": "from a remote peer", "anchors": anchors})
	if list, _ := out["anchors"].([]any); out["anchors_ignored"] != "remote caller" || len(list) != 0 {
		t.Fatalf("remote write not refused as remote: %v", out)
	}
	// The MCP bridge marks an OAuth or HTTP call as remote even though its peer
	// is the loopback proxy. That marking must count.
	out = rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "from a proxied peer", "anchors": anchors, "caller_remote": true})
	if out["anchors_ignored"] != "remote caller" {
		t.Fatalf("caller_remote on loopback was not honoured: %v", out)
	}
	if body := noteBody(t, h, "memory/net.md"); strings.Contains(body, "anc=") {
		t.Fatalf("a remote caller stored an anchor:\n%s", body)
	}
}

func TestEventsCarryNoAnchors(t *testing.T) {
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "infer": false,
		"text": "the cutover happened on 2026-10-11", "event": true,
		"anchors": []map[string]any{{"type": "port_listening", "port": 9111}}})
	if out["anchors_ignored"] != "event" {
		t.Fatalf("event kept its anchors: %v", out)
	}
	body := noteBody(t, h, "memory/ops.md")
	if !strings.Contains(body, "evt=1") || strings.Contains(body, "anc=") {
		t.Fatalf("event trailer wrong:\n%s", body)
	}
}

func TestAWriteWithTooManyAnchorsIsRefused(t *testing.T) {
	_, h := testServer(t)
	var many []map[string]any
	for i := 0; i < maxAnchorsPerWrite+1; i++ {
		many = append(many, map[string]any{"type": "port_closed", "port": 65000})
	}
	w := postFrom(t, h, anchorLoopback, "POST", "/api/memory",
		map[string]any{"topic": "net", "infer": false, "text": "x", "anchors": many})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestReferenceOnlyTypesAreNotStored(t *testing.T) {
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "a note about a path", "anchors": []map[string]any{{"type": "ref_path", "path": "/x"}}})
	rep := out["anchors"].([]any)[0].(map[string]any)
	if rep["result"] != "error" || out["anchors_stored"] != float64(0) {
		t.Fatalf("a reference-only type was accepted as a probe: %v", out)
	}
	if body := noteBody(t, h, "memory/net.md"); strings.Contains(body, "anc=") {
		t.Fatalf("reference-only anchor stored:\n%s", body)
	}
}

// recallJSON is the raw body of a recall, so a test can compare bytes.
func recallJSON(t *testing.T, h http.Handler, q string) string {
	t.Helper()
	w := do(t, h, "GET", "/api/memory?q="+q, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("recall = %d: %s", w.Code, w.Body)
	}
	return w.Body.String()
}

// freshnessAction returns the recall action for the one fact in a recall.
func freshnessAction(t *testing.T, h http.Handler, q string) (action, reason string) {
	t.Helper()
	var facts []map[string]any
	if err := json.Unmarshal([]byte(recallJSON(t, h, q)), &facts); err != nil || len(facts) == 0 {
		t.Fatalf("recall %q: %v", q, err)
	}
	f := facts[0]["freshness"].(map[string]any)
	return f["action"].(string), f["reason"].(string)
}

func TestRecallGatesOnAnchorsOnlyBehindTheFlag(t *testing.T) {
	s, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "kestrel gateway is on the lan", "fresh": "stable"})
	// A stored anchor whose literal is no longer in the file: the claim has
	// drifted, and only the flag makes recall say so.
	storeAnchor(t, s, "memory/net.md", "kestrel gateway is on the lan",
		"file_contains;path="+litFile(t, "nothing here")+";text=missing-literal")

	if action, _ := freshnessAction(t, h, "kestrel+gateway+lan"); action != "use" {
		t.Fatalf("flag off: action = %q, want use", action)
	}
	restore := exp.Set(anchorFlag)
	defer restore()
	if action, reason := freshnessAction(t, h, "kestrel+gateway+lan"); action != "verify" || !strings.Contains(reason, "file_contains") {
		t.Fatalf("flag on: action = %q reason = %q, want verify naming file_contains", action, reason)
	}
}

func TestRecallWithAllAnchorsPassingIsUse(t *testing.T) {
	_, h := testServer(t)
	restore := exp.Set(anchorFlag)
	defer restore()
	lan := litFile(t, "192.168.0.1")
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "router address is recorded", "fresh": "volatile",
		"anchors": []map[string]any{{"type": "file_contains", "path": lan, "text": "192.168.0.1"}}})
	if action, reason := freshnessAction(t, h, "router+address+recorded"); action != "use" || reason != "stored anchors hold" {
		t.Fatalf("action = %q reason = %q, want use", action, reason)
	}
}

// storeAnchor puts an anchor on an existing bullet through the store's own
// edit path, which is how a claim whose world has since changed comes to carry
// a probe that no longer holds: writes only store anchors that pass when made.
func storeAnchor(t *testing.T, s *Server, note, text, enc string) {
	t.Helper()
	existing, err := s.Vault.Read(note)
	if err != nil {
		t.Fatal(err)
	}
	body := existing.Body
	for _, e := range memory.Parse(body) {
		if e.Text == text {
			if err := s.mutateEntry(note, e.ID, func(x *memory.Entry) { x.Anchors = enc }); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatalf("no bullet with %q in %s:\n%s", text, note, body)
}

// Recall output must not depend on anchors when the flag is off. The same fact
// written with and without anchors, at the same instant, must recall to the
// same bytes.
func TestRecallIsByteIdenticalWithTheFlagOffWhetherOrNotAnchored(t *testing.T) {
	when := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	const text = "kestrel gateway is on the lan"
	recallFor := func(anchored bool) string {
		s, h := testServer(t)
		at(t, when, func() {
			rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
				"text": text, "fresh": "7d"})
		})
		if anchored {
			storeAnchor(t, s, "memory/net.md", text,
				"file_contains;path="+litFile(t, "nothing")+";text=missing-literal")
		}
		return recallJSON(t, h, "kestrel+gateway+lan")
	}
	plain := recallFor(false)
	anchored := recallFor(true)
	if plain != anchored {
		t.Fatalf("flag off: recall changed with anchors present\nplain:    %s\nanchored: %s", plain, anchored)
	}
	if !strings.Contains(plain, `"action":"use"`) {
		t.Fatalf("expected a plain use verdict, got %s", plain)
	}
}

func TestAdminAnchorReportCountsFactsAndNamesFailuresWithoutText(t *testing.T) {
	s, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "a passing claim about the lan", "anchors": []map[string]any{
			{"type": "file_contains", "path": litFile(t, "lan-ok"), "text": "lan-ok"}}})
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "a plain claim with no anchor"})
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "infer": false,
		"text": "the deploy happened", "event": true})
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "prefs", "infer": false,
		"text": "the user prefers tabs", "kind": "preference"})
	storeAnchor(t, s, "memory/net.md", "a plain claim with no anchor",
		"file_contains;path="+litFile(t, "gone")+";text=zzz")

	w := do(t, h, "GET", "/api/memory/anchors", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("anchors = %d: %s", w.Code, w.Body)
	}
	raw := w.Body.String()
	for _, secret := range []string{"a passing claim", "a plain claim", "the deploy happened", "tabs"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("report leaked fact text %q: %s", secret, raw)
		}
	}
	var out struct {
		Facts          int              `json:"facts"`
		Events         int              `json:"events"`
		Preferences    int              `json:"preferences"`
		Claims         int              `json:"claims"`
		AnchoredClaims int              `json:"anchored_claims"`
		Anchors        int              `json:"anchors"`
		Pass           int              `json:"pass"`
		Fail           int              `json:"fail"`
		Failing        []map[string]any `json:"failing"`
	}
	decode(t, w, &out)
	if out.Facts != 4 || out.Events != 1 || out.Preferences != 1 || out.Claims != 2 || out.AnchoredClaims != 2 {
		t.Fatalf("counts = %+v in %s", out, raw)
	}
	if out.Anchors != 2 || out.Pass != 1 || out.Fail != 1 || len(out.Failing) != 1 {
		t.Fatalf("anchor counts = %+v in %s", out, raw)
	}
	if out.Failing[0]["anchor"] != "file_contains" || out.Failing[0]["path"] != "memory/net.md" {
		t.Fatalf("failing = %v", out.Failing[0])
	}
}

func TestAnchorReportAndRefScanAreAdminOnly(t *testing.T) {
	s, h := testServer(t)
	if _, err := s.Auth.Create("owner", "Owner", "a long enough password", "admin"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/memory/anchors", "/api/memory/refs"} {
		if w := do(t, h, "GET", path, nil); w.Code == http.StatusOK {
			t.Errorf("%s answered an anonymous caller once accounts exist", path)
		}
	}
}

func TestRefScanReportsDeadReferencesByNoteAndTotals(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.WriteFile(filepath.Join(home, "present.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, h := testServer(t)
	if _, err := s.Vault.Write("ops/runbook.md", "See ~/present.md and ~/gone-"+strings.Repeat("z", 3)+".md.\n", nil); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "GET", "/api/memory/refs", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("refs = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Notes     int `json:"notes"`
		Refs      int `json:"refs"`
		DeadTotal int `json:"dead_total"`
		Dead      []struct {
			Path, Ref string
		} `json:"dead"`
	}
	decode(t, w, &out)
	if out.Notes != 1 || out.Refs != 2 || out.DeadTotal != 1 {
		t.Fatalf("totals = %+v in %s", out, w.Body)
	}
	if len(out.Dead) != 1 || out.Dead[0].Path != "ops/runbook.md" || out.Dead[0].Ref != filepath.Join(home, "gone-zzz.md") {
		t.Fatalf("dead = %+v", out.Dead)
	}
}
