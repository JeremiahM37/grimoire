package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

const forgetText = "the deploy service is owned by the platform team"

// rememberAs writes one fact through the public route, as key (or anonymously
// when key is empty), and returns its note and id.
func rememberAs(t *testing.T, h http.Handler, key, topic, text, agent string, human bool) (string, string) {
	t.Helper()
	body := map[string]any{"text": text, "topic": topic, "agent": agent, "human": human}
	var w *httptest.ResponseRecorder
	if key == "" {
		w = do(t, h, "POST", "/api/memory", body)
	} else {
		w = asKey(t, h, key, "POST", "/api/memory", body)
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", w.Code, w.Body)
	}
	var res map[string]any
	decode(t, w, &res)
	path, _ := res["path"].(string)
	return path, entryIDIn(t, h, key, path, text)
}

func rememberFact(t *testing.T, h http.Handler, topic, text, agent string, human bool) (string, string) {
	return rememberAs(t, h, "", topic, text, agent, human)
}

// entryIDIn finds the id of the entry carrying text in a note, through recall.
func entryIDIn(t *testing.T, h http.Handler, key, path, text string) string {
	t.Helper()
	url := "/api/memory?q=" + strings.ReplaceAll(text, " ", "+")
	var w *httptest.ResponseRecorder
	if key == "" {
		w = do(t, h, "GET", url, nil)
	} else {
		w = asKey(t, h, key, "GET", url, nil)
	}
	var facts []map[string]any
	decode(t, w, &facts)
	for _, f := range facts {
		if f["text"] == text && f["path"] == path {
			return f["id"].(string)
		}
	}
	t.Fatalf("no entry %q in %s: %s", text, path, w.Body)
	return ""
}

// cascadeReq builds a cascade request. agent is sent as the agent identity;
// empty means the caller presents none, which is a person.
func cascadeReq(t *testing.T, path, id, agent string, dry bool) *http.Request {
	t.Helper()
	req := requestFor(t, "POST", "/api/memory/forget", map[string]any{
		"path": path, "id": id, "cascade": true, "dry_run": dry})
	if agent != "" {
		req.Header.Set("X-Grimoire-Agent", agent)
	}
	return req
}

func withKey(req *http.Request, key string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

func run(t *testing.T, h http.Handler, req *http.Request) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Code == http.StatusOK {
		decode(t, rec, &out)
	}
	return rec.Code, out
}

func cNote(t *testing.T, s *Server, path string) string {
	t.Helper()
	n, err := s.Vault.Read(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return n.Body
}

// assertNoForgottenText checks the response itself does not repeat the text.
func assertNoForgottenText(t *testing.T, out map[string]any) {
	t.Helper()
	b, _ := json.Marshal(out)
	for _, w := range []string{"platform team", "owned by"} {
		if strings.Contains(string(b), w) {
			t.Errorf("response repeats the forgotten text (%q): %s", w, b)
		}
	}
}

func TestCascadeRedactsAndRemovesAcrossCopies(t *testing.T) {
	s, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "claude", false)
	copyPath, _ := rememberFact(t, h, "ownership",
		"Ownership note: "+forgetText+" and pages the on call rota", "claude", false)
	rememberFact(t, h, "ownership", "it pages the on call rota", "claude", false)

	code, out := run(t, h, cascadeReq(t, path, id, "claude-code", false))
	if code != http.StatusOK {
		t.Fatalf("cascade = %d %v", code, out)
	}
	if out["status"] != "clean" {
		t.Errorf("status = %v, verification = %v", out["status"], out["verification"])
	}
	if body := cNote(t, s, path); strings.Contains(body, "platform team") || strings.Contains(body, "~~") {
		t.Errorf("target not removed outright:\n%s", body)
	}
	copyBody := cNote(t, s, copyPath)
	if !strings.Contains(copyBody, "Ownership note: [forgotten] and pages") {
		t.Errorf("the copy was not redacted in place:\n%s", copyBody)
	}
	if !strings.Contains(copyBody, "pages the on call rota") {
		t.Errorf("an unrelated fact was lost:\n%s", copyBody)
	}
	assertNoForgottenText(t, out)
}

func TestAgentForgetChallengesAHumanCopyAndReportsIt(t *testing.T) {
	s, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "claude", false)
	// A person's own entry says the same thing, in the same note.
	rememberFact(t, h, "ownership", "Confirmed by Sam: "+forgetText, "", true)
	code, out := run(t, h, cascadeReq(t, path, id, "claude-code", false))
	if code != http.StatusOK {
		t.Fatalf("cascade = %d %v", code, out)
	}
	if out["status"] != "residual" {
		t.Errorf("a person's entry still carrying the text must be residual, got %v", out["status"])
	}
	body := cNote(t, s, path)
	if !strings.Contains(body, forgetText) {
		t.Errorf("a person's entry was altered by an agent-initiated forget:\n%s", body)
	}
	challenged := false
	for _, ln := range strings.Split(body, "\n") {
		if strings.Contains(ln, "chal=") && strings.Contains(ln, forgottenMark) {
			challenged = true
		}
	}
	if !challenged {
		t.Errorf("no challenge entry was filed beside the person's entry:\n%s", body)
	}
	assertNoForgottenText(t, out)
	// A repeat files no second challenge.
	if code, _ := run(t, h, cascadeReq(t, path, id, "claude-code", false)); code != http.StatusNotFound && code != http.StatusOK {
		t.Fatalf("repeat = %d", code)
	}
	if n := strings.Count(cNote(t, s, path), "chal="); n != 1 {
		t.Errorf("repeat filed %d challenge entries, want 1", n)
	}
}

func TestAgentCannotCascadeAPersonsEntry(t *testing.T) {
	s, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "", true)
	code, _ := run(t, h, cascadeReq(t, path, id, "claude-code", false))
	if code != http.StatusForbidden {
		t.Fatalf("agent cascade over a person's entry = %d, want 403", code)
	}
	if !strings.Contains(cNote(t, s, path), forgetText) {
		t.Error("the person's entry was removed")
	}
}

func TestHumanForgetCascadesFully(t *testing.T) {
	s, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "", true)
	code, out := run(t, h, cascadeReq(t, path, id, "", false))
	if code != http.StatusOK || out["status"] != "clean" {
		t.Fatalf("human forget = %d %v", code, out)
	}
	if strings.Contains(cNote(t, s, path), "platform team") {
		t.Error("a human-initiated cascade left the text behind")
	}
}

func TestCascadeDryRunChangesNothingAndWritesNoReceipt(t *testing.T) {
	s, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "claude", false)
	before := cNote(t, s, path)
	code, out := run(t, h, cascadeReq(t, path, id, "claude-code", true))
	if code != http.StatusOK || out["status"] != "dry_run" {
		t.Fatalf("dry run = %d %v", code, out)
	}
	if acts, _ := out["actions"].([]any); len(acts) == 0 {
		t.Error("dry run planned nothing")
	}
	if cNote(t, s, path) != before {
		t.Error("dry run changed the note")
	}
	if _, err := os.Stat(filepath.Join(s.Vault.Root, receiptsDir)); err == nil {
		t.Error("dry run wrote a receipt")
	}
}

func TestCascadeIsIdempotentAndAnswersWithTheReceipt(t *testing.T) {
	_, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "claude", false)
	code, first := run(t, h, cascadeReq(t, path, id, "claude-code", false))
	if code != http.StatusOK {
		t.Fatalf("first = %d", code)
	}
	code, second := run(t, h, cascadeReq(t, path, id, "claude-code", false))
	if code != http.StatusOK || second["already_forgotten"] != true {
		t.Fatalf("second = %d %v", code, second)
	}
	if second["receipt"] != first["receipt"] {
		t.Errorf("repeat returned receipt %v, want %v", second["receipt"], first["receipt"])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestFor(t, "GET", "/api/memory/receipts", nil))
	var list []map[string]any
	decode(t, rec, &list)
	if len(list) != 1 {
		t.Errorf("receipts listed = %d, want 1", len(list))
	}
}

func TestReceiptsHoldNoForgottenText(t *testing.T) {
	s, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "claude", false)
	rememberFact(t, h, "ownership", "Ownership note: "+forgetText+" and pages", "claude", false)
	code, out := run(t, h, cascadeReq(t, path, id, "claude-code", false))
	if code != http.StatusOK {
		t.Fatalf("cascade = %d", code)
	}
	rc, _ := out["receipt"].(string)
	if rc == "" {
		t.Fatal("no receipt path returned")
	}
	md := cNote(t, s, rc)
	jsonFiles, _ := filepath.Glob(filepath.Join(s.Vault.Root, receiptsDir, "*.json"))
	if len(jsonFiles) != 1 {
		t.Fatalf("json receipts = %v", jsonFiles)
	}
	raw, _ := os.ReadFile(jsonFiles[0])
	for _, word := range []string{"platform", "owned by", "deploy service", "pages"} {
		if strings.Contains(strings.ToLower(md), word) || strings.Contains(strings.ToLower(string(raw)), word) {
			t.Errorf("receipt contains %q", word)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || doc["target_hash"] == "" || doc["target_tokens"] == nil {
		t.Errorf("receipt json = %s", raw)
	}
}

func TestResidualHitsOutsideMemoryAreReported(t *testing.T) {
	s, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "claude", false)
	if w := do(t, h, "POST", "/api/notes", map[string]any{
		"path": "ops/handbook.md", "body": "# Handbook\n\n" + forgetText + " per the wiki.\n"}); w.Code != http.StatusCreated {
		t.Fatalf("create note = %d", w.Code)
	}
	code, out := run(t, h, cascadeReq(t, path, id, "claude-code", false))
	if code != http.StatusOK || out["status"] != "residual" {
		t.Fatalf("a person's note holding the text must be residual: %d %v", code, out["status"])
	}
	if !strings.Contains(cNote(t, s, "ops/handbook.md"), forgetText) {
		t.Error("a person's note was altered")
	}
}

func TestDreamReportsAreRedacted(t *testing.T) {
	s, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "claude", false)
	if w := do(t, h, "POST", "/api/notes", map[string]any{
		"path": "Dreams/2026-08-14.md", "body": "# Dream\n\n- " + forgetText + "\n- unrelated line\n"}); w.Code != http.StatusCreated {
		t.Fatalf("create dream = %d %s", w.Code, w.Body)
	}
	if code, _ := run(t, h, cascadeReq(t, path, id, "claude-code", false)); code != http.StatusOK {
		t.Fatalf("cascade = %d", code)
	}
	dream := cNote(t, s, "Dreams/2026-08-14.md")
	if strings.Contains(dream, "platform team") || !strings.Contains(dream, "unrelated line") {
		t.Errorf("dream report not redacted:\n%s", dream)
	}
}

func TestShortTextIsRefused(t *testing.T) {
	_, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", "yes ok", "claude", false)
	if code, _ := run(t, h, cascadeReq(t, path, id, "claude-code", false)); code != http.StatusBadRequest {
		t.Errorf("short text = %d", code)
	}
}

func TestPlainForgetIsUnchanged(t *testing.T) {
	_, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "claude", false)
	w := do(t, h, "DELETE", "/api/memory/entry?path="+path+"&id="+id+"&agent=claude", nil)
	want := `{"id":"` + id + `","path":"` + path + `","retracted":true}` + "\n"
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("plain delete body = %q, want %q", w.Body.String(), want)
	}
	path2, id2 := rememberFact(t, h, "ownership", "a different fact about the rota", "claude", false)
	w2 := do(t, h, "POST", "/api/memory/forget", map[string]any{"path": path2, "id": id2, "agent": "claude"})
	want2 := `{"id":"` + id2 + `","path":"` + path2 + `","retracted":true}` + "\n"
	if w2.Code != http.StatusOK || w2.Body.String() != want2 {
		t.Fatalf("post without cascade body = %q, want %q", w2.Body.String(), want2)
	}
}

func TestCascadeCannotWriteWhereTheCallerCannot(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")
	w := asKey(t, h, adminKey, "POST", "/api/spaces",
		map[string]any{"name": "Shared", "prefix": "memory/shared"})
	if w.Code != http.StatusCreated {
		t.Fatalf("space = %d %s", w.Code, w.Body)
	}
	var space map[string]any
	decode(t, w, &space)
	if w := asKey(t, h, adminKey, "POST", "/api/spaces/"+space["id"].(string)+"/members",
		map[string]any{"user": "bob", "role": "reader"}); w.Code != http.StatusOK {
		t.Fatalf("add reader = %d %s", w.Code, w.Body)
	}
	// The copy sits in the space bob may only read; the target is in commons.
	copyPath := "memory/shared/copy.md"
	body := "# Memory\n\n" + memory.Entry{ID: memory.DeriveID("2026-08-14 09:00", "claude",
		"Shared copy: "+forgetText+" today"), Text: "Shared copy: " + forgetText + " today",
		Agent: "claude", Stamp: "2026-08-14 09:00"}.Format() + "\n"
	if _, err := s.Vault.Write(copyPath, body, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert(copyPath); err != nil {
		t.Fatal(err)
	}
	tpath, tid := rememberAs(t, h, bobKey, "ownership", forgetText, "claude", false)

	before := cNote(t, s, copyPath)
	code, out := run(t, h, withKey(cascadeReq(t, tpath, tid, "claude-code", false), bobKey))
	if code != http.StatusOK {
		t.Fatalf("cascade as bob = %d %v", code, out)
	}
	if out["status"] != "residual" {
		t.Errorf("a copy bob cannot write must be residual, got %v", out["status"])
	}
	if cNote(t, s, copyPath) != before {
		t.Error("a cascade wrote into a space the caller cannot write")
	}
	assertNoForgottenText(t, out)
}

func TestProfilesDoNotServeAForgottenFactFromCache(t *testing.T) {
	_, h := testServer(t)
	path, id := rememberFact(t, h, "ownership", forgetText, "claude", false)
	rememberFact(t, h, "ownership", "it pages the on call rota", "claude", false)
	// Build and cache a profile that contains the text.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, requestFor(t, "GET", "/api/memory/profile", nil))
	if !strings.Contains(rec.Body.String(), "platform team") {
		t.Fatalf("profile before the forget lacks the fact: %s", rec.Body)
	}
	if code, _ := run(t, h, cascadeReq(t, path, id, "claude-code", false)); code != http.StatusOK {
		t.Fatalf("cascade = %d", code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, requestFor(t, "GET", "/api/memory/profile", nil))
	if strings.Contains(rec.Body.String(), "platform team") {
		t.Errorf("profile still serves the forgotten fact: %s", rec.Body)
	}
}
