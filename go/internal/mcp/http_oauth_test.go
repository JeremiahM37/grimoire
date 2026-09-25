package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/oauth"
)

// testOAuth builds a minimal, otherwise-unconfigured oauth.Handler — no
// Identity backend, no AllowedLogins — which is exactly what this file
// needs: internal/oauth's own server_test.go already exercises the
// authorize/consent flow end to end, including a faked Tailscale whois. What
// belongs HERE is the boundary this package owns: that a request bearing an
// OAuth token gets exactly the scopes that token carries, applied to
// tools/list and tools/call, and that the static bearer and OAuth remain
// independent.
func testOAuthHandler(t *testing.T) *oauth.Handler {
	t.Helper()
	store, err := oauth.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatalf("oauth.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h, err := oauth.New(oauth.Config{
		Store:         store,
		PublicBase:    "https://mcp.example.com",
		AuthorizeBase: "https://consent.example.com",
	})
	if err != nil {
		t.Fatalf("oauth.New: %v", err)
	}
	return h
}

func issueScopedToken(t *testing.T, oa *oauth.Handler, scopes []string) string {
	t.Helper()
	client, _, err := oa.Store.CreateClient("Test Client", []string{"http://localhost/cb"}, "")
	if err != nil {
		t.Fatalf("CreateClient: %v", err)
	}
	pair, err := oa.Store.IssueTokenPair(client.ID, scopes, oa.Resource)
	if err != nil {
		t.Fatalf("IssueTokenPair: %v", err)
	}
	return pair.AccessToken
}

func rpcPostBearer(h http.Handler, method, params, token string) *httptest.ResponseRecorder {
	body := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"`
	if params != "" {
		body += `,"params":` + params
	}
	body += `}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestOAuthConfiguredRequiresAToken(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	s.OAuth = testOAuthHandler(t)
	rec := rpcPostBearer(s.HTTPHandler(), "ping", "", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token with OAuth configured: got %d, want 401", rec.Code)
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(wa, "resource_metadata=") || !strings.Contains(wa, "oauth-protected-resource") {
		t.Fatalf("WWW-Authenticate must point at the protected-resource metadata, got %q", wa)
	}
}

func TestOAuthTokenScopesFilterToolsList(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	oa := testOAuthHandler(t)
	s.OAuth = oa
	tok := issueScopedToken(t, oa, []string{oauth.ScopeNotesRead})

	rec := rpcPostBearer(s.HTTPHandler(), "tools/list", "", tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("tools/list with a valid scoped token: got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range resp.Result.Tools {
		names[tl.Name] = true
	}
	if !names["search_notes"] {
		t.Error("search_notes (notes:read) should be listed")
	}
	if names["create_note"] {
		t.Error("create_note (notes:write) must not be listed for a notes:read-only token")
	}
	if names["use_credential"] {
		t.Error("use_credential (credentials) must not be listed for a notes:read-only token")
	}
}

func TestOAuthInsufficientScopeOnToolsCall(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	oa := testOAuthHandler(t)
	s.OAuth = oa
	tok := issueScopedToken(t, oa, []string{oauth.ScopeNotesRead})

	rec := rpcPostBearer(s.HTTPHandler(), "tools/call", `{"name":"create_note","arguments":{}}`, tok)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("calling an out-of-scope tool: got %d, want 403: %s", rec.Code, rec.Body.String())
	}
	wa := rec.Header().Get("WWW-Authenticate")
	if !strings.Contains(wa, `error="insufficient_scope"`) || !strings.Contains(wa, oauth.ScopeNotesWrite) {
		t.Fatalf("WWW-Authenticate should name the missing scope, got %q", wa)
	}
}

func TestOAuthTokenWithinScopeReachesTheTool(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	oa := testOAuthHandler(t)
	s.OAuth = oa
	tok := issueScopedToken(t, oa, []string{oauth.ScopeNotesRead})

	// search_notes is notes:read, so this must reach the dispatcher (and
	// fail for an ordinary reason — no server at that base URL — rather
	// than being blocked by scope, which would be a 403 caught above.)
	rec := rpcPostBearer(s.HTTPHandler(), "tools/call", `{"name":"search_notes","arguments":{"query":"x"}}`, tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("an in-scope tool call should reach dispatch (200 with an isError result), got %d", rec.Code)
	}
}

func TestOAuthTokenBoundToWrongResourceIsRejected(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	oa := testOAuthHandler(t)
	s.OAuth = oa
	client, _, _ := oa.Store.CreateClient("C", []string{"http://localhost/cb"}, "")
	pair, err := oa.Store.IssueTokenPair(client.ID, []string{oauth.ScopeNotesRead}, "https://someone-elses-server.example/mcp")
	if err != nil {
		t.Fatalf("IssueTokenPair: %v", err)
	}
	rec := rpcPostBearer(s.HTTPHandler(), "ping", "", pair.AccessToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a token issued for a different resource must be rejected here, got %d", rec.Code)
	}
}

func TestStaticBearerStillWorksAlongsideOAuth(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	s.InboundToken = "local-secret"
	s.OAuth = testOAuthHandler(t)

	// The static token is unrestricted — full, unfiltered tools/list —
	// exactly as it behaved before OAuth existed.
	rec := rpcPostBearer(s.HTTPHandler(), "tools/list", "", "local-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("static bearer with OAuth also configured: got %d", rec.Code)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	found := false
	for _, tl := range resp.Result.Tools {
		if tl.Name == "use_credential" {
			found = true
		}
	}
	if !found {
		t.Error("the static bearer must still see every tool, unfiltered")
	}

	// A bogus OAuth-shaped token must not fall back to being treated as the
	// static bearer, and vice versa.
	rec2 := rpcPostBearer(s.HTTPHandler(), "ping", "", "not-the-static-token-and-not-a-real-oauth-token")
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("an unrecognised token must be rejected, got %d", rec2.Code)
	}
}

func TestOAuthProtocolVersionRejectsUnsupported(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	req := httptest.NewRequest(http.MethodPost, "/mcp",
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)))
	req.Header.Set("MCP-Protocol-Version", "1999-01-01")
	rec := httptest.NewRecorder()
	s.HTTPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsupported MCP-Protocol-Version: got %d, want 400", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/mcp",
		bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)))
	req2.Header.Set("MCP-Protocol-Version", "2025-06-18")
	rec2 := httptest.NewRecorder()
	s.HTTPHandler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("a supported MCP-Protocol-Version must be accepted, got %d", rec2.Code)
	}
}

func TestOAuthPublicRoutesAreMountedOnTheMCPHandler(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	s.OAuth = testOAuthHandler(t)
	h := s.HTTPHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("protected resource metadata: got %d", rec.Code)
	}

	// /oauth/authorize must NEVER be reachable from this handler — that is
	// the private listener's route, served from a different process
	// address entirely. See cmd/grimoire-mcp/main.go's serveAuthorize.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/oauth/authorize", nil))
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("/oauth/authorize must 404 on the public MCP handler, got %d", rec2.Code)
	}
}
