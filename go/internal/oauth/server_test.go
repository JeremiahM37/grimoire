package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/identity"
)

// fakeTailscaled serves the one LocalAPI call the Tailscale identity backend
// makes, so tests never touch a real tailscaled socket. logins maps a peer
// address to the LoginName it "belongs" to; an address not in the map
// answers 200 with an empty profile, matching how tailscaled behaves for a
// peer it does not recognise.
func fakeTailscaled(t *testing.T, logins map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-Tailscale") != "localapi" {
			http.Error(w, "missing CSRF header", http.StatusForbidden)
			return
		}
		addr := r.URL.Query().Get("addr")
		host, _, _ := strings.Cut(addr, ":")
		login := logins[host]
		w.Header().Set("Content-Type", "application/json")
		if login == "" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Node":        map[string]any{"Name": "peer.tailnet.ts.net.", "Hostinfo": map[string]any{"Hostname": "peer"}},
			"UserProfile": map[string]any{"LoginName": login},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testHandler(t *testing.T, ts *httptest.Server) *Handler {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatalf("Open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var resolver *identity.Resolver
	if ts != nil {
		resolver = identity.New(&identity.Tailscale{
			Endpoint: ts.URL,
			Ranges:   []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10")},
		})
	}
	h, err := New(Config{
		Store:         store,
		PublicBase:    "https://mcp.example.com",
		AuthorizeBase: "https://consent.example.com",
		Identity:      resolver,
		AllowedLogins: []string{"jam@github"},
		AdminToken:    "s3cret-admin-token",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func pkcePair() (verifier, challenge string) {
	verifier = "test-code-verifier-0123456789-abcdefghijklmnop"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

var hiddenReq = regexp.MustCompile(`name="req" value="([^"]+)"`)

func extractReqToken(t *testing.T, html string) string {
	t.Helper()
	m := hiddenReq.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("could not find hidden req token in consent page:\n%s", html)
	}
	return m[1]
}

// registerClient performs Dynamic Client Registration against the public
// mux and returns the issued client_id.
func registerClient(t *testing.T, pub http.Handler, redirectURI string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": []string{redirectURI},
		"client_name":   "Test Connector",
	})
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	pub.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: want 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("register: bad JSON: %v", err)
	}
	if resp["client_secret"] != nil {
		t.Fatal("a client that did not ask for a confidential auth method must not receive a secret")
	}
	return resp["client_id"].(string)
}

// TestFullOAuthFlow walks the whole lifecycle end to end: DCR, authorize
// with PKCE via a faked tailnet whois, token issuance, tools scope shape,
// refresh rotation and revocation.
func TestFullOAuthFlow(t *testing.T) {
	ownerIP := "100.64.0.5"
	ts := fakeTailscaled(t, map[string]string{ownerIP: "jam@github"})
	h := testHandler(t, ts)
	pub, priv := h.PublicMux(), h.PrivateMux()

	redirectURI := "http://localhost:12345/callback"
	clientID := registerClient(t, pub, redirectURI)
	verifier, challenge := pkcePair()

	// --- GET /oauth/authorize on the PRIVATE mux, as the owner ---
	authorizeURL := "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"notes:read notes:write memory"},
		"state":                 {"xyz123"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	getReq := httptest.NewRequest(http.MethodGet, authorizeURL, nil)
	getReq.RemoteAddr = ownerIP + ":54321"
	getRec := httptest.NewRecorder()
	priv.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET /oauth/authorize: want 200, got %d: %s", getRec.Code, getRec.Body.String())
	}
	page := getRec.Body.String()
	if !strings.Contains(page, "jam@github") {
		t.Fatalf("expected the owner to be shown as pre-identified via Tailscale, got:\n%s", page)
	}
	if strings.Contains(page, `name="admin_token"`) {
		t.Fatal("a pre-identified owner should not be asked for the admin token")
	}
	reqToken := extractReqToken(t, page)

	// --- POST approval, deliberately leaving "credentials" unchecked ---
	form := url.Values{
		"req":      {reqToken},
		"decision": {"approve"},
		"scope":    {ScopeNotesRead, ScopeNotesWrite, ScopeMemory},
	}
	postReq := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.RemoteAddr = ownerIP + ":54321"
	postRec := httptest.NewRecorder()
	priv.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusFound {
		t.Fatalf("POST /oauth/authorize: want 302, got %d: %s", postRec.Code, postRec.Body.String())
	}
	loc, err := url.Parse(postRec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("bad Location header: %v", err)
	}
	if loc.Query().Get("state") != "xyz123" {
		t.Fatalf("state must round-trip, got %q", loc.Query().Get("state"))
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("expected an authorization code in the redirect, got %s", loc)
	}

	// --- POST /oauth/token on the PUBLIC mux ---
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"resource":      {h.Resource},
	}
	tokenReq := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRec := httptest.NewRecorder()
	pub.ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusOK {
		t.Fatalf("POST /oauth/token: want 200, got %d: %s", tokenRec.Code, tokenRec.Body.String())
	}
	var tok map[string]any
	if err := json.Unmarshal(tokenRec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("bad token response JSON: %v", err)
	}
	scope, _ := tok["scope"].(string)
	if strings.Contains(scope, "credentials") {
		t.Fatalf("credentials must not be granted when the owner left it unchecked, got scope=%q", scope)
	}
	if !strings.Contains(scope, "notes:read") || !strings.Contains(scope, "notes:write") || !strings.Contains(scope, "memory") {
		t.Fatalf("expected the three checked scopes, got %q", scope)
	}
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("expected both tokens, got %+v", tok)
	}

	rec, err := h.Store.LookupAccessToken(access)
	if err != nil {
		t.Fatalf("issued access token must resolve: %v", err)
	}
	if rec.Resource != h.Resource {
		t.Fatalf("access token must be bound to this server's resource, got %q", rec.Resource)
	}

	// --- reusing the same code must fail: single-use ---
	replay := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
	replay.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	replayRec := httptest.NewRecorder()
	pub.ServeHTTP(replayRec, replay)
	if replayRec.Code != http.StatusBadRequest {
		t.Fatalf("replaying an authorization code: want 400, got %d", replayRec.Code)
	}

	// --- refresh rotation ---
	refreshForm := url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refresh}}
	refreshReq := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(refreshForm.Encode()))
	refreshReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	refreshRec := httptest.NewRecorder()
	pub.ServeHTTP(refreshRec, refreshReq)
	if refreshRec.Code != http.StatusOK {
		t.Fatalf("refresh: want 200, got %d: %s", refreshRec.Code, refreshRec.Body.String())
	}
	var tok2 map[string]any
	_ = json.Unmarshal(refreshRec.Body.Bytes(), &tok2)
	if tok2["access_token"] == access {
		t.Fatal("rotation must issue a new access token")
	}
	if tok2["refresh_token"] == refresh {
		t.Fatal("rotation must issue a new refresh token")
	}

	// old refresh token is now dead
	reuseRec := httptest.NewRecorder()
	reuseReq := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(refreshForm.Encode()))
	reuseReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	pub.ServeHTTP(reuseRec, reuseReq)
	if reuseRec.Code != http.StatusBadRequest {
		t.Fatalf("reusing a rotated-away refresh token: want 400, got %d", reuseRec.Code)
	}

	// --- revoke the new access token ---
	newAccess, _ := tok2["access_token"].(string)
	revokeForm := url.Values{"token": {newAccess}}
	revokeReq := httptest.NewRequest(http.MethodPost, "/oauth/revoke", strings.NewReader(revokeForm.Encode()))
	revokeReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	revokeRec := httptest.NewRecorder()
	pub.ServeHTTP(revokeRec, revokeReq)
	if revokeRec.Code != http.StatusOK {
		t.Fatalf("revoke: want 200, got %d", revokeRec.Code)
	}
	if _, err := h.Store.LookupAccessToken(newAccess); err != ErrNotFound {
		t.Fatalf("revoked token must no longer resolve, got err=%v", err)
	}
}

func TestRegisterRefusesUnknownRedirectHost(t *testing.T) {
	h := testHandler(t, nil)
	pub := h.PublicMux()
	body, _ := json.Marshal(map[string]any{
		"redirect_uris": []string{"https://evil.example.com/callback"},
		"client_name":   "Sketchy",
	})
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	pub.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unregistered redirect host, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] != "invalid_redirect_uri" {
		t.Fatalf("want error=invalid_redirect_uri, got %+v", resp)
	}
}

func TestPublicListenerHidesAuthorize(t *testing.T) {
	h := testHandler(t, nil)
	pub := h.PublicMux()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		req := httptest.NewRequest(method, "/oauth/authorize?client_id=x", nil)
		rec := httptest.NewRecorder()
		pub.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s /oauth/authorize on the public mux: want 404, got %d", method, rec.Code)
		}
	}
}

func TestNonOwnerWhoisDenied(t *testing.T) {
	strangerIP := "100.64.9.9"
	ts := fakeTailscaled(t, map[string]string{strangerIP: "someone-else@github"})
	h := testHandler(t, ts)
	pub, priv := h.PublicMux(), h.PrivateMux()

	redirectURI := "http://localhost:9/callback"
	clientID := registerClient(t, pub, redirectURI)
	_, challenge := pkcePair()

	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirectURI},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}.Encode()
	getReq := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q, nil)
	getReq.RemoteAddr = strangerIP + ":1"
	getRec := httptest.NewRecorder()
	priv.ServeHTTP(getRec, getReq)
	page := getRec.Body.String()
	if strings.Contains(page, "someone-else@github") {
		t.Fatal("a login not on the allowlist must never be shown as the pre-identified owner")
	}
	if !strings.Contains(page, `name="admin_token"`) {
		t.Fatal("a caller who is not the recognised owner must be asked for the admin token")
	}
	reqToken := extractReqToken(t, page)

	// Approving without the right admin token must not mint a code.
	form := url.Values{"req": {reqToken}, "decision": {"approve"}, "admin_token": {"wrong"}}
	postReq := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.RemoteAddr = strangerIP + ":1"
	postRec := httptest.NewRecorder()
	priv.ServeHTTP(postRec, postReq)
	if postRec.Code == http.StatusFound {
		t.Fatal("a wrong admin token must never produce a redirect with an authorization code")
	}

	// The correct admin token does work, proving the fallback path itself
	// is not broken — only identity spoofing is refused.
	form.Set("admin_token", "s3cret-admin-token")
	postReq2 := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	postReq2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq2.RemoteAddr = strangerIP + ":1"
	postRec2 := httptest.NewRecorder()
	priv.ServeHTTP(postRec2, postReq2)
	if postRec2.Code != http.StatusFound {
		t.Fatalf("the correct admin token should still be able to approve, got %d: %s", postRec2.Code, postRec2.Body.String())
	}
}

func TestAuthorizeRefusesUnregisteredRedirectURI(t *testing.T) {
	h := testHandler(t, nil)
	pub, priv := h.PublicMux(), h.PrivateMux()
	clientID := registerClient(t, pub, "http://localhost:1/callback")
	_, challenge := pkcePair()

	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID},
		"redirect_uri":          {"http://localhost:1/somewhere-else"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+q, nil)
	rec := httptest.NewRecorder()
	priv.ServeHTTP(rec, req)
	if rec.Code == http.StatusFound {
		t.Fatal("an unregistered redirect_uri must never produce a redirect")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 (rendered error page), got %d", rec.Code)
	}
}

func TestAdminRevokeRequiresToken(t *testing.T) {
	h := testHandler(t, nil)
	priv := h.PrivateMux()
	req := httptest.NewRequest(http.MethodGet, "/admin/oauth/clients", nil)
	rec := httptest.NewRecorder()
	priv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin listing without a token: want 401, got %d", rec.Code)
	}

	ok := httptest.NewRequest(http.MethodGet, "/admin/oauth/clients", nil)
	ok.Header.Set("X-Grimoire-Admin", "s3cret-admin-token")
	okRec := httptest.NewRecorder()
	priv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("admin listing with the right token: want 200, got %d", okRec.Code)
	}
}
