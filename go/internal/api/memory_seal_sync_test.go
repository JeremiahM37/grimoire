package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/seal"
)

// A forgotten fact must not come back through either sync route. These tests
// go through the real HTTP routes (the CRDT merge, the passphrase change and
// the cloud-folder pull), not the suppression helper directly.

const staleFact = "the staging token rotates every seven days"

// rememberPlain writes one ordinary fact and returns its note path and id.
func rememberPlain(t *testing.T, h http.Handler, text string) (string, string) {
	t.Helper()
	w := do(t, h, "POST", "/api/memory", map[string]any{"text": text, "infer": false, "topic": "seal-sync"})
	if w.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Path string `json:"path"`
		ID   string `json:"id"`
	}
	decode(t, w, &out)
	return out.Path, out.ID
}

func TestStalePeerCRDTPushCannotResurrectAForgottenFact(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rel, id := rememberPlain(t, h, staleFact)

	// The stale peer's copy: the document it would push, taken before the forget.
	w := do(t, h, "GET", "/api/crdt/doc/"+rel, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("crdt doc = %d: %s", w.Code, w.Body)
	}
	var doc struct {
		Doc string         `json:"doc"`
		FM  map[string]any `json:"fm"`
	}
	decode(t, w, &doc)

	if w := do(t, h, "DELETE", "/api/memory/entry?path="+url.QueryEscape(rel)+"&id="+id+"&hard=1", nil); w.Code != http.StatusOK {
		t.Fatalf("hard forget = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(noteBytes(t, s, rel), id) {
		t.Fatal("the forget left the bullet in the note")
	}

	// The stale peer pushes its document through the route.
	if w := do(t, h, "POST", "/api/crdt/merge", map[string]any{"path": rel, "doc": doc.Doc, "fm": doc.FM}); w.Code != http.StatusOK {
		t.Fatalf("crdt merge = %d: %s", w.Code, w.Body)
	}

	if raw := noteBytes(t, s, rel); strings.Contains(raw, id) || strings.Contains(raw, staleFact) {
		t.Fatalf("the stale push resurrected the forgotten fact:\n%s", raw)
	}
	if w := do(t, h, "GET", "/api/memory?q=staging+token", nil); strings.Contains(w.Body.String(), "staging token") {
		t.Errorf("recall returns the resurrected fact: %s", w.Body)
	}
}

// Changing the passphrase must keep both sealed facts and tombstones valid:
// a sealed fact is still readable by its owner afterwards, and a forget made
// before the change still suppresses after it.
func TestPassphraseChangeKeepsSealedFactsAndTombstones(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rememberSealed(t, h, "", sealCanary)
	relPlain, idPlain := rememberPlain(t, h, staleFact)
	stale := noteBytes(t, s, relPlain)
	if w := do(t, h, "DELETE", "/api/memory/entry?path="+url.QueryEscape(relPlain)+"&id="+idPlain+"&hard=1", nil); w.Code != http.StatusOK {
		t.Fatalf("forget = %d: %s", w.Code, w.Body)
	}

	w := do(t, h, "POST", "/api/vault/change-passphrase", map[string]any{
		"old": "correct horse battery", "new": "a different long passphrase"})
	if w.Code != http.StatusOK {
		t.Fatalf("change passphrase = %d: %s", w.Code, w.Body)
	}

	if got := recallAs(t, h, "", "forecast"); !strings.Contains(got, sealCanary) {
		t.Errorf("sealed fact unreadable after passphrase change: %s", got)
	}

	// The tombstone was re-signed under the new session key, so it still
	// verifies and still suppresses the restored copy.
	if _, err := s.Vault.Write(relPlain, stale, currentFrontmatter(t, s, relPlain)); err != nil {
		t.Fatal(err)
	}
	s.suppressTombstoned(relPlain)
	if strings.Contains(noteBytes(t, s, relPlain), idPlain) {
		t.Error("a tombstone stopped verifying after the passphrase change")
	}
	ts, rep, err := s.tombLog().Load(s.tombKey())
	if err != nil || rep.Verified == 0 || rep.Rejected != 0 || !seal.Suppressed(ts, idPlain) {
		t.Errorf("tombstone after passphrase change: %+v, %v", rep, err)
	}
}

// Folder sync is the other route a stale copy takes into a vault. Device B
// still has the forgotten fact and publishes a newer version of the note; device
// A, which forgot it, must not take the fact back when it pulls.
func TestCloudFolderPullCannotResurrectAForgottenFact(t *testing.T) {
	s1, h1 := cloudServer(t)
	s2, h2 := cloudServer(t)
	for _, s := range []*Server{s1, s2} {
		if err := s.Secrets.Initialize("correct horse battery"); err != nil {
			t.Fatal(err)
		}
	}
	folder := filepath.Join(t.TempDir(), "Dropbox", "Grimoire")
	if err := os.MkdirAll(filepath.Dir(folder), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, id := rememberPlain(t, h1, staleFact)

	w := do(t, h1, "POST", "/api/sync/folder", map[string]any{"folder": folder,
		"passphrase": "long enough pass", "create": true, "device_name": "A"})
	if w.Code != http.StatusOK {
		t.Fatalf("A setup = %d %s", w.Code, w.Body)
	}
	w = do(t, h2, "POST", "/api/sync/folder", map[string]any{"folder": folder,
		"passphrase": "long enough pass", "device_name": "B"})
	if w.Code != http.StatusOK {
		t.Fatalf("B join = %d %s", w.Code, w.Body)
	}
	if !strings.Contains(noteBytes(t, s2, rel), id) {
		t.Fatal("precondition failed: B did not receive the fact")
	}

	// A forgets. Its tombstone is not yet in the folder.
	if w := do(t, h1, "DELETE", "/api/memory/entry?path="+url.QueryEscape(rel)+"&id="+id+"&hard=1", nil); w.Code != http.StatusOK {
		t.Fatalf("A forget = %d: %s", w.Code, w.Body)
	}

	// B, still holding the fact, changes the note and publishes that version.
	if err := os.WriteFile(filepath.Join(s2.Vault.Root, rel), append(readFile(t, s2, rel), []byte("\n- a line B added\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Cloud.SyncOnce(); err != nil {
		t.Fatalf("B sync: %v", err)
	}

	// A pulls B's stale version.
	if _, err := s1.Cloud.SyncOnce(); err != nil {
		t.Fatalf("A sync: %v", err)
	}
	if raw := noteBytes(t, s1, rel); strings.Contains(raw, id) || strings.Contains(raw, staleFact) {
		t.Fatalf("pulling a stale cloud copy resurrected the forgotten fact:\n%s", raw)
	}
	if !strings.Contains(noteBytes(t, s1, rel), "a line B added") {
		t.Fatal("the pull did not bring B's other change, so the test did not reach the write")
	}
	_ = h2
}

func readFile(t *testing.T, s *Server, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.Vault.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// recallAs runs a recall with include_private as the given key ("" = no auth).
func recallAs(t *testing.T, h http.Handler, key, q string) string {
	t.Helper()
	req := requestFor(t, "GET", "/api/memory?q="+url.QueryEscape(q)+"&include_private=1", nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Body.String()
}
