package oauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/identity"
)

// Config is what an operator provides; Handler is what New derives from it.
type Config struct {
	Store *Store

	// PublicBase is the base URL of the tunnel — where claude.ai and
	// ChatGPT's backends reach this server. Everything except
	// /oauth/authorize is served relative to it. The MCP resource this
	// server protects is PublicBase + "/mcp".
	PublicBase string

	// AuthorizeBase is the base URL of the private, owner-only consent
	// listener — reachable only from the tailnet (or loopback). Only
	// /oauth/authorize lives here.
	AuthorizeBase string

	// Redirects bounds which hosts Dynamic Client Registration may claim.
	// Nil uses DefaultRedirectPatterns.
	Redirects *RedirectAllowlist

	// Identity, when set, is asked who the caller is on every
	// /oauth/authorize request — normally identity.FromEnv()'s resolver,
	// which is a no-op unless GRIMOIRE_IDENTITY names a backend.
	Identity *identity.Resolver

	// AllowedLogins gates identity.Identify's answer: only these logins may
	// be treated as the vault's owner. Required for the Tailscale
	// fast-path to ever fire; empty means every consent goes through the
	// admin-token form.
	AllowedLogins []string

	// AdminToken gates both the admin-token consent fallback and the
	// client-management API. Normally GRIMOIRE_ADMIN_TOKEN, the same value
	// grimoire-mcp already presents to the main API as X-Grimoire-Admin.
	AdminToken string
}

// Handler serves both the public and the private OAuth surfaces. Which
// routes it answers depends only on which ServeMux it is registered into —
// see RegisterPublic / RegisterPrivate — never on where the process bound a
// socket, so a misconfigured bind cannot accidentally expose /oauth/authorize
// to the internet.
type Handler struct {
	Config
	Resource string // canonical MCP resource URI: PublicBase + "/mcp"
	signer   *signer
}

// New validates cfg and builds a Handler.
func New(cfg Config) (*Handler, error) {
	cfg.PublicBase = strings.TrimRight(cfg.PublicBase, "/")
	cfg.AuthorizeBase = strings.TrimRight(cfg.AuthorizeBase, "/")
	if cfg.PublicBase == "" {
		return nil, errors.New("oauth: PublicBase is required")
	}
	if cfg.AuthorizeBase == "" {
		return nil, errors.New("oauth: AuthorizeBase is required")
	}
	if cfg.Store == nil {
		return nil, errors.New("oauth: Store is required")
	}
	if cfg.Redirects == nil {
		cfg.Redirects = NewRedirectAllowlist(DefaultRedirectPatterns())
	}
	sg, err := newSigner()
	if err != nil {
		return nil, err
	}
	return &Handler{Config: cfg, Resource: cfg.PublicBase + "/mcp", signer: sg}, nil
}

// RegisterPublic mounts the surface reachable from claude.ai/ChatGPT's cloud:
// discovery metadata, registration, token issuance and revocation.
// Deliberately NOT /oauth/authorize — see RegisterPrivate.
func (h *Handler) RegisterPublic(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", h.resourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", h.serverMetadata)
	mux.HandleFunc("POST /oauth/register", h.register)
	mux.HandleFunc("POST /oauth/token", h.token)
	mux.HandleFunc("POST /oauth/revoke", h.revoke)
}

// PublicMux is RegisterPublic on a fresh mux, for standalone use and tests.
// A request for /oauth/authorize against this mux gets ServeMux's ordinary
// 404 — there is no handler for it here, which is what makes "the public
// listener refuses /oauth/authorize" true by construction rather than by a
// check somebody could forget.
func (h *Handler) PublicMux() *http.ServeMux {
	mux := http.NewServeMux()
	h.RegisterPublic(mux)
	return mux
}

// RegisterPrivate mounts the surface only the owner's own browser should
// ever load: the consent page, and the small client-management console.
func (h *Handler) RegisterPrivate(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/authorize", h.authorizeShow)
	mux.HandleFunc("POST /oauth/authorize", h.authorizeDecide)
	mux.HandleFunc("GET /admin/oauth", h.adminPage)
	mux.HandleFunc("GET /admin/oauth/clients", h.adminList)
	mux.HandleFunc("POST /admin/oauth/clients/{id}/revoke", h.adminRevoke)
}

// PrivateMux is RegisterPrivate on a fresh mux.
func (h *Handler) PrivateMux() *http.ServeMux {
	mux := http.NewServeMux()
	h.RegisterPrivate(mux)
	return mux
}

// ---------------------------------------------------------- discovery

func (h *Handler) resourceMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, ProtectedResourceMetadata{
		Resource:               h.Resource,
		AuthorizationServers:   []string{h.PublicBase},
		ScopesSupported:        AllScopes,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Grimoire",
	})
}

func (h *Handler) serverMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, AuthorizationServerMetadata{
		Issuer:                            h.PublicBase,
		AuthorizationEndpoint:             h.AuthorizeBase + "/oauth/authorize",
		TokenEndpoint:                     h.PublicBase + "/oauth/token",
		RegistrationEndpoint:              h.PublicBase + "/oauth/register",
		RevocationEndpoint:                h.PublicBase + "/oauth/revoke",
		ScopesSupported:                   AllScopes,
		ResponseTypesSupported:            []string{"code"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		TokenEndpointAuthMethodsSupported: []string{"none", "client_secret_post"},
	})
}

// ---------------------------------------------------------- registration

type registerRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// register implements RFC 7591 Dynamic Client Registration, narrowly: every
// requested redirect_uri must already be on the host allowlist, or the whole
// registration is refused. This is the one gate standing between "any cloud
// service can register a client" (true — DCR has no other access control)
// and "any cloud service can get a token" (false — that still needs a human
// to approve it at /oauth/authorize, on the private listener, per request).
func (h *Handler) register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_client_metadata", "malformed JSON body")
		return
	}
	if len(req.RedirectURIs) == 0 {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uris is required")
		return
	}
	for _, u := range req.RedirectURIs {
		if !h.Redirects.Allowed(u) {
			writeOAuthErr(w, http.StatusBadRequest, "invalid_redirect_uri",
				fmt.Sprintf("%q is not on this server's allowed redirect host list", u))
			return
		}
	}
	name := strings.TrimSpace(req.ClientName)
	if name == "" {
		name = "Unnamed MCP client"
	}
	client, secret, err := h.Store.CreateClient(name, req.RedirectURIs, req.TokenEndpointAuthMethod)
	if err != nil {
		writeOAuthErr(w, http.StatusInternalServerError, "server_error", "could not register client")
		return
	}
	resp := map[string]any{
		"client_id":                  client.ID,
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": client.AuthMethod,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"client_id_issued_at":        client.Created.Unix(),
	}
	if secret != "" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0 // 0 per RFC 7591: does not expire
	}
	writeJSON(w, http.StatusCreated, resp)
}

// ---------------------------------------------------------- authorize (private)

func exactRedirectMatch(registered []string, uri string) bool {
	for _, r := range registered {
		if r == uri {
			return true
		}
	}
	return false
}

func (h *Handler) authorizeShow(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("response_type") != "code" {
		h.renderError(w, `response_type must be "code"`)
		return
	}
	client, err := h.Store.GetClient(q.Get("client_id"))
	if err != nil {
		h.renderError(w, "unknown client_id — this connector has not registered with this server")
		return
	}
	redirectURI := q.Get("redirect_uri")
	// Checked BEFORE anything below is allowed to redirect anywhere: an
	// unregistered redirect_uri is exactly the open-redirect OAuth 2.1
	// requires refusing, so every later error in this handler reports
	// itself on THIS page rather than by sending the browser to a URI
	// nobody vouched for.
	if !exactRedirectMatch(client.RedirectURIs, redirectURI) {
		h.renderError(w, "redirect_uri does not match what this client registered")
		return
	}
	state := q.Get("state")
	challenge := q.Get("code_challenge")
	method := q.Get("code_challenge_method")
	if challenge == "" || method != "S256" {
		h.redirectErr(w, r, redirectURI, state, "invalid_request", "PKCE with S256 is required")
		return
	}
	if resource := q.Get("resource"); resource != "" && resource != h.Resource {
		h.redirectErr(w, r, redirectURI, state, "invalid_target", "resource does not match this server")
		return
	}
	ar := authRequest{
		ClientID:            client.ID,
		RedirectURI:         redirectURI,
		State:               state,
		Requested:           ValidateScopes(strings.Fields(q.Get("scope"))),
		Resource:            h.Resource,
		CodeChallenge:       challenge,
		CodeChallengeMethod: method,
	}
	reqToken, err := h.signer.sign(ar)
	if err != nil {
		h.renderError(w, "internal error preparing the consent form")
		return
	}
	owner, _ := h.identifyOwner(r)
	h.renderConsent(w, client, ar, reqToken, owner, "")
}

func (h *Handler) authorizeDecide(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.renderError(w, "malformed form submission")
		return
	}
	ar, err := h.signer.verify(r.PostForm.Get("req"))
	if err != nil {
		h.renderError(w, "this consent request has expired — go back to the connector and try again")
		return
	}
	client, err := h.Store.GetClient(ar.ClientID)
	if err != nil {
		h.renderError(w, "unknown client")
		return
	}
	owner, ok := h.identifyOwner(r)
	if !ok {
		if !h.checkAdminToken(r.PostForm.Get("admin_token")) {
			h.renderConsent(w, client, ar, r.PostForm.Get("req"), Owner{}, "That token was not accepted.")
			return
		}
		owner = Owner{Verified: true, Name: "admin", Via: "admin-token"}
	}
	_ = owner // identified for the approval to be meaningful; not yet persisted per-token (single-owner deployment)

	if r.PostForm.Get("decision") != "approve" {
		h.redirectErr(w, r, ar.RedirectURI, ar.State, "access_denied", "the owner declined")
		return
	}
	granted := ValidateScopes(r.PostForm["scope"])
	code, err := randomToken()
	if err != nil {
		h.renderError(w, "internal error issuing the authorization code")
		return
	}
	err = h.Store.SaveAuthCode(code, AuthCode{
		ClientID:            ar.ClientID,
		RedirectURI:         ar.RedirectURI,
		Scopes:              granted,
		Resource:            ar.Resource,
		CodeChallenge:       ar.CodeChallenge,
		CodeChallengeMethod: ar.CodeChallengeMethod,
	}, 2*time.Minute)
	if err != nil {
		h.renderError(w, "internal error issuing the authorization code")
		return
	}
	h.Store.pruneExpiredCodes()
	u, _ := url.Parse(ar.RedirectURI)
	uq := u.Query()
	uq.Set("code", code)
	if ar.State != "" {
		uq.Set("state", ar.State)
	}
	u.RawQuery = uq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (h *Handler) renderError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = errorPage.Execute(w, msg)
}

// redirectErr reports an error the OAuth way — by sending the browser back
// to the client with ?error=... — used only once redirect_uri has already
// been checked against the client's registered set.
func (h *Handler) redirectErr(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		h.renderError(w, desc)
		return
	}
	q := u.Query()
	q.Set("error", code)
	if desc != "" {
		q.Set("error_description", desc)
	}
	if state != "" {
		q.Set("state", state)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (h *Handler) renderConsent(w http.ResponseWriter, client *Client, ar authRequest, reqToken string, owner Owner, formErr string) {
	host := ar.RedirectURI
	if u, err := url.Parse(ar.RedirectURI); err == nil && u.Host != "" {
		host = u.Host
	}
	data := struct {
		ClientName   string
		RedirectHost string
		Owner        Owner
		ReqToken     string
		Scopes       []scopeView
		Error        string
	}{
		ClientName:   client.Name,
		RedirectHost: host,
		Owner:        owner,
		ReqToken:     reqToken,
		Scopes:       scopeViews(ar.Requested),
		Error:        formErr,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consentPage.Execute(w, data)
}

// ---------------------------------------------------------- token (public)

func (h *Handler) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		h.tokenFromCode(w, r)
	case "refresh_token":
		h.tokenFromRefresh(w, r)
	default:
		writeOAuthErr(w, http.StatusBadRequest, "unsupported_grant_type",
			"only authorization_code and refresh_token are supported")
	}
}

func (h *Handler) authenticateClient(f url.Values) (*Client, bool) {
	client, err := h.Store.GetClient(f.Get("client_id"))
	if err != nil {
		return nil, false
	}
	if client.hasSecret() && !h.Store.CheckClientSecret(client.ID, f.Get("client_secret")) {
		return nil, false
	}
	return client, true
}

func (h *Handler) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	client, ok := h.authenticateClient(f)
	if !ok {
		writeOAuthErr(w, http.StatusUnauthorized, "invalid_client", "unknown client or bad client_secret")
		return
	}
	ac, err := h.Store.ConsumeAuthCode(f.Get("code"))
	if err != nil {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_grant", "unknown, expired or already-used authorization code")
		return
	}
	if ac.ClientID != client.ID {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_grant", "authorization code was issued to a different client")
		return
	}
	if ac.RedirectURI != f.Get("redirect_uri") {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	if resource := f.Get("resource"); resource != "" && resource != h.Resource {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_target", "resource does not match this server")
		return
	}
	if !VerifyPKCE(f.Get("code_verifier"), ac.CodeChallenge, ac.CodeChallengeMethod) {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	pair, err := h.Store.IssueTokenPair(client.ID, ac.Scopes, ac.Resource)
	if err != nil {
		writeOAuthErr(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	writeTokenResponse(w, pair)
}

func (h *Handler) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	f := r.PostForm
	client, ok := h.authenticateClient(f)
	if !ok {
		writeOAuthErr(w, http.StatusUnauthorized, "invalid_client", "unknown client or bad client_secret")
		return
	}
	raw := f.Get("refresh_token")
	if raw == "" {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	pair, err := h.Store.RotateRefresh(raw, client.ID)
	if err != nil {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_grant", "unknown, expired or already-used refresh token")
		return
	}
	writeTokenResponse(w, pair)
}

func writeTokenResponse(w http.ResponseWriter, pair *TokenPair) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  pair.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    pair.ExpiresIn,
		"refresh_token": pair.RefreshToken,
		"scope":         joinScope(pair.Scopes),
	})
}

// ---------------------------------------------------------- revoke (public)

// revoke implements RFC 7009. Per §2.2, an unknown or already-invalid token
// is not an error — the endpoint always reports success so it cannot be used
// to test which tokens are still live.
func (h *Handler) revoke(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	token := r.PostForm.Get("token")
	if token == "" {
		writeOAuthErr(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	_ = h.Store.RevokeToken(token)
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------- shared helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOAuthErr(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}
