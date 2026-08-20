package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetFactUpdatesInPlaceThenAppends(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "ops.md", "body": "# Ops\n\n- port:: 8443\n\ntrailing prose\n"})

	// an existing key is REWRITTEN, keeping its indent and bullet — markdown
	// stays the source of truth, so a fact set here reads like a human typed it
	w := do(t, h, "POST", "/api/facts", map[string]any{
		"note": "ops.md", "key": "PORT", "value": "9000"})
	if w.Code != http.StatusCreated {
		t.Fatalf("set fact = %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	decode(t, w, &got)
	if got["key"] != "port" {
		t.Errorf("key = %v, want it lowercased", got["key"])
	}
	note, err := (&testVault{t, h}).read("ops.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "- PORT:: 9000") {
		t.Errorf("fact not rewritten in place:\n%s", note)
	}
	if strings.Contains(note, "8443") {
		t.Errorf("old value survived:\n%s", note)
	}

	// an unknown key is appended
	do(t, h, "POST", "/api/facts", map[string]any{
		"note": "ops.md", "key": "owner", "value": "platform"})
	note, _ = (&testVault{t, h}).read("ops.md")
	if !strings.Contains(note, "owner:: platform") {
		t.Errorf("new fact not appended:\n%s", note)
	}
	if !strings.Contains(note, "trailing prose") {
		t.Errorf("append clobbered the body:\n%s", note)
	}

	// and the fact is queryable, which is the whole point of writing it
	var facts []map[string]any
	decode(t, do(t, h, "GET", "/api/facts?key=owner", nil), &facts)
	if len(facts) != 1 || facts[0]["value"] != "platform" {
		t.Errorf("facts = %v", facts)
	}
}

func TestSetFactValidates(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "n.md", "body": "# N\n"})
	for _, body := range []map[string]any{
		{"note": "n.md", "key": "", "value": "x"},
		{"note": "n.md", "key": "k", "value": ""},
		{"note": "", "key": "k", "value": "v"},
	} {
		if w := do(t, h, "POST", "/api/facts", body); w.Code != http.StatusBadRequest {
			t.Errorf("%v = %d, want 400", body, w.Code)
		}
	}
	if w := do(t, h, "POST", "/api/facts",
		map[string]any{"note": "nope.md", "key": "k", "value": "v"}); w.Code != http.StatusNotFound {
		t.Errorf("missing note = %d, want 404", w.Code)
	}
}

func TestConsolidateMemoryDedupesWithoutAnLLM(t *testing.T) {
	_, h := testServer(t)
	// infer=false stores verbatim with no reconciliation, which is the only
	// way a duplicate reaches the note now — reconciliation refuses to write
	// one. Consolidation still has to clean up whatever is already on file,
	// including notes written before reconciliation existed.
	for i := 0; i < 2; i++ {
		if w := do(t, h, "POST", "/api/memory", map[string]any{
			"topic": "deploy", "text": "force-recreate after any VPN change",
			"agent": "claude-code", "infer": false}); w.Code >= 400 {
			t.Fatalf("remember = %d: %s", w.Code, w.Body)
		}
	}
	var mems []map[string]any
	decode(t, do(t, h, "GET", "/api/memory?shape=notes", nil), &mems)
	if n := strings.Count(mems[0]["body"].(string), "force-recreate"); n != 2 {
		t.Fatalf("test needs two duplicates on file, got %d", n)
	}

	var out map[string]any
	decode(t, do(t, h, "POST", "/api/memory/consolidate", map[string]any{}), &out)
	if out["notes_changed"] == nil {
		t.Fatalf("no result: %v", out)
	}

	decode(t, do(t, h, "GET", "/api/memory?shape=notes", nil), &mems)
	if len(mems) != 1 {
		t.Fatalf("memories = %d", len(mems))
	}
	body := mems[0]["body"].(string)
	if n := strings.Count(body, "force-recreate after any VPN change"); n != 1 {
		t.Errorf("entry appears %d times after consolidation:\n%s", n, body)
	}
}

func TestRememberRefusesToWriteADuplicateInTheFirstPlace(t *testing.T) {
	// The behaviour consolidation used to clean up after: the same belief
	// reported twice must not land twice.
	_, h := testServer(t)
	for i := 0; i < 2; i++ {
		if w := do(t, h, "POST", "/api/memory", map[string]any{
			"topic": "deploy", "text": "force-recreate after any VPN change",
			"agent": "claude-code"}); w.Code >= 400 {
			t.Fatalf("remember = %d: %s", w.Code, w.Body)
		}
	}
	var mems []map[string]any
	decode(t, do(t, h, "GET", "/api/memory?shape=notes", nil), &mems)
	if n := strings.Count(mems[0]["body"].(string), "force-recreate after any VPN change"); n != 1 {
		t.Errorf("duplicate belief written %d times:\n%s", n, mems[0]["body"])
	}
}

func TestConsolidateSnapshotsBeforeRewriting(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/memory", map[string]any{
		"topic": "deploy", "text": "one", "agent": "a", "infer": false})
	do(t, h, "POST", "/api/memory", map[string]any{
		"topic": "deploy", "text": "one", "agent": "a", "infer": false})
	do(t, h, "POST", "/api/memory/consolidate", map[string]any{})

	// memory that a model can rewrite must stay rollback-able, or the human
	// has no way back from a bad consolidation
	var versions []map[string]any
	decode(t, do(t, h, "GET", "/api/notes/memory/deploy.md/history", nil), &versions)
	if len(versions) == 0 {
		t.Error("no snapshot taken before the rewrite")
	}
}

func TestAudioMemoSavesTheRecordingEvenWithoutTranscription(t *testing.T) {
	_, h := testServer(t)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "memo.webm")
	if err != nil {
		t.Fatal(err)
	}
	audio := []byte("not really audio, but bytes are bytes")
	if _, err := part.Write(audio); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("title", "Standup thoughts")
	mw.Close()

	req := httptest.NewRequest("POST", "/api/audio", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("audio = %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// no whisper configured — the note must still exist, with the recording
	// attached. Losing the recording because a service is missing is the one
	// unacceptable outcome here.
	if !strings.Contains(got["transcript"].(string), "transcription unavailable") {
		t.Errorf("transcript = %v", got["transcript"])
	}
	notePath := got["path"].(string)
	body, err := (&testVault{t, h}).read(notePath)
	if err != nil {
		t.Fatalf("note not written: %v", err)
	}
	if !strings.Contains(body, got["audio"].(string)) {
		t.Errorf("note does not link the recording:\n%s", body)
	}
	if !strings.Contains(body, "Standup thoughts") {
		t.Errorf("title lost:\n%s", body)
	}
}

// testVault reads a note back through the API, so a test asserts on what a
// client would actually see rather than on the file layout.
type testVault struct {
	t *testing.T
	h http.Handler
}

func (v *testVault) read(rel string) (string, error) {
	w := do(v.t, v.h, "GET", "/api/notes/"+rel, nil)
	if w.Code != http.StatusOK {
		return "", errStatus(w.Code)
	}
	var note map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &note); err != nil {
		return "", err
	}
	body, _ := note["body"].(string)
	fm, _ := note["frontmatter"].(map[string]any)
	title, _ := fm["title"].(string)
	return title + "\n" + body, nil
}

type errStatus int

func (e errStatus) Error() string { return http.StatusText(int(e)) }
