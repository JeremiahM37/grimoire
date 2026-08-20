package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The auth gate protects the secret vault, so its tests are about what it
// REFUSES. The routes named here are the ones whose exposure actually matters:
// unlocking the vault, listing and adding secrets, issuing grants, and reading
// the audit trail.
var protectedRoutes = []struct {
	method, path string
}{
	{"GET", "/api/notes"},
	{"GET", "/api/search?q=x"},
	{"GET", "/api/secrets"},
	{"POST", "/api/vault/unlock"},
	{"GET", "/api/grants"},
	{"GET", "/api/audit"},
	{"POST", "/api/reindex"},
	{"GET", "/api/memory"},
	{"GET", "/api/blocks"},
	{"GET", "/api/bookmarks"},
	{"POST", "/api/bookmarks"},
	{"DELETE", "/api/bookmarks?kind=note&target=x"},
	// Agent memory is note content in every shape it is served in, and each of
	// these was a separate handler that could have forgotten to say so.
	{"GET", "/api/memory/export"},
	{"GET", "/api/memory/facets"},
	{"GET", "/api/memory/graph"},
	{"POST", "/api/memory/search"},
	{"POST", "/api/embed"},
	{"POST", "/api/memory/batch"},
	{"POST", "/api/memory/feedback"},
	{"PATCH", "/api/memory/entry"},
	{"DELETE", "/api/memory/entry?path=memory/x.md&id=1"},
}

func TestAuthTokenUnsetLeavesServerOpen(t *testing.T) {
	// The documented default. Asserted so that adding the gate cannot silently
	// become mandatory for existing deployments.
	s, _ := testServer(t)
	s.AuthToken = ""
	h := s.Routes()
	for _, rt := range protectedRoutes {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(rt.method, rt.path, nil))
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("%s %s was gated with no token configured", rt.method, rt.path)
		}
	}
}

func TestAuthTokenRejectsMissingAndWrongCredentials(t *testing.T) {
	s, _ := testServer(t)
	s.AuthToken = "s3cret-token"
	h := s.Routes()

	for _, rt := range protectedRoutes {
		for _, tc := range []struct {
			name string
			set  func(*http.Request)
		}{
			{"no credential", func(*http.Request) {}},
			{"wrong bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }},
			{"empty bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") }},
			{"wrong scheme", func(r *http.Request) { r.Header.Set("Authorization", "Basic s3cret-token") }},
			{"wrong cookie", func(r *http.Request) {
				r.AddCookie(&http.Cookie{Name: authCookie, Value: "nope"})
			}},
			{"prefix of the token", func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer s3cret")
			}},
			{"token plus suffix", func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer s3cret-token-extra")
			}},
		} {
			req := httptest.NewRequest(rt.method, rt.path, nil)
			tc.set(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s: got %d, want 401",
					rt.method, rt.path, tc.name, rec.Code)
			}
		}
	}
}

func TestAuthTokenAcceptsValidCredentials(t *testing.T) {
	s, _ := testServer(t)
	s.AuthToken = "s3cret-token"
	h := s.Routes()

	for _, tc := range []struct {
		name string
		set  func(*http.Request)
	}{
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer s3cret-token") }},
		{"bearer lowercase scheme", func(r *http.Request) {
			r.Header.Set("Authorization", "bearer s3cret-token")
		}},
		{"cookie", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: authCookie, Value: "s3cret-token"})
		}},
	} {
		req := httptest.NewRequest("GET", "/api/notes", nil)
		tc.set(req)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("valid credential via %s was rejected", tc.name)
		}
	}
}

// A ?token= is accepted once and promoted to a cookie, so the credential stops
// travelling in URLs — where it would end up in proxy logs and Referer headers.
func TestAuthTokenFromQueryIsPromotedToCookie(t *testing.T) {
	s, _ := testServer(t)
	s.AuthToken = "s3cret-token"
	h := s.Routes()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/notes?token=s3cret-token", nil))
	if rec.Code == http.StatusUnauthorized {
		t.Fatal("valid ?token= was rejected")
	}
	var got *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == authCookie {
			got = c
		}
	}
	if got == nil {
		t.Fatal("no auth cookie was set after a ?token= handoff")
	}
	if !got.HttpOnly {
		t.Error("auth cookie is readable from JavaScript")
	}
	if got.SameSite != http.SameSiteStrictMode {
		t.Error("auth cookie is not SameSite=Strict, so state-changing routes are CSRF-reachable")
	}
}

// Health stays open so an uptime check or proxy probe does not need the
// credential. It must not become a way to learn anything else.
func TestAuthTokenLeavesHealthOpen(t *testing.T) {
	s, _ := testServer(t)
	s.AuthToken = "s3cret-token"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health should stay reachable, got %d", rec.Code)
	}
}

// The static console must be gated too: it is same-origin with the API, so
// serving it unauthenticated hands an attacker the client for the API.
func TestAuthTokenGatesTheConsole(t *testing.T) {
	s, _ := testServer(t)
	s.AuthToken = "s3cret-token"
	s.WebDir = t.TempDir()
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("console served without a credential: got %d", rec.Code)
	}
}

// The headers SECURITY.md promises are asserted here because two of them were
// documented but not being sent at all.
func TestSecurityHeadersArePresent(t *testing.T) {
	s, _ := testServer(t)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	for header, want := range map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            "no-referrer",
		"X-Frame-Options":            "SAMEORIGIN",
		"Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("no Content-Security-Policy header")
	}
}

func TestFrameOptionsOverride(t *testing.T) {
	s, _ := testServer(t)
	s.FrameOptions = "DENY"
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/api/health", nil))
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want DENY", got)
	}
}

// The sync token authenticates a peer, and only a peer. Sharing a credential
// with another machine must not also hand that machine the secret vault.
func TestSyncTokenIsScopedToPeerRoutes(t *testing.T) {
	s, _ := testServer(t)
	s.AuthToken = "admin-token"
	s.SyncToken = "peer-token"
	h := s.Routes()

	peer := []struct{ method, path string }{
		{"GET", "/api/sync/manifest"},
		{"POST", "/api/sync/pull"},
		{"POST", "/api/sync/push"},
		{"POST", "/api/crdt/merge"},
		{"GET", "/api/crdt/doc/some%2Fnote.md"},
	}
	for _, rt := range peer {
		req := httptest.NewRequest(rt.method, rt.path, nil)
		req.Header.Set("Authorization", "Bearer peer-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("peer route %s %s rejected the sync token", rt.method, rt.path)
		}
	}

	notPeer := []struct{ method, path string }{
		{"GET", "/api/secrets"},
		{"GET", "/api/grants"},
		{"GET", "/api/audit"},
		{"POST", "/api/vault/unlock"},
		{"GET", "/api/notes"},
		{"POST", "/api/secrets/x/grant"},
	}
	for _, rt := range notPeer {
		req := httptest.NewRequest(rt.method, rt.path, nil)
		req.Header.Set("Authorization", "Bearer peer-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("sync token reached non-peer route %s %s (got %d)",
				rt.method, rt.path, rec.Code)
		}
	}

	// The admin token still reaches everything, peer routes included.
	for _, rt := range append(peer, notPeer...) {
		req := httptest.NewRequest(rt.method, rt.path, nil)
		req.Header.Set("Authorization", "Bearer admin-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized {
			t.Errorf("admin token rejected on %s %s", rt.method, rt.path)
		}
	}
}
