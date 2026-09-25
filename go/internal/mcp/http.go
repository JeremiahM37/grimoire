package mcp

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Streamable-HTTP transport for the MCP server.
//
// README has documented `GRIMOIRE_MCP_TRANSPORT=http ./grimoire-mcp` serving at
// 127.0.0.1:9112/mcp since before the Go rewrite; the rewrite shipped stdio
// only, so the variable did nothing and the documented address answered
// nothing. The dispatch logic is shared with the stdio loop — this is a second
// transport over the same handle(), never a second implementation.
//
// It binds loopback by default for the reason the README already gives: there
// is no authentication here, so exposing it is the reverse proxy's job. Two
// other ways to reach it exist now, both opt-in and both real authentication
// rather than a bind-address assumption: a static GRIMOIRE_MCP_TOKEN bearer
// (unchanged, see authorized below), and OAuth 2.1 via Server.OAuth — see
// internal/oauth and docs/web-connectors.md for why claude.ai and ChatGPT
// need the second one: their connectors run from the vendor's own cloud and
// can send nothing but a token that an OAuth flow handed them.

// MaxRequestBytes bounds a single JSON-RPC frame. Notes can be large, so this
// matches the stdio scanner's ceiling rather than a typical API default.
const MaxRequestBytes = 16 << 20

// supportedProtocolVersions are the MCP-Protocol-Version values this server
// answers. The transport spec says a server SHOULD reject a version it does
// not recognise with 400 rather than silently guess — recognising several
// past revisions here is what makes that check meaningful instead of
// rejecting every real client, since the wire format this server actually
// speaks has not changed across them.
var supportedProtocolVersions = map[string]bool{
	"2024-11-05": true,
	"2025-03-26": true,
	"2025-06-18": true,
	"2025-11-25": true,
}

// HTTPHandler serves JSON-RPC over POST at /mcp, plus — when Server.OAuth is
// configured — the public half of the OAuth surface (discovery metadata,
// registration, token issuance, revocation) on the same mux. The private
// half, /oauth/authorize, is never mounted here; see
// internal/oauth.Handler.RegisterPrivate and cmd/grimoire-mcp/main.go, which
// serves it on a second listener.
func (s *Server) HTTPHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.serveMCP)
	if s.OAuth != nil {
		s.OAuth.RegisterPublic(mux)
	}
	return mux
}

func (s *Server) serveMCP(w http.ResponseWriter, r *http.Request) {
	auth := s.checkAuth(r)
	if !auth.ok {
		// A bearer challenge rather than a bare 403: MCP clients read it,
		// and a hosted one can prompt for the token — or, with
		// resource_metadata present, walk straight into the OAuth
		// discovery flow — instead of failing with nothing to act on.
		w.Header().Set("WWW-Authenticate", s.wwwAuthenticate("", ""))
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		// The Streamable HTTP transport spec allows GET to open a
		// server-initiated SSE stream, which this server does not offer;
		// 405 with Allow is the spec's other permitted answer.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedProtocolVersions[v] {
		http.Error(w, "unsupported MCP-Protocol-Version", http.StatusBadRequest)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, MaxRequestBytes+1))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	if len(raw) > MaxRequestBytes {
		http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
		return
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		// A malformed frame gets a JSON-RPC parse error rather than a bare
		// 400, because the client is speaking JSON-RPC and can act on it.
		writeRPC(w, http.StatusBadRequest, &response{
			JSONRPC: "2.0",
			Error:   &rpcError{Code: -32700, Message: "parse error"},
		})
		return
	}
	if auth.scopes != nil && req.Method == "tools/call" {
		// Checked here, ahead of dispatch, because a POST in this
		// transport is exactly one tool call: the resource-request-level
		// 403 the spec describes for insufficient_scope maps cleanly onto
		// "this one call was out of scope," which would not be true if a
		// single request could carry several.
		var params struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &params)
		if need, ok := scopeFor(params.Name); ok && !scopeGrantedLocal(auth.scopes, need) {
			w.Header().Set("WWW-Authenticate", s.wwwAuthenticate("insufficient_scope", need))
			http.Error(w, "insufficient scope", http.StatusForbidden)
			return
		}
	}
	resp := s.handle(req)
	if resp == nil {
		// A notification has no id and takes no reply.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if auth.scopes != nil && req.Method == "tools/list" {
		if m, ok := resp.Result.(map[string]any); ok {
			if list, ok := m["tools"].([]tool); ok {
				m["tools"] = filterToolsByScope(list, auth.scopes)
			}
		}
	}
	writeRPC(w, http.StatusOK, resp)
}

// authResult is what checkAuth decided.
type authResult struct {
	ok bool
	// scopes is nil for an unrestricted caller — the static bearer, or no
	// auth configured at all — and non-nil (possibly empty) for a caller
	// authenticated via OAuth, whose access is bounded to exactly what was
	// granted at consent time.
	scopes []string
}

// checkAuth decides whether the request may proceed, and with what scope.
//
// Static bearer and OAuth are independent and both accepted: InboundToken is
// unrestricted, matching every deployment before this feature existed, while
// an OAuth-authenticated caller is bounded to the scopes its token carries.
// With NEITHER configured, the request is admitted unrestricted — the
// original loopback-trust default, unchanged by this file existing.
func (s *Server) checkAuth(r *http.Request) authResult {
	hasStatic := strings.TrimSpace(s.InboundToken) != ""
	hasOAuth := s.OAuth != nil
	if !hasStatic && !hasOAuth {
		return authResult{ok: true}
	}
	if hasStatic && staticBearerOK(r, s.InboundToken) {
		return authResult{ok: true}
	}
	if hasOAuth {
		if tok := bearerToken(r); tok != "" {
			if rec, err := s.OAuth.Store.LookupAccessToken(tok); err == nil && rec.Resource == s.OAuth.Resource {
				scopes := rec.Scopes
				if scopes == nil {
					scopes = []string{}
				}
				return authResult{ok: true, scopes: scopes}
			}
		}
	}
	return authResult{ok: false}
}

// staticBearerOK checks the inbound bearer token in constant time.
func staticBearerOK(r *http.Request, want string) bool {
	got := bearerToken(r)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// bearerToken reads the caller's presented token: the Authorization header,
// falling back to a query parameter for clients that cannot set headers on
// an SSE/stream URL. The query form is accepted second, never preferred —
// URLs end up in logs and browser history in a way headers do not.
func bearerToken(r *http.Request) string {
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	return got
}

// wwwAuthenticate builds the challenge for a 401 or 403. When OAuth is
// configured it carries resource_metadata, per RFC 9728 §5.1 and the MCP
// authorization spec's discovery flow — the one thing an OAuth-capable
// client needs to go find the authorization server on its own. errCode/scope
// add the insufficient_scope parameters described in the spec's Scope
// Challenge Handling section; both empty means a plain unauthorized
// challenge.
func (s *Server) wwwAuthenticate(errCode, scope string) string {
	if s.OAuth == nil {
		return `Bearer realm="grimoire"`
	}
	v := fmt.Sprintf(`Bearer realm="grimoire", resource_metadata="%s/.well-known/oauth-protected-resource"`,
		s.OAuth.PublicBase)
	if errCode != "" {
		v += fmt.Sprintf(`, error="%s"`, errCode)
	}
	if scope != "" {
		v += fmt.Sprintf(`, scope="%s"`, scope)
	}
	return v
}

// loopback reports whether an address is reachable only from this machine.
func loopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	switch host {
	case "localhost", "":
		return host == "localhost" // an empty host means every interface
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeRPC(w http.ResponseWriter, status int, resp *response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// ListenAndServe runs the HTTP transport until the process is stopped.
// Deliberately not named ServeHTTP: this type is not an http.Handler, and a
// method with that name and a different signature reads like a broken one.
func (s *Server) ListenAndServe(addr string) error {
	// Fail closed. The whole point of binding off-loopback is that something
	// remote can reach it, and what it reaches is the vault plus the credential
	// broker. Refusing here makes the dangerous configuration impossible rather
	// than documented-against. OAuth counts as real authentication for this
	// check the same way GRIMOIRE_MCP_TOKEN does — both are opt-in and both
	// reject an unauthenticated caller, which is the property this guard
	// actually cares about.
	if !loopback(addr) && strings.TrimSpace(s.InboundToken) == "" && s.OAuth == nil {
		return fmt.Errorf("refusing to serve MCP on %s without GRIMOIRE_MCP_TOKEN or OAuth: "+
			"this transport exposes the vault and the credential broker, so an "+
			"unauthenticated non-loopback bind would publish both", addr)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.HTTPHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
