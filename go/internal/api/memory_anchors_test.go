package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
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

func TestRecallRunsAFactsLocalChecksUnlessSwitchedOff(t *testing.T) {
	t.Setenv("GRIMOIRE_ANCHOR_RECALL", "0")
	s, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "kestrel gateway is on the lan", "fresh": "stable"})
	// A stored anchor whose literal is no longer in the file: the claim has
	// drifted. With the switch off recall does not look.
	storeAnchor(t, s, "memory/net.md", "kestrel gateway is on the lan",
		"file_contains;path="+litFile(t, "nothing here")+";text=missing-literal")

	if action, _ := freshnessAction(t, h, "kestrel+gateway+lan"); action != "use" {
		t.Fatalf("switched off: action = %q, want use", action)
	}
	t.Setenv("GRIMOIRE_ANCHOR_RECALL", "")
	if action, reason := freshnessAction(t, h, "kestrel+gateway+lan"); action != "verify" || !strings.Contains(reason, "file_contains") {
		t.Fatalf("default: action = %q reason = %q, want verify naming file_contains", action, reason)
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
func TestRecallIsByteIdenticalWhenSwitchedOffWhetherOrNotAnchored(t *testing.T) {
	t.Setenv("GRIMOIRE_ANCHOR_RECALL", "0")
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
		t.Fatalf("switched off: recall changed with anchors present\nplain:    %s\nanchored: %s", plain, anchored)
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

// fakeHost stands in for this machine's name for the length of one test.
func fakeHost(t *testing.T, name string) {
	t.Helper()
	t.Cleanup(anchor.SetLocalHost(name))
}

func TestLocalClaimIsStampedWithThisMachine(t *testing.T) {
	fakeHost(t, "aiserver")
	_, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway is up"})
	if body := noteBody(t, h, "memory/net.md"); !strings.Contains(body, "host=aiserver") {
		t.Fatalf("local claim not stamped with this machine:\n%s", body)
	}
}

func TestHostIsNotStampedOnEventsPreferencesOrRemoteWrites(t *testing.T) {
	fakeHost(t, "aiserver")
	_, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "infer": false,
		"text": "the cutover happened", "event": true})
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "prefs", "infer": false,
		"text": "the user prefers tabs", "kind": "preference"})
	rememberFrom(t, h, anchorRemote, map[string]any{"topic": "remote", "infer": false,
		"text": "from a remote peer"})
	for _, note := range []string{"memory/ops.md", "memory/prefs.md", "memory/remote.md"} {
		if body := noteBody(t, h, note); strings.Contains(body, "host=") {
			t.Fatalf("%s was stamped with a host:\n%s", note, body)
		}
	}
}

func TestExplicitHostIsValidatedAndStoredLowercased(t *testing.T) {
	fakeHost(t, "aiserver")
	_, h := testServer(t)
	rememberFrom(t, h, anchorRemote, map[string]any{"topic": "net", "infer": false,
		"text": "the proxy runs on the other box", "host": "MediaServer"})
	if body := noteBody(t, h, "memory/net.md"); !strings.Contains(body, "host=mediaserver") {
		t.Fatalf("explicit host not stored lowercased:\n%s", body)
	}
	for _, bad := range []string{"bad host;rm", "-leading", "a.b/c", strings.Repeat("x", 64), "$(id)"} {
		w := postFrom(t, h, anchorLoopback, "POST", "/api/memory",
			map[string]any{"topic": "net", "infer": false, "text": "x", "host": bad})
		if w.Code != http.StatusBadRequest {
			t.Errorf("host %q: status = %d, want 400", bad, w.Code)
		}
	}
}

func TestRecallCarriesHostOnlyWhenOneWasRecorded(t *testing.T) {
	_, h := testServer(t)
	rememberFrom(t, h, anchorRemote, map[string]any{"topic": "net", "infer": false,
		"text": "kestrel gateway has no machine", "fresh": "stable"})
	rememberFrom(t, h, anchorRemote, map[string]any{"topic": "net", "infer": false,
		"text": "kestrel gateway sits on mediaserver", "fresh": "stable", "host": "mediaserver"})
	var facts []map[string]any
	if err := json.Unmarshal([]byte(recallJSON(t, h, "kestrel")), &facts); err != nil || len(facts) != 2 {
		t.Fatalf("recall: %v in %d facts", err, len(facts))
	}
	for _, f := range facts {
		_, has := f["host"]
		text := f["text"].(string)
		switch {
		case strings.Contains(text, "no machine") && has:
			t.Fatalf("a fact with no host carries a host key: %v", f)
		case strings.Contains(text, "mediaserver") && (!has || f["host"] != "mediaserver"):
			t.Fatalf("host not recalled: %v", f)
		}
	}
}

// fakeSSHOnPath puts an ssh that exits with code on PATH for one test. The
// anchor package runs "ssh" by name, so this is the only ssh it can reach, and
// it never dials anything. It also points the host allowlist at a host that
// is never dialled.
func fakeSSHOnPath(t *testing.T, code int, out string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s' '" + out + "'\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	anchor.SetHosts(map[string]string{"mediaserver": "root@example.invalid"})
	t.Cleanup(func() { anchor.SetHosts(nil) })
}

func TestAClaimAboutAnotherHostIsProbedThere(t *testing.T) {
	fakeSSHOnPath(t, 0, "active\n")
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the proxy is up", "host": "mediaserver",
		"anchors": []map[string]any{{"type": "unit_active", "unit": "caddy.service"}}})
	rep := out["anchors"].([]any)[0].(map[string]any)
	if rep["result"] != "pass" || out["anchors_stored"] != float64(1) {
		t.Fatalf("remote anchor not run on its host: %v", out)
	}
	body := noteBody(t, h, "memory/net.md")
	if !strings.Contains(body, "host=mediaserver") || !strings.Contains(body, "anc=unit_active;host=mediaserver;unit=caddy.service") {
		t.Fatalf("anchor did not inherit the fact's host:\n%s", body)
	}
}

func TestAnAnchorForAHostOutsideTheAllowlistIsSkippedAndSaysSo(t *testing.T) {
	fakeSSHOnPath(t, 0, "active\n")
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the box runs nginx", "host": "elsewhere",
		"anchors": []map[string]any{{"type": "unit_active", "unit": "nginx.service"}}})
	rep := out["anchors"].([]any)[0].(map[string]any)
	if rep["result"] != "skipped" || rep["message"] != anchor.SkipHostNotAllowed {
		t.Fatalf("anchor for an unlisted host: %v", rep)
	}
}

func TestRetrofitFactsInheritTheirHostInTheReport(t *testing.T) {
	fakeSSHOnPath(t, 1, "")
	s, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway config lives on the proxy host", "host": "mediaserver"})
	storeAnchor(t, s, "memory/net.md", "the gateway config lives on the proxy host",
		"file_exists;path=/srv/gateway.conf")
	w := do(t, h, "GET", "/api/memory/anchors", nil)
	var out struct {
		Fail    int                       `json:"fail"`
		ByHost  map[string]map[string]int `json:"by_host"`
		Failing []map[string]any          `json:"failing"`
	}
	decode(t, w, &out)
	if out.Fail != 1 || len(out.Failing) != 1 || out.ByHost["mediaserver"]["claims"] != 1 {
		t.Fatalf("report did not run the inherited anchor on mediaserver: %s", w.Body)
	}
}

// autoHome makes a file under a temporary HOME so a path in a claim's text is
// a reference the store can check. It returns the home directory.
func autoHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "notes", "cfg.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestAClaimWithNoAnchorsGetsWeakAnchorsForThePathsItNames(t *testing.T) {
	autoHome(t)
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway config is at ~/notes/cfg.txt"})
	auto, _ := out["anchors_auto"].([]any)
	if len(auto) != 1 || auto[0].(map[string]any)["type"] != "ref_path" {
		t.Fatalf("anchors_auto = %v in %v", auto, out)
	}
	body := noteBody(t, h, "memory/net.md")
	if !strings.Contains(body, "anc=ref_path;auto=1;path=") {
		t.Fatalf("auto anchor not stored:\n%s", body)
	}
}

func TestAutoAnchorsCanBeSwitchedOffAndStoreNothingWhenNoneHold(t *testing.T) {
	autoHome(t)
	_, h := testServer(t)
	t.Setenv("GRIMOIRE_ANCHOR_AUTO", "0")
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway config is at ~/notes/cfg.txt"})
	if _, present := out["anchors_auto"]; present {
		t.Fatalf("auto anchors ran with the switch off: %v", out)
	}
	if body := noteBody(t, h, "memory/net.md"); strings.Contains(body, "anc=") {
		t.Fatalf("auto anchor stored with the switch off:\n%s", body)
	}

	t.Setenv("GRIMOIRE_ANCHOR_AUTO", "")
	out = rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway config is at ~/notes/missing.txt"})
	if _, present := out["anchors_auto"]; present {
		t.Fatalf("a path that does not exist was kept: %v", out)
	}
	if body := noteBody(t, h, "memory/net.md"); strings.Contains(body, "missing.txt") && strings.Contains(body, "anc=") {
		t.Fatalf("a failing auto anchor was stored:\n%s", body)
	}
}

func TestAutoAnchorsDoNotApplyToRemoteEventsOrPreferences(t *testing.T) {
	autoHome(t)
	_, h := testServer(t)
	rememberFrom(t, h, anchorRemote, map[string]any{"topic": "net", "infer": false,
		"text": "remote note about ~/notes/cfg.txt"})
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "infer": false, "event": true,
		"text": "cutover of ~/notes/cfg.txt"})
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "prefs", "infer": false, "kind": "preference",
		"text": "the user keeps ~/notes/cfg.txt"})
	for _, note := range []string{"memory/net.md", "memory/ops.md", "memory/prefs.md"} {
		if body := noteBody(t, h, note); strings.Contains(body, "anc=") {
			t.Fatalf("%s carries anchors it should not:\n%s", note, body)
		}
	}
}

func TestWriteAdviceNamesWhatIsUncheckableOrPastTense(t *testing.T) {
	_, h := testServer(t)
	plain := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway is on the lan"})
	if adv, _ := plain["advice"].([]any); len(adv) != 1 || adv[0] != adviceNoAnchors {
		t.Fatalf("no anchor advice: %v", plain["advice"])
	}
	past := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "infer": false,
		"text": "Fixed the proxy on 2026-10-11", "event": false})
	if adv, _ := past["advice"].([]any); len(adv) < 1 || !containsAdvice(adv, adviceEvent) {
		t.Fatalf("no past-tense advice: %v", past["advice"])
	}
	event := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "infer": false,
		"text": "Fixed the proxy on 2026-10-11", "event": true})
	if _, present := event["advice"]; present {
		t.Fatalf("an event got advice: %v", event["advice"])
	}
	checked := litFile(t, "192.168.0.1")
	ok := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the router address is recorded", "anchors": []map[string]any{
			{"type": "file_contains", "path": checked, "text": "192.168.0.1"}}})
	if _, present := ok["advice"]; present {
		t.Fatalf("an anchored claim got advice: %v", ok["advice"])
	}
}

func containsAdvice(list []any, want string) bool {
	for _, a := range list {
		if a == want {
			return true
		}
	}
	return false
}

func TestReportSeparatesAutoAnchoredClaimsAndCountsSkipReasons(t *testing.T) {
	autoHome(t)
	fakeHost(t, "aiserver")
	fakeSSHOnPath(t, 0, "active\n")
	_, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway config is at ~/notes/cfg.txt"})
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the box runs nginx", "host": "elsewhere",
		"anchors": []map[string]any{{"type": "unit_active", "unit": "nginx.service"}}})
	w := do(t, h, "GET", "/api/memory/anchors", nil)
	var out struct {
		Claims             int                       `json:"claims"`
		AnchoredClaims     int                       `json:"anchored_claims"`
		AutoAnchoredClaims int                       `json:"auto_anchored_claims"`
		Skipped            int                       `json:"skipped"`
		SkippedReasons     map[string]int            `json:"skipped_reasons"`
		ByHost             map[string]map[string]int `json:"by_host"`
	}
	decode(t, w, &out)
	if out.Claims != 2 || out.AutoAnchoredClaims != 1 || out.AnchoredClaims != 1 {
		t.Fatalf("counts = %+v in %s", out, w.Body)
	}
	if out.Skipped != 1 || out.SkippedReasons["host_not_allowlisted"] != 1 || out.SkippedReasons["remote"] != 0 {
		t.Fatalf("skip reasons = %+v", out.SkippedReasons)
	}
	if out.ByHost["aiserver"]["claims"] != 1 || out.ByHost["elsewhere"]["anchored_claims"] != 1 {
		t.Fatalf("by_host = %v", out.ByHost)
	}
}

// annotateBody posts an annotate request and returns the response.
func annotateFrom(t *testing.T, h http.Handler, peer string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return postFrom(t, h, peer, "POST", "/api/memory/annotate", body)
}

// bulletLine returns the stored line for the fact with this id.
func bulletLine(t *testing.T, h http.Handler, note, id string) string {
	t.Helper()
	for _, line := range strings.Split(noteBody(t, h, note), "\n") {
		if strings.Contains(line, "id="+id+" ") || strings.Contains(line, "id="+id+"-->") {
			return line
		}
	}
	t.Fatalf("no line for %s in %s", id, note)
	return ""
}

func TestAnnotateChangesOnlyTheTrailerOfTheFactsLine(t *testing.T) {
	fakeHost(t, "aiserver")
	fakeSSHOnPath(t, 0, "")
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway is on the lan", "fresh": "stable"})
	id := out["id"].(string)
	before := bulletLine(t, h, "memory/net.md", id)
	lan := litFile(t, "192.168.0.1")

	w := annotateFrom(t, h, anchorLoopback, map[string]any{"items": []map[string]any{{
		"id": id, "path": "memory/net.md", "host": "mediaserver",
		"anchors": []map[string]any{{"type": "file_contains", "path": lan, "text": "192.168.0.1"}}}}})
	if w.Code != http.StatusOK {
		t.Fatalf("annotate = %d: %s", w.Code, w.Body)
	}
	after := bulletLine(t, h, "memory/net.md", id)
	beforeText := before[:strings.Index(before, " <!--m ")]
	afterText := after[:strings.Index(after, " <!--m ")]
	if beforeText != afterText {
		t.Fatalf("the fact's text or stamp changed:\n%s\n%s", before, after)
	}
	if !strings.Contains(after, "id="+id) || !strings.Contains(after, "host=mediaserver") ||
		!strings.Contains(after, "anc=file_contains;host=mediaserver;path=") {
		t.Fatalf("trailer not annotated:\n%s", after)
	}
	if strings.Count(after, "id=") != 1 || strings.Count(after, "<!--m") != 1 {
		t.Fatalf("trailer duplicated:\n%s", after)
	}
}

func TestAnnotateDryRunReportsWithoutWriting(t *testing.T) {
	fakeHost(t, "aiserver")
	_, h := testServer(t)
	id := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway is on the lan"})["id"].(string)
	before := noteBody(t, h, "memory/net.md")
	w := annotateFrom(t, h, anchorLoopback, map[string]any{"dry_run": true, "items": []map[string]any{{
		"id": id, "path": "memory/net.md", "event": true}}})
	var out struct {
		Items []struct {
			Changed bool `json:"changed"`
		} `json:"items"`
	}
	decode(t, w, &out)
	if len(out.Items) != 1 || !out.Items[0].Changed {
		t.Fatalf("dry run: %s", w.Body)
	}
	if noteBody(t, h, "memory/net.md") != before {
		t.Fatal("a dry run wrote the note")
	}
}

func TestAnnotateIsRefusedForRemoteCallers(t *testing.T) {
	_, h := testServer(t)
	item := []map[string]any{{"id": "x", "path": "memory/net.md", "event": true}}
	if w := annotateFrom(t, h, anchorRemote, map[string]any{"items": item}); w.Code != http.StatusForbidden {
		t.Errorf("remote peer: %d", w.Code)
	}
	if w := annotateFrom(t, h, anchorLoopback, map[string]any{"items": item, "caller_remote": true}); w.Code != http.StatusForbidden {
		t.Errorf("caller_remote on loopback: %d", w.Code)
	}
}

func TestAnnotateKeepsExistingAnchorsWhenNoNewOneHolds(t *testing.T) {
	s, h := testServer(t)
	id := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
		"text": "the gateway is on the lan"})["id"].(string)
	storeAnchor(t, s, "memory/net.md", "the gateway is on the lan",
		"file_exists;path="+litFile(t, "x"))
	before := bulletLine(t, h, "memory/net.md", id)
	w := annotateFrom(t, h, anchorLoopback, map[string]any{"items": []map[string]any{{
		"id": id, "path": "memory/net.md",
		"anchors": []map[string]any{{"type": "file_contains", "path": litFile(t, "nope"), "text": "absent-literal"}}}}})
	var out struct {
		Items []struct {
			Changed bool             `json:"changed"`
			Anchors []map[string]any `json:"anchors"`
		} `json:"items"`
	}
	decode(t, w, &out)
	if out.Items[0].Changed || out.Items[0].Anchors[0]["result"] != "fail" {
		t.Fatalf("failing anchor changed the fact: %s", w.Body)
	}
	if bulletLine(t, h, "memory/net.md", id) != before {
		t.Fatal("existing anchors were replaced by ones that do not hold")
	}
}

func TestAnnotateEventDropsAnchorsAndUnknownFactsAreReported(t *testing.T) {
	s, h := testServer(t)
	id := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "infer": false,
		"text": "the cutover is planned"})["id"].(string)
	storeAnchor(t, s, "memory/ops.md", "the cutover is planned", "port_closed;port=65000")
	w := annotateFrom(t, h, anchorLoopback, map[string]any{"items": []map[string]any{
		{"id": id, "path": "memory/ops.md", "event": true},
		{"id": "nosuchid", "path": "memory/ops.md", "event": true},
	}})
	if w.Code != http.StatusOK {
		t.Fatalf("annotate = %d: %s", w.Code, w.Body)
	}
	line := bulletLine(t, h, "memory/ops.md", id)
	if !strings.Contains(line, "evt=1") || strings.Contains(line, "anc=") {
		t.Fatalf("event kept its anchors:\n%s", line)
	}
	if !strings.Contains(w.Body.String(), "fact not found") {
		t.Fatalf("unknown fact not reported: %s", w.Body)
	}
}

// Items on one note are applied in memory and the note is written once: 3 facts
// on one note and 2 on another are 2 note writes, not 5. Two items on the same
// fact see each other, so both trailer changes land.
func TestAnnotateWritesEachNoteOnceAndSequencesItemsOnOneFact(t *testing.T) {
	fakeHost(t, "aiserver")
	_, h := testServer(t)
	var ops, net []string
	for _, text := range []string{"the cutover is planned", "the backup runs nightly", "the mail relay is down"} {
		ops = append(ops, rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "infer": false,
			"text": text})["id"].(string))
	}
	for _, text := range []string{"the gateway is on the lan", "the switch is managed"} {
		net = append(net, rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "net", "infer": false,
			"text": text})["id"].(string))
	}
	var items []map[string]any
	for _, id := range ops {
		items = append(items, map[string]any{"id": id, "path": "memory/ops.md", "event": true})
	}
	for _, id := range net {
		items = append(items, map[string]any{"id": id, "path": "memory/net.md", "event": true})
	}
	// A second change to the first ops fact, after its event change.
	items = append(items, map[string]any{"id": ops[0], "path": "memory/ops.md", "host": "mediaserver"})

	w := annotateFrom(t, h, anchorLoopback, map[string]any{"items": items})
	if w.Code != http.StatusOK {
		t.Fatalf("annotate = %d: %s", w.Code, w.Body)
	}
	var out struct {
		NotesWritten int `json:"notes_written"`
		Items        []struct {
			Changed bool   `json:"changed"`
			Error   string `json:"error"`
		} `json:"items"`
	}
	decode(t, w, &out)
	if out.NotesWritten != 2 {
		t.Fatalf("notes_written = %d, want 2 (one write per note): %s", out.NotesWritten, w.Body)
	}
	for i, it := range out.Items {
		if !it.Changed || it.Error != "" {
			t.Fatalf("item %d: changed=%v error=%q", i, it.Changed, it.Error)
		}
	}
	first := bulletLine(t, h, "memory/ops.md", ops[0])
	if !strings.Contains(first, "evt=1") || !strings.Contains(first, "host=mediaserver") {
		t.Fatalf("two items on one fact did not both land:\n%s", first)
	}
	for _, id := range append(append([]string{}, ops[1:]...), net...) {
		path := "memory/ops.md"
		if strings.Contains(noteBody(t, h, "memory/net.md"), id) {
			path = "memory/net.md"
		}
		if !strings.Contains(bulletLine(t, h, path, id), "evt=1") {
			t.Fatalf("fact %s not marked event", id)
		}
	}
}

func TestRecallDoesNotRunChecksOnAnotherMachineByDefault(t *testing.T) {
	fakeHost(t, "aiserver")
	for _, e := range []memory.Entry{
		{Anchors: "file_exists;path=/x", Host: "elsewhere"},
		{Anchors: "file_exists;host=elsewhere;path=/x"},
	} {
		if checkedHere(e) {
			t.Fatalf("%+v would be checked on another machine during a recall", e)
		}
	}
	if !checkedHere(memory.Entry{Anchors: "file_exists;path=/x", Host: "aiserver"}) || !checkedHere(memory.Entry{Anchors: "file_exists;path=/x"}) {
		t.Fatal("a local fact's checks should run")
	}
	if checkedHere(memory.Entry{}) {
		t.Fatal("a fact with no checks has nothing to run")
	}
}
