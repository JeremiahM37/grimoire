package api

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/crypto"
)

// SECURITY.md makes specific, falsifiable promises. This file turns the ones
// that are cheap to check into tests, because the alternative has already
// happened here: the document described an auth token, an SSRF guard and
// origin-exact grant scoping that the Go implementation did not have, and
// nothing failed until somebody read both.

// "Key derivation: Argon2id (64 MiB, t=3, p=4)".
func TestArgon2ParametersMatchTheDocumentedOnes(t *testing.T) {
	if crypto.ArgonMemoryKiB != 64*1024 {
		t.Errorf("Argon2 memory = %d KiB, SECURITY.md says 64 MiB", crypto.ArgonMemoryKiB)
	}
	if crypto.ArgonTime != 3 {
		t.Errorf("Argon2 time = %d, SECURITY.md says t=3", crypto.ArgonTime)
	}
	if crypto.ArgonParallelism != 4 {
		t.Errorf("Argon2 parallelism = %d, SECURITY.md says p=4", crypto.ArgonParallelism)
	}
}

// "Keep .grimoire/ (which holds secrets.enc and the index) off any sync/backup"
// and "the /api/file route and vault export exclude .grimoire so the secret
// store and index never leave."
//
// The export walks the vault, so anything the walk reaches is shipped to
// whoever asked for the zip. This asserts the encrypted secret store is not
// among it.
func TestVaultExportNeverShipsTheSecretStore(t *testing.T) {
	s, h := testServer(t)

	// a real secret store on disk, plus an ordinary note to prove the export works
	gdir := filepath.Join(s.Vault.Root, ".grimoire")
	if err := os.MkdirAll(gdir, 0o700); err != nil {
		t.Fatal(err)
	}
	const canary = "SECRET-STORE-CANARY-do-not-export"
	if err := os.WriteFile(filepath.Join(gdir, "secrets.enc"), []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	// .md inside .grimoire too: the walk filters by extension as well as by
	// directory, and only the directory rule protects this one
	if err := os.WriteFile(filepath.Join(gdir, "notes.md"), []byte(canary), 0o600); err != nil {
		t.Fatal(err)
	}
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "ordinary.md", "body": "# Ordinary\n\nplain content"})

	w := do(t, h, "GET", "/api/export/vault", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("export returned %d", w.Code)
	}
	body := w.Body.Bytes()
	if bytes.Contains(body, []byte(canary)) {
		t.Fatal("the exported vault contains the secret store's bytes")
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	found := false
	for _, f := range zr.File {
		names = append(names, f.Name)
		if strings.Contains(f.Name, ".grimoire") {
			t.Errorf("export contains %q, which is inside the secret store", f.Name)
		}
		if f.Name == "ordinary.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("export did not contain the ordinary note; it may be empty for the "+
			"wrong reason, making this test vacuous. entries: %v", names)
	}
}

// "No CORS headers are set → browsers enforce same-origin for API calls."
func TestNoCORSHeadersAreEverSet(t *testing.T) {
	s, _ := testServer(t)
	h := s.Routes()
	for _, path := range []string{"/api/health", "/api/notes", "/api/search?q=x",
		"/api/secrets", "/api/grants"} {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Origin", "https://evil.example")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		for header := range rec.Header() {
			if strings.HasPrefix(strings.ToLower(header), "access-control-") {
				t.Errorf("%s returned %s: SECURITY.md states no CORS headers are set",
					path, header)
			}
		}
	}
}

// "the plaintext never touches disk, the SQLite index, FTS search, the vector
// store / RAG" — asserted end to end rather than trusting the write path.
func TestEncryptedNoteBodyReachesNoRetrievalSurface(t *testing.T) {
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	const secret = "zarquon-plaintext-marker"
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "sealed.md", "body": "# Sealed\n\n" + secret})

	w := do(t, h, "POST", "/api/notes/sealed.md/encrypt", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("encrypt = %d: %s", w.Code, w.Body)
	}
	// Prove the note really is sealed before concluding anything from the
	// absence of its plaintext — otherwise this test passes when encryption
	// silently does nothing, which is precisely the failure it exists to catch.
	var note map[string]any
	decode(t, w, &note)
	if enc, _ := note["encrypted"].(bool); !enc {
		t.Fatalf("note did not come back encrypted: %v", note)
	}
	raw, err := os.ReadFile(filepath.Join(s.Vault.Root, "sealed.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("plaintext is still on disk after encrypting")
	}

	for _, q := range []string{
		"SELECT count(*) FROM notes WHERE body LIKE '%' || ? || '%'",
		"SELECT count(*) FROM vectors WHERE chunk LIKE '%' || ? || '%'",
	} {
		n, err := s.Index.DB.Count(q, secret)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if n != 0 {
			t.Errorf("plaintext found in the index via: %s", q)
		}
	}
	for _, path := range []string{
		"/api/search?q=zarquon-plaintext-marker&full=true",
		"/api/retrieve?q=zarquon-plaintext-marker&k=10",
	} {
		if got := do(t, h, "GET", path, nil).Body.String(); strings.Contains(got, secret) {
			t.Errorf("%s leaked the sealed plaintext", path)
		}
	}
}
