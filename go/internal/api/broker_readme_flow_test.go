package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The README shows the credential-broker flow as concrete curl calls with
// concrete field names. Those are the first thing anyone evaluating this
// feature will run, so the exact request and response shapes are pinned here.
//
// The first draft of that section already got them wrong — `ttl` for
// `ttl_seconds`, `token` for `grant` — which is the same class of error as
// documenting a setting nothing reads, just one a reader hits in thirty
// seconds instead of never.
func TestBrokerFlowMatchesTheDocumentedShapes(t *testing.T) {
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}

	// POST /api/secrets {"name","value"}
	if w := do(t, h, "POST", "/api/secrets",
		map[string]any{"name": "gh", "value": "ghp_supersecret"}); w.Code >= 400 {
		t.Fatalf("add secret = %d: %s", w.Code, w.Body)
	}

	// POST /api/secrets/{name}/grant {"grantee","scope","ttl_seconds"}
	w := do(t, h, "POST", "/api/secrets/gh/grant", map[string]any{
		"grantee": "research-agent", "scope": "https://api.github.com/repos",
		"ttl_seconds": 3600})
	if w.Code != http.StatusOK {
		t.Fatalf("grant = %d: %s", w.Code, w.Body)
	}
	var got map[string]any
	decode(t, w, &got)
	token, ok := got["grant"].(string)
	if !ok || token == "" {
		t.Fatalf(`grant response has no "grant" field: %v`, got)
	}
	if _, ok := got["expires_in"]; !ok {
		t.Errorf(`grant response has no "expires_in" field: %v`, got)
	}

	// The listing is what the human console and list_grants read. It must
	// name the secret without ever carrying its value.
	w = do(t, h, "GET", "/api/grants", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list grants = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "research-agent") {
		t.Errorf("grant listing does not mention the grantee: %s", body)
	}
	if strings.Contains(body, "ghp_supersecret") {
		t.Fatal("the grant listing leaked the secret value")
	}

	// And the scope really binds: the README claims a grant for
	// api.github.com does not authorize api.github.com.evil.example.
	w = do(t, h, "POST", "/api/secrets/broker", map[string]any{
		"grant": token, "url": "https://api.github.com.evil.example/repos",
		"method": "GET"})
	if w.Code < 400 {
		t.Errorf("broker accepted a host-suffix URL outside the grant scope: %d %s",
			w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "ghp_supersecret") {
		t.Fatal("the broker's error response leaked the secret value")
	}
}

// A revoked grant must stop working immediately — the README sells revocation
// as the alternative to rotating a key.
func TestRevokingAGrantStopsIt(t *testing.T) {
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	do(t, h, "POST", "/api/secrets", map[string]any{"name": "gh", "value": "v"})
	w := do(t, h, "POST", "/api/secrets/gh/grant",
		map[string]any{"grantee": "a", "scope": "https://api.github.com", "ttl_seconds": 60})
	var got map[string]any
	decode(t, w, &got)
	token, _ := got["grant"].(string)
	if token == "" {
		t.Fatal("no grant issued")
	}

	req := httptest.NewRequest("DELETE", "/api/grants/"+token, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code >= 400 {
		t.Fatalf("revoke = %d: %s", rec.Code, rec.Body)
	}

	after := do(t, h, "GET", "/api/grants", nil).Body.String()
	var list []map[string]any
	if err := json.Unmarshal([]byte(after), &list); err == nil {
		for _, g := range list {
			if g["token"] == token {
				t.Error("revoked grant is still listed")
			}
		}
	}
	if w := do(t, h, "POST", "/api/secrets/broker", map[string]any{
		"grant": token, "url": "https://api.github.com/x", "method": "GET",
	}); w.Code < 400 {
		t.Errorf("a revoked grant still brokered: %d %s", w.Code, w.Body)
	}
}
