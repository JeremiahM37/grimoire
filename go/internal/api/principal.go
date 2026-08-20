package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/auth"
	"github.com/JeremiahM37/grimoire/go/internal/index"
)

// Who is asking, and what they may see.
//
// One rule governs everything here: a deployment with no accounts must behave
// exactly as the single-user server always did — no login, everything visible,
// the optional shared token still honoured. Multi-user starts when the first
// account is created, and every check below reads that state rather than a
// setting, so there is no way to half-enable it.

type principalKey struct{}

// caller is everything a handler needs to answer "may they see this", captured
// once per request.
//
// The space table is resolved in the middleware, not on demand. Resolving it
// on demand meant a database query per note, from inside handlers that were
// already iterating a result set — and with a single SQLite connection, a
// query issued while a cursor is open waits for a connection the cursor holds.
// That is a deadlock, and it is the kind that only appears once real
// authorization exists, which is exactly when it is hardest to debug.
type caller struct {
	principal *auth.Principal
	spaces    []auth.Space
	enabled   bool
}

func (c *caller) spaceOf(path string) string {
	if !c.enabled {
		return auth.CommonsID
	}
	return auth.SpaceOf(path, c.spaces)
}

// sessionCookie carries a browser session.
const sessionCookie = "grimoire_session"

// withPrincipal resolves the caller and puts them in the request context.
func (s *Server) withPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := &caller{principal: s.resolve(r)}
		if s.Auth != nil && s.Auth.Enabled() {
			c.enabled = true
			if spaces, err := s.Auth.Spaces(); err == nil {
				c.spaces = spaces
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, c)))
	})
}

func (s *Server) resolve(r *http.Request) *auth.Principal {
	if s.Auth == nil || !s.Auth.Enabled() {
		return auth.Unrestricted()
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if u, err := s.Auth.UserForSession(c.Value); err == nil {
			if p, err := s.Auth.PrincipalFor(u); err == nil {
				return p
			}
		}
	}
	if key := bearer(r); key != "" {
		if u, err := s.Auth.UserForAPIKey(key); err == nil {
			if p, err := s.Auth.PrincipalFor(u); err == nil {
				return p
			}
		}
	}
	return auth.Anonymous()
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	// Agents configured through an MCP client often carry the key in a header
	// of their own; accepting both saves a class of "it works in curl" bugs.
	return strings.TrimSpace(r.Header.Get("X-Grimoire-Key"))
}

// callerOf returns the request's captured caller. Handlers never construct one
// themselves, so a missing middleware fails closed rather than silently
// granting access.
func callerOf(r *http.Request) *caller {
	if c, ok := r.Context().Value(principalKey{}).(*caller); ok && c != nil {
		return c
	}
	return &caller{principal: auth.Anonymous(), enabled: true}
}

// principal returns who is asking.
func principal(r *http.Request) *auth.Principal { return callerOf(r).principal }

// filterFor is what this caller may retrieve.
func filterFor(r *http.Request, includePrivate bool) index.Filter {
	return index.Filter{IncludePrivate: includePrivate, Spaces: principal(r).ReadableSpaces()}
}

// SpaceOf implements index.Spaces, so rows are written with their space.
//
// Called by the indexer at write time rather than by a handler, so it reads the
// space table itself — behind a short-lived snapshot, because a write may touch
// thousands of notes and each one would otherwise be a query. It must not be
// called while a result set is open; see the note on caller.
func (s *Server) SpaceOf(path string) string {
	if s.Auth == nil {
		return auth.CommonsID
	}
	s.spaceMu.Lock()
	if time.Since(s.spaceAt) > spaceSnapshotTTL {
		s.spaceAt = time.Now()
		s.spaceEnabled = s.Auth.Enabled()
		if s.spaceEnabled {
			if spaces, err := s.Auth.Spaces(); err == nil {
				s.spaceList = spaces
			}
		} else {
			s.spaceList = nil
		}
	}
	enabled, spaces := s.spaceEnabled, s.spaceList
	s.spaceMu.Unlock()
	if !enabled {
		return auth.CommonsID
	}
	return auth.SpaceOf(path, spaces)
}

// forgetSpaces drops the indexer's snapshot after the space table changes.
func (s *Server) forgetSpaces() {
	s.spaceMu.Lock()
	s.spaceAt = time.Time{}
	s.spaceMu.Unlock()
}

// spaceSnapshotTTL bounds how stale the indexer's view of the space table can
// be when it is changed out of band — by the CLI, or by another process.
const spaceSnapshotTTL = 3 * time.Second

// canRead reports whether the caller may see a note.
func (s *Server) canRead(r *http.Request, path string) bool {
	c := callerOf(r)
	return c.principal.CanRead(c.spaceOf(path))
}

// canWrite reports whether the caller may change a note.
func (s *Server) canWrite(r *http.Request, path string) bool {
	c := callerOf(r)
	return c.principal.CanWrite(c.spaceOf(path))
}

// requireRead ends the request unless the caller may read the path.
//
// A note in a space the caller cannot see is reported as absent rather than
// forbidden: "you may not read this" confirms that a note by that exact name
// exists, which is often the sensitive part.
func (s *Server) requireRead(w http.ResponseWriter, r *http.Request, path string) bool {
	if s.canRead(r, path) {
		return true
	}
	writeErr(w, http.StatusNotFound, "no such note")
	return false
}

// requireWrite ends the request unless the caller may write the path.
func (s *Server) requireWrite(w http.ResponseWriter, r *http.Request, path string) bool {
	if s.canWrite(r, path) {
		return true
	}
	if principal(r).Anonymous {
		writeErr(w, http.StatusUnauthorized, "sign in first")
		return false
	}
	if !s.canRead(r, path) {
		writeErr(w, http.StatusNotFound, "no such note")
		return false
	}
	writeErr(w, http.StatusForbidden, "you have read-only access to that space")
	return false
}

// requireAdmin gates instance administration: accounts, spaces, connectors and
// the credential vault.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	p := principal(r)
	if p.IsAdmin() {
		return true
	}
	if p.Anonymous {
		writeErr(w, http.StatusUnauthorized, "sign in first")
		return false
	}
	writeErr(w, http.StatusForbidden, "administrators only")
	return false
}

// requireUser gates anything a signed-in caller may do on a multi-user
// deployment, and everyone may do on a single-user one.
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) bool {
	if !principal(r).Anonymous {
		return true
	}
	writeErr(w, http.StatusUnauthorized, "sign in first")
	return false
}

// spaceSQL restricts a query to the caller's readable spaces.
//
// Returns an empty fragment when the caller may read everything — a single-user
// deployment or an administrator — so those queries stay exactly as they were.
// For a caller with no readable spaces at all it returns a clause that matches
// nothing, because "no spaces" and "no restriction" must never collapse into
// the same SQL.
func (s *Server) spaceSQL(r *http.Request, col string) (string, []any) {
	allowed := principal(r).ReadableSpaces()
	if allowed == nil {
		return "", nil
	}
	names := make([]string, 0, len(allowed))
	for name, ok := range allowed {
		if ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "0", nil
	}
	args := make([]any, 0, len(names))
	ph := make([]string, 0, len(names))
	for _, n := range names {
		ph = append(ph, "?")
		args = append(args, n)
	}
	return col + " IN (" + strings.Join(ph, ",") + ")", args
}

// whereSpace composes spaceSQL into a query that may already have a WHERE.
func (s *Server) whereSpace(r *http.Request, col, existing string) (string, []any) {
	clause, args := s.spaceSQL(r, col)
	if clause == "" {
		return existing, nil
	}
	if strings.TrimSpace(existing) == "" {
		return " WHERE " + clause, args
	}
	return existing + " AND " + clause, args
}

// adminOnly wraps a handler so it answers administrators only. Applied at
// registration rather than inside each handler, so the route table itself
// shows which surfaces are administrative.
func (s *Server) adminOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requireAdmin(w, r) {
			return
		}
		h(w, r)
	}
}
