package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The published site is the one surface with no principal behind it, so most
// of what matters is what it REFUSES.

func publishing(t *testing.T, on bool) (*Server, http.Handler) {
	t.Helper()
	s, h := testServer(t)
	if on {
		if err := s.Settings.Update(map[string]string{PublishSetting: "true"}); err != nil {
			t.Fatal(err)
		}
	}
	return s, h
}

func seedPublished(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.WriteNote("public.md",
		"# Public Note\n\nvisible to the world, links to [[Draft]] and [[Also Public]]\n",
		map[string]any{"title": "Public Note", "publish": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteNote("also.md", "# Also Public\n\nlinks to [[Public Note]]\n",
		map[string]any{"title": "Also Public", "publish": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteNote("draft.md",
		"# Draft\n\nnot for the world, links to [[Public Note]]\n",
		map[string]any{"title": "Draft"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteNote("secret.md", "# Secret\n\nnot for anyone\n",
		map[string]any{"title": "Secret", "publish": true, "private": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestPublishingDoesNotExistUntilTurnedOn(t *testing.T) {
	// A public surface must not appear because somebody typed a frontmatter
	// key.
	s, h := publishing(t, false)
	seedPublished(t, s)
	for _, path := range []string{"/published", "/published/public", "/api/published"} {
		if w := get(t, h, path); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d with publishing off, want 404", path, w.Code)
		}
	}
}

func TestPublishedNotesAreServedAnonymously(t *testing.T) {
	s, h := publishing(t, true)
	seedPublished(t, s)

	index := get(t, h, "/published")
	if index.Code != http.StatusOK {
		t.Fatalf("index = %d: %s", index.Code, index.Body)
	}
	if !strings.Contains(index.Body.String(), "Public Note") {
		t.Errorf("index does not list the published note:\n%s", index.Body)
	}
	page := get(t, h, "/published/public")
	if page.Code != http.StatusOK {
		t.Fatalf("page = %d: %s", page.Code, page.Body)
	}
	if !strings.Contains(page.Body.String(), "visible to the world") {
		t.Errorf("page did not render:\n%s", page.Body)
	}
	// The .md form works too, since that is what a link carries.
	if w := get(t, h, "/published/public.md"); w.Code != http.StatusOK {
		t.Errorf("published/public.md = %d", w.Code)
	}
}

func TestUnpublishedAndPrivateNotesAreNotServed(t *testing.T) {
	s, h := publishing(t, true)
	seedPublished(t, s)

	for _, path := range []string{"/published/draft", "/published/secret"} {
		if w := get(t, h, path); w.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, w.Code)
		}
	}
	body := get(t, h, "/published").Body.String()
	for _, forbidden := range []string{"Draft", "Secret"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the index leaked %q:\n%s", forbidden, body)
		}
	}
	// private beats publish: a note marked both is not published.
	var listed []map[string]any
	decode(t, get(t, h, "/api/published"), &listed)
	for _, n := range listed {
		if n["title"] == "Secret" {
			t.Errorf("a private note was listed as published: %v", listed)
		}
	}
}

func TestLinksResolveOnlyToPublishedNotes(t *testing.T) {
	// A link to an unpublished note must not become a working URL into the
	// vault.
	s, h := publishing(t, true)
	seedPublished(t, s)

	body := get(t, h, "/published/public").Body.String()
	if !strings.Contains(body, `href="/published/also"`) {
		t.Errorf("a link between two published notes did not resolve:\n%s", body)
	}
	if strings.Contains(body, `href="/published/draft"`) {
		t.Errorf("a link to an unpublished note resolved:\n%s", body)
	}
	if strings.Contains(body, `href="/read/draft"`) || strings.Contains(body, `/api/notes`) {
		t.Errorf("the published page links back into the private surfaces:\n%s", body)
	}
}

func TestBacklinksComeOnlyFromPublishedNotes(t *testing.T) {
	// An unpublished note linking to a published one must not be able to
	// announce itself in the footer.
	s, h := publishing(t, true)
	seedPublished(t, s)

	body := get(t, h, "/published/public").Body.String()
	_, footer, ok := strings.Cut(body, "Linked from:")
	if !ok {
		t.Fatalf("no backlinks at all:\n%s", body)
	}
	if !strings.Contains(footer, "Also Public") {
		t.Errorf("a published backlink is missing:\n%s", footer)
	}
	// Draft links here too, and appears in the body as an unresolved link the
	// author wrote — but it must not appear in the footer, which is the part
	// the linking note controls.
	if strings.Contains(footer, "Draft") {
		t.Errorf("an unpublished note announced itself through backlinks:\n%s", footer)
	}
}

func TestPublishedSurfaceStillAnswersToTheGlobalAuthToken(t *testing.T) {
	// An operator who closed the server closed it. A "public" surface that
	// punched through the gate would be a hole; the way to run a public site
	// is not to set that token.
	s, _ := publishing(t, true)
	seedPublished(t, s)
	s.AuthToken = "s3cret-token"
	h := s.Routes()

	for _, path := range []string{"/published", "/published/public", "/api/published"} {
		if w := get(t, h, path); w.Code != http.StatusUnauthorized {
			t.Errorf("%s = %d with a token set, want 401", path, w.Code)
		}
	}
}

func TestUnpublishingTakesEffect(t *testing.T) {
	s, h := publishing(t, true)
	seedPublished(t, s)
	if w := get(t, h, "/published/public"); w.Code != http.StatusOK {
		t.Fatalf("setup: %d", w.Code)
	}
	if _, err := s.WriteNote("public.md", "# Public Note\n\nno longer public\n",
		map[string]any{"title": "Public Note"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert("public.md"); err != nil {
		t.Fatal(err)
	}
	if w := get(t, h, "/published/public"); w.Code != http.StatusNotFound {
		t.Errorf("an unpublished note is still served: %d", w.Code)
	}
}

func TestPublishedIndexSaysSoWhenEmpty(t *testing.T) {
	_, h := publishing(t, true)
	body := get(t, h, "/published").Body.String()
	if !strings.Contains(body, "Nothing published") {
		t.Errorf("empty index = %s", body)
	}
}

func TestPublishedPathCannotEscapeTheVault(t *testing.T) {
	s, h := publishing(t, true)
	seedPublished(t, s)
	for _, path := range []string{
		"/published/../../etc/passwd", "/published/..%2f..%2fsecret",
		"/published/secret", "/published/nonexistent",
	} {
		if w := get(t, h, path); w.Code == http.StatusOK {
			t.Errorf("%s returned 200:\n%s", path, w.Body)
		}
	}
}
