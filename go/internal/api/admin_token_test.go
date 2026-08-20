package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The homelab case this exists for: notes and retrieval deliberately open to a
// trusted network, and the levers — the credential vault, connectors, accounts
// — not.

func adminTokenServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, _ := testServer(t)
	s.AdminToken = "sekrit-admin-token"
	return s, s.Routes()
}

func withAdmin(t *testing.T, h http.Handler, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := requestFor(t, method, path, body)
	if token != "" {
		req.Header.Set("X-Grimoire-Admin", token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAdminTokenClosesTheLeversAndLeavesReadingOpen(t *testing.T) {
	s, h := adminTokenServer(t)
	// Seed through the vault directly: the API is now gated for this test.
	if _, err := s.Vault.Write("open.md", "# Open\n\nkestrel migration", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert("open.md"); err != nil {
		t.Fatal(err)
	}

	// Reading is untouched — this is the whole point of a second token.
	// /api/vault/status is here on purpose: whether the vault is locked is a
	// status the console shows on every load, not a lever, and gating it means
	// a padlock that reports an error instead of a state.
	for _, path := range []string{
		"/api/notes", "/api/notes/open.md", "/api/search?q=kestrel",
		"/api/retrieve?q=kestrel&k=3", "/api/health", "/api/briefing",
		"/api/vault/status",
	} {
		if w := do(t, h, "GET", path, nil); w.Code != http.StatusOK {
			t.Errorf("%s = %d, want it to stay open", path, w.Code)
		}
	}
	// Writing a note is reading's neighbour, not an administrative act.
	if w := do(t, h, "POST", "/api/notes", map[string]any{
		"path": "written.md", "body": "# W"}); w.Code != http.StatusCreated {
		t.Errorf("note write = %d", w.Code)
	}

	// The levers are closed.
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/secrets", nil},
		{"POST", "/api/vault/unlock", map[string]any{"passphrase": "guess"}},
		{"GET", "/api/grants", nil},
		{"GET", "/api/audit", nil},
		{"GET", "/api/connectors", nil},
		{"POST", "/api/connectors", map[string]any{"kind": "rss"}},
		{"GET", "/api/users", nil},
		{"POST", "/api/reindex", nil},
	} {
		if w := do(t, h, c.method, c.path, c.body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 without the admin token", c.method, c.path, w.Code)
		}
		if w := withAdmin(t, h, c.method, c.path, c.body, "sekrit-admin-token"); w.Code == http.StatusUnauthorized {
			t.Errorf("%s %s refused the correct admin token", c.method, c.path)
		}
		if w := withAdmin(t, h, c.method, c.path, c.body, "wrong"); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s accepted a wrong admin token: %d", c.method, c.path, w.Code)
		}
	}
}

// With no token configured the server behaves exactly as it did — this must not
// become a setting people have to know about to keep their instance working.
func TestNoAdminTokenChangesNothing(t *testing.T) {
	_, h := testServer(t)
	for _, path := range []string{"/api/secrets", "/api/connectors", "/api/users"} {
		if w := do(t, h, "GET", path, nil); w.Code == http.StatusUnauthorized {
			t.Errorf("%s = 401 with no admin token configured", path)
		}
	}
}

// On a multi-user instance a signed-in administrator has already proved more
// than the token does, so accounts take precedence and an admin is not asked
// for a second credential.
func TestASignedInAdministratorDoesNotNeedTheToken(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "alice", "admin")
	s.AdminToken = "sekrit-admin-token"
	h = s.Routes()

	if w := asKey(t, h, adminKey, "GET", "/api/connectors", nil); w.Code != http.StatusOK {
		t.Fatalf("signed-in admin = %d, want the account to be enough", w.Code)
	}
	// A member still cannot, with or without the token: the token is a floor
	// for instances with no accounts, not a way around the ones that have them.
	memberKey := makeUser(t, s, h, adminKey, "bob", "member")
	if w := asKey(t, h, memberKey, "GET", "/api/connectors", nil); w.Code == http.StatusOK {
		t.Error("a member reached the connector configuration")
	}
}
