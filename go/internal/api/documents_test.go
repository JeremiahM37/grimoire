package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListDocumentsDoesNotAppendUnreadableRecords(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")
	if _, err := s.Documents.ImportBytesAt("/external/private.txt", "private.txt", []byte("secret"), "users/alice/private.md"); err != nil {
		t.Fatal(err)
	}
	note, err := s.Vault.Read("users/alice/private.md")
	if err != nil {
		t.Fatal(err)
	}
	fm := note.Frontmatter.Clone()
	fm.Set("private", true)
	if _, err := s.Vault.Write(note.Path, note.Body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert(note.Path); err != nil {
		t.Fatal(err)
	}
	wrapped := s.withPrincipal(http.HandlerFunc(s.listDocuments))
	req := requestFor(t, "GET", "/api/documents", nil)
	req.Header.Set("Authorization", "Bearer "+bobKey)
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "private.md") || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("unreadable document leaked: %s", w.Body)
	}
}
