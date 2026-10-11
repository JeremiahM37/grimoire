package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/seal"
)

// The sealed-fact contract, over the real HTTP surface. The canary is a phrase
// no other test writes, so finding it anywhere is a leak, not a coincidence.
const sealCanary = "zeppelin-quartz-harbor forecast seven"

// rememberSealed writes one sealed fact as the caller and returns its path and id.
func rememberSealed(t *testing.T, h http.Handler, key, text string) (string, string) {
	t.Helper()
	body := map[string]any{"text": text, "visibility": "sealed", "infer": false, "topic": "seal-test"}
	var w *httptest.ResponseRecorder
	if key == "" {
		w = do(t, h, "POST", "/api/memory", body)
	} else {
		req := requestFor(t, "POST", "/api/memory", body)
		req.Header.Set("Authorization", "Bearer "+key)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, req)
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("remember sealed = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Path    string `json:"path"`
		ID      string `json:"id"`
		Results []struct {
			Text string `json:"text"`
		} `json:"results"`
	}
	decode(t, w, &out)
	if strings.Contains(w.Body.String(), sealCanary) {
		t.Fatal("the write response carries the sealed text")
	}
	if out.ID == "" {
		t.Fatalf("no id in %s", w.Body)
	}
	return out.Path, out.ID
}

func noteBytes(t *testing.T, s *Server, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(s.Vault.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestSealedFactTextIsAbsentFromFileSearchAndExport(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rel, _ := rememberSealed(t, h, "", sealCanary)

	raw := noteBytes(t, s, rel)
	if strings.Contains(raw, "zeppelin") || strings.Contains(raw, "quartz") {
		t.Fatalf("plaintext in the note file:\n%s", raw)
	}
	if !strings.Contains(raw, "vis=sealed") || !strings.Contains(raw, "sealed=") {
		t.Fatalf("bullet is missing its sealed fields:\n%s", raw)
	}
	if w := do(t, h, "GET", "/api/search?q=zeppelin", nil); strings.Contains(w.Body.String(), "zeppelin") {
		t.Errorf("search found the sealed text: %s", w.Body)
	}
	if w := do(t, h, "GET", "/api/search?q=quartz", nil); w.Code == http.StatusOK && strings.Contains(w.Body.String(), "quartz") {
		t.Errorf("search found the sealed text: %s", w.Body)
	}
	if w := do(t, h, "GET", "/api/memory/export?include_private=1", nil); strings.Contains(w.Body.String(), "zeppelin") {
		t.Errorf("export carried the sealed text: %s", w.Body)
	}
}

func TestSealedFactNeedsAnUnlockedVault(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	s.Secrets.Lock()
	w := do(t, h, "POST", "/api/memory", map[string]any{
		"text": sealCanary, "visibility": "sealed", "infer": false, "topic": "seal-test"})
	if w.Code != http.StatusLocked {
		t.Fatalf("sealed write with a locked vault = %d, want 423: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "zeppelin") {
		t.Error("the refusal echoed the fact")
	}
}

func TestSealedFactOwnerReadsItOthersGetTheStub(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	// The first account needs no credentials; the second is made by the first's
	// administrator. Both are created before anything is written, so the owner
	// boundary is the thing under test, not the membership setup.
	adminKey := makeUser(t, s, h, "", "root", "admin")
	aliceKey := makeUser(t, s, h, adminKey, "alice", "member")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")

	_, id := rememberSealed(t, h, aliceKey, sealCanary)

	recall := func(key string) string {
		t.Helper()
		req := requestFor(t, "GET", "/api/memory?q=forecast&include_private=1", nil)
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("recall = %d: %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	if got := recall(aliceKey); !strings.Contains(got, sealCanary) {
		t.Errorf("owner did not get the sealed text back: %s", got)
	}
	for name, key := range map[string]string{"bob": bobKey, "root": adminKey} {
		got := recall(key)
		if strings.Contains(got, "zeppelin") || strings.Contains(got, "quartz") {
			t.Errorf("%s read the sealed text: %s", name, got)
		}
		if !strings.Contains(got, id) || !strings.Contains(got, "[sealed]") {
			t.Errorf("%s should see the placeholder for %s, got %s", name, id, got)
		}
	}
}

func TestSealedFactCannotBeEditedInPlace(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rel, id := rememberSealed(t, h, "", sealCanary)
	before := noteBytes(t, s, rel)
	w := do(t, h, "PATCH", "/api/memory/entry", map[string]any{
		"path": rel, "id": id, "text": "replaced in plain"})
	if w.Code != http.StatusConflict {
		t.Fatalf("patch of a sealed fact = %d, want 409: %s", w.Code, w.Body)
	}
	if noteBytes(t, s, rel) != before {
		t.Error("a refused patch still changed the note")
	}
}

func TestForgetShredsTheKeyAndNoSavedCopyOpens(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rel, id := rememberSealed(t, h, "", sealCanary)
	saved := noteBytes(t, s, rel) // a copy taken before the forget, e.g. in a backup

	var keyID, token string
	for _, e := range memory.Parse(saved) {
		if e.ID == id {
			keyID, token = e.KeyID, e.Sealed
		}
	}
	if keyID == "" || token == "" {
		t.Fatal("saved copy has no key id or ciphertext")
	}

	// Plain forget, not cascade: a sealed fact is removed whatever is asked.
	w := do(t, h, "POST", "/api/memory/forget", map[string]any{"path": rel, "id": id})
	if w.Code != http.StatusOK {
		t.Fatalf("forget = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(noteBytes(t, s, rel), id) {
		t.Error("the bullet is still in the note after forget")
	}

	ks := seal.NewKeystore(filepath.Join(s.Vault.Root, ".grimoire", "seal-keys.json"))
	if ks.Has(keyID) {
		t.Fatal("the data key survived the forget")
	}
	var plain string
	err := s.Secrets.WithSessionKey(func(key []byte) error {
		var e error
		plain, e = ks.Open(key, "local", keyID, token)
		return e
	})
	if !errors.Is(err, seal.ErrShredded) {
		t.Fatalf("opening the saved copy = %q, %v; want ErrShredded", plain, err)
	}

	// The tombstone blocks the saved copy from coming back on reindex.
	if _, err := s.Vault.Write(rel, saved, currentFrontmatter(t, s, rel)); err != nil {
		t.Fatalf("restoring the saved copy: %v", err)
	}
	if w := do(t, h, "POST", "/api/reindex", nil); w.Code != http.StatusOK {
		t.Fatalf("reindex = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(noteBytes(t, s, rel), id) {
		t.Error("a restored copy resurrected the forgotten bullet on reindex")
	}
	tomb, _, err := seal.NewTombLog(s.Vault.Root).Load(s.tombKey())
	if err != nil || !seal.Suppressed(tomb, id) {
		t.Fatalf("no tombstone for %s: %v", id, err)
	}
	logBytes, _ := os.ReadFile(filepath.Join(s.Vault.Root, seal.TombstoneFile))
	if strings.Contains(string(logBytes), id) {
		t.Error("the tombstone log carries the fact id in clear")
	}
}

func TestSealedRecallNeedsNoKeyStoreWhenNothingIsSealed(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	w := do(t, h, "POST", "/api/memory", map[string]any{
		"text": "plain fact about nothing sealed", "infer": false, "topic": "seal-test"})
	if w.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(s.Vault.Root, ".grimoire", "seal-keys.json")); !os.IsNotExist(err) {
		t.Errorf("an ordinary write created the key store (err=%v)", err)
	}
	var got []map[string]any
	decode(t, do(t, h, "GET", "/api/memory?q=nothing+sealed", nil), &got)
	if len(got) == 0 {
		t.Error("an ordinary fact is no longer recalled")
	}
}

func currentFrontmatter(t *testing.T, s *Server, rel string) *markdown.Frontmatter {
	t.Helper()
	note, err := s.Vault.Read(rel)
	if err != nil {
		t.Fatal(err)
	}
	return note.Frontmatter.Clone()
}

// A cascade forget of a sealed fact has no text to match, so it must fall back
// to the plain forget and still destroy the key, not fail with a 400.
func TestCascadeForgetOfASealedFactStillShredsTheKey(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rel, id := rememberSealed(t, h, "", sealCanary)
	var keyID string
	for _, e := range memory.Parse(noteBytes(t, s, rel)) {
		if e.ID == id {
			keyID = e.KeyID
		}
	}
	w := do(t, h, "POST", "/api/memory/forget", map[string]any{"path": rel, "id": id, "cascade": true})
	if w.Code != http.StatusOK {
		t.Fatalf("cascade forget of a sealed fact = %d: %s", w.Code, w.Body)
	}
	ks := seal.NewKeystore(filepath.Join(s.Vault.Root, ".grimoire", "seal-keys.json"))
	if keyID == "" || ks.Has(keyID) {
		t.Fatalf("key %q survived a cascade forget", keyID)
	}
}

// Consolidation rewrites a note through a model. A note holding a sealed fact
// is neither sent nor rewritten, and the response says why.
func TestConsolidateSkipsANoteWithASealedFact(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	rel, _ := rememberSealed(t, h, "", sealCanary)
	before := noteBytes(t, s, rel)
	w := do(t, h, "POST", "/api/memory/consolidate", map[string]any{"path": rel})
	if w.Code != http.StatusOK {
		t.Fatalf("consolidate = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "sealed") {
		t.Errorf("response does not say the note was held back: %s", w.Body)
	}
	if noteBytes(t, s, rel) != before {
		t.Error("consolidate rewrote a note that holds a sealed fact")
	}
}
