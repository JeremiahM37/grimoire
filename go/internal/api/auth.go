package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Authentication for the HTTP surface.
//
// README and SECURITY.md have documented GRIMOIRE_AUTH_TOKEN as the optional
// gate on the API and console since before the Go rewrite. The rewrite did not
// carry it over, so setting it did nothing: every route — including
// /api/vault/unlock, /api/secrets and /api/grants — answered anyone who could
// reach the port. A documented control that silently does nothing is worse
// than an absent one, because it is deployed in place of a real gate.
//
// It remains OPTIONAL and empty-means-open, as documented: the primary gate is
// meant to be an authenticated reverse proxy. This is the second factor.

// authCookie carries the token after a successful ?token= handoff so the web
// console keeps working across the XHRs it makes after the first page load.
const authCookie = "grimoire_auth"

// requireAuth gates every route except health.
//
// Comparison is constant-time over SHA-256 digests rather than over the raw
// strings: fixed-width inputs mean the comparison cannot leak the token's
// length, which a direct subtle.ConstantTimeCompare of variable-length strings
// would.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	if s.AuthToken == "" {
		return next // documented default: empty token means open
	}
	want := sha256.Sum256([]byte(s.AuthToken))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health stays open so a proxy or uptime check does not need the
		// credential; it reveals nothing but liveness.
		if r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		presented, fromQuery := presentedToken(r)
		if presented == "" {
			unauthorized(w)
			return
		}
		got := sha256.Sum256([]byte(presented))
		ok := subtle.ConstantTimeCompare(got[:], want[:]) == 1
		if !ok && s.SyncToken != "" && isPeerRoute(r.URL.Path) {
			// SECURITY.md says the sync token authenticates a peer. Until the
			// gate existed the receiving side validated nothing, so it
			// authenticated no one. It is accepted only on the routes a peer
			// actually calls: a sync credential shared with another machine
			// should not also unlock the secret vault.
			sync := sha256.Sum256([]byte(s.SyncToken))
			ok = subtle.ConstantTimeCompare(got[:], sync[:]) == 1
		}
		if !ok {
			unauthorized(w)
			return
		}
		if fromQuery {
			// Promote a URL token to a cookie once, so it stops travelling in
			// URLs (and therefore in proxy logs and Referer headers) for the
			// rest of the session. SameSite=Strict is what keeps the cookie
			// from turning every state-changing route into a CSRF target.
			http.SetCookie(w, &http.Cookie{
				Name:     authCookie,
				Value:    presented,
				Path:     "/",
				HttpOnly: true,
				Secure:   r.TLS != nil,
				SameSite: http.SameSiteStrictMode,
			})
		}
		next.ServeHTTP(w, r)
	})
}

// peerRoutes are the endpoints a sync peer calls on this node. Everything else
// is local-only and requires the full auth token.
var peerRoutes = []string{
	"/api/sync/manifest",
	"/api/sync/pull",
	"/api/sync/push",
	"/api/crdt/merge",
	"/api/crdt/doc/",
}

func isPeerRoute(path string) bool {
	for _, p := range peerRoutes {
		if path == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(path, p)) {
			return true
		}
	}
	return false
}

// presentedToken pulls the credential from the header, then the cookie, then
// the query string, and reports whether it came from the query.
func presentedToken(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		if rest, ok := cutBearer(h); ok {
			return rest, false
		}
	}
	if c, err := r.Cookie(authCookie); err == nil && c.Value != "" {
		return c.Value, false
	}
	if q := r.URL.Query().Get("token"); q != "" {
		return q, true
	}
	return "", false
}

// cutBearer accepts the scheme case-insensitively, as RFC 7235 requires.
func cutBearer(header string) (string, bool) {
	const scheme = "bearer "
	if len(header) < len(scheme) || !strings.EqualFold(header[:len(scheme)], scheme) {
		return "", false
	}
	return strings.TrimSpace(header[len(scheme):]), true
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="grimoire"`)
	writeErr(w, http.StatusUnauthorized, "authentication required")
}
