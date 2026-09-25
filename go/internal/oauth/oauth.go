// Package oauth is the authorization server for grimoire-mcp's public
// Streamable-HTTP transport.
//
// Claude.ai and ChatGPT add an MCP server from their own cloud, not from the
// owner's browser: the thing that must authenticate a caller is a public
// HTTPS endpoint, and the only credential either vendor's connector UI can
// send is whatever a normal OAuth 2.1 authorization-code-with-PKCE flow hands
// it. They cannot be configured with a static bearer header, so
// `GRIMOIRE_MCP_TOKEN` (see internal/mcp) does not reach this case at all —
// this package is what makes a connector-shaped client usable without one.
//
// The design leans on a fact those two vendors don't share with a typical
// public API: the approval step does not have to happen in their UI. Nothing
// in the OAuth authorization-code flow requires the authorization endpoint to
// be reachable from the same network as the client — only the browser doing
// the redirect has to reach it. So the /oauth/authorize page that actually
// grants access is never exposed on the public listener at all; it lives on
// a second, tailnet-only listener, and the owner's own browser is the only
// browser that ever loads it. See server.go for the split.
//
// Tokens are opaque random values, stored hashed — the same idiom
// internal/auth and internal/secrets already use for sessions, API keys and
// grants, kept here rather than introduced fresh.
package oauth

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"time"
)

// Now is overridden in tests that need to move expiry around without
// sleeping.
var Now = time.Now

// Scopes gate which MCP tools a token may call. There are exactly four,
// matching the shape of what grimoire-mcp exposes: read vs write on notes,
// the separate agent-memory surface, and the credential broker, which is
// kept apart from everything else because it is the one scope that can move
// money or call another service on the owner's behalf. See internal/mcp's
// toolScope map for which tool needs which.
const (
	ScopeNotesRead   = "notes:read"
	ScopeNotesWrite  = "notes:write"
	ScopeMemory      = "memory"
	ScopeCredentials = "credentials"
)

// AllScopes is the full set, in the order presented on the consent page.
var AllScopes = []string{ScopeNotesRead, ScopeNotesWrite, ScopeMemory, ScopeCredentials}

// DefaultScopes are what a web connector is granted when the owner approves
// without touching the checkboxes. Credentials is deliberately excluded: the
// broker can spend the owner's other credentials, and a connector should
// never receive that reach just because it was the first thing offered.
var DefaultScopes = []string{ScopeNotesRead, ScopeNotesWrite, ScopeMemory}

// ValidScope reports whether s is one of the four scopes this server knows.
func ValidScope(s string) bool {
	for _, v := range AllScopes {
		if v == s {
			return true
		}
	}
	return false
}

// ValidateScopes filters requested down to the known scopes, deduplicated,
// order preserved. Unknown scope strings are dropped rather than rejected —
// a client asking for a scope this server never advertised is not an attack,
// and refusing outright would break a client that sends one extra scope this
// server doesn't have a tool for yet.
func ValidateScopes(requested []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(requested))
	for _, s := range requested {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] || !ValidScope(s) {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// splitScope parses the space-separated scope string OAuth wire formats use.
func splitScope(s string) []string {
	return ValidateScopes(strings.Fields(s))
}

// joinScope is splitScope's inverse, for storage and the token response.
func joinScope(scopes []string) string {
	return strings.Join(scopes, " ")
}

// randomToken returns a URL-safe opaque token. 32 bytes matches the entropy
// internal/secrets already uses for grant tokens.
func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// scopeGranted reports whether need is present in granted.
func scopeGranted(granted []string, need string) bool {
	for _, g := range granted {
		if g == need {
			return true
		}
	}
	return false
}
