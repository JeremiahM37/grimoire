package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// createConflictNote makes a note and returns its path and the hash a GET
// reports for it, which is the value an editor holds as its base.
func createConflictNote(t *testing.T, h http.Handler) (string, string) {
	t.Helper()
	w := do(t, h, "POST", "/api/notes", map[string]any{
		"title": "Conflict Note", "body": "# Conflict Note\n\nfirst line\n"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var created map[string]any
	decode(t, w, &created)
	path, _ := created["path"].(string)
	return path, noteHash(t, h, path)
}

func noteHash(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	w := do(t, h, "GET", "/api/notes/"+path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	decode(t, w, &got)
	hash, _ := got["hash"].(string)
	if hash == "" {
		t.Fatal("GET returned no hash")
	}
	return hash
}

// TestStaleSaveIsRefusedAndWritesNothing reproduces the data-loss case: the
// editor loads a note, another writer appends a line, and the editor saves
// its stale copy. The save must be a 409 and the other writer's line must
// survive on disk.
func TestStaleSaveIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	path, editorBase := createConflictNote(t, h)

	// another writer (an agent, with no base) appends a line
	agentBody := "# Conflict Note\n\nfirst line\nagent line\n"
	if w := do(t, h, "PUT", "/api/notes/"+path, map[string]any{"body": agentBody}); w.Code != http.StatusOK {
		t.Fatalf("agent write = %d: %s", w.Code, w.Body)
	}
	currentHash := noteHash(t, h, path)
	if currentHash == editorBase {
		t.Fatal("agent write did not change the hash")
	}

	// the editor saves its stale copy, based on the hash it loaded
	stale := "# Conflict Note\n\nfirst line\neditor line\n"
	w := do(t, h, "PUT", "/api/notes/"+path, map[string]any{"body": stale, "base_hash": editorBase})
	if w.Code != http.StatusConflict {
		t.Fatalf("stale save = %d, want 409: %s", w.Code, w.Body)
	}
	var conflict struct {
		Error   string         `json:"error"`
		Current map[string]any `json:"current"`
	}
	decode(t, w, &conflict)
	if conflict.Error != "conflict" {
		t.Errorf("error = %q, want conflict", conflict.Error)
	}
	if got, _ := conflict.Current["hash"].(string); got != currentHash {
		t.Errorf("current.hash = %q, want %q", got, currentHash)
	}
	if got, _ := conflict.Current["body"].(string); got != agentBody {
		t.Errorf("current.body = %q, want the agent's version", got)
	}
	if _, ok := conflict.Current["backlinks"]; !ok {
		t.Error("current is missing the GET note shape (backlinks)")
	}

	// nothing was written: the file on disk is still the agent's version
	onDisk, err := os.ReadFile(filepath.Join(s.Vault.Root, path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(onDisk, []byte("agent line")) || bytes.Contains(onDisk, []byte("editor line")) {
		t.Errorf("file changed on a refused save:\n%s", onDisk)
	}
	if noteHash(t, h, path) != currentHash {
		t.Error("hash moved after a refused save")
	}
}

func TestMatchingBaseSaves(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	path, base := createConflictNote(t, h)

	w := do(t, h, "PUT", "/api/notes/"+path, map[string]any{
		"body": "# Conflict Note\n\nedited\n", "base_hash": base})
	if w.Code != http.StatusOK {
		t.Fatalf("matching base save = %d: %s", w.Code, w.Body)
	}
	if noteHash(t, h, path) == base {
		t.Error("save with a matching base did not change the note")
	}
}

func TestNoBaseStillSaves(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	path, base := createConflictNote(t, h)

	// move the note on, so a stale base would have been refused
	if w := do(t, h, "PUT", "/api/notes/"+path, map[string]any{"body": "# Conflict Note\n\nother\n"}); w.Code != http.StatusOK {
		t.Fatalf("first write = %d: %s", w.Code, w.Body)
	}
	w := do(t, h, "PUT", "/api/notes/"+path, map[string]any{"body": "# Conflict Note\n\nno base\n"})
	if w.Code != http.StatusOK {
		t.Fatalf("no-base save = %d, want 200: %s", w.Code, w.Body)
	}
	if noteHash(t, h, path) == base {
		t.Error("no-base save did not write")
	}
}

func TestIdenticalContentSavesOnStaleBase(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	path, base := createConflictNote(t, h)
	if w := do(t, h, "PUT", "/api/notes/"+path, map[string]any{"body": "# Conflict Note\n\nmoved\n"}); w.Code != http.StatusOK {
		t.Fatalf("move = %d: %s", w.Code, w.Body)
	}
	// saving what the note already says is not a conflict, even with a stale base
	w := do(t, h, "PUT", "/api/notes/"+path, map[string]any{
		"body": "# Conflict Note\n\nmoved\n", "base_hash": base})
	if w.Code != http.StatusOK {
		t.Fatalf("identical content on stale base = %d, want 200: %s", w.Code, w.Body)
	}
}

func TestIfMatchHeaderIsAnAlternativeBase(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	path, base := createConflictNote(t, h)
	if w := do(t, h, "PUT", "/api/notes/"+path, map[string]any{"body": "# Conflict Note\n\nmoved\n"}); w.Code != http.StatusOK {
		t.Fatalf("move = %d: %s", w.Code, w.Body)
	}

	b, _ := json.Marshal(map[string]any{"body": "# Conflict Note\n\nstale\n"})
	req := httptest.NewRequest("PUT", "/api/notes/"+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"`+base+`"`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("If-Match with a stale hash = %d, want 409: %s", rec.Code, rec.Body)
	}
}

func TestMissingNoteWithBaseIsAConflict(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	w := do(t, h, "PUT", "/api/notes/gone.md", map[string]any{
		"body": "# Gone\n", "base_hash": "0123456789abcdef"})
	if w.Code != http.StatusConflict {
		t.Fatalf("missing note with base = %d, want 409: %s", w.Code, w.Body)
	}
	var conflict map[string]any
	decode(t, w, &conflict)
	if conflict["error"] != "conflict" || conflict["current"] != nil {
		t.Errorf("body = %s, want error=conflict current=null", w.Body)
	}
}
