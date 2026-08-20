package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/auth"
)

// Accounts, sessions, keys and spaces over HTTP.
//
// Every route here is a no-op-shaped thing on a single-user deployment: /api/me
// reports an unrestricted local caller, and the rest refuse politely until
// somebody creates the first account. That is deliberate — the console can ask
// "who am I" unconditionally and render either world from the answer.

func (s *Server) authRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/me", s.whoami)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/logout", s.logout)
	mux.HandleFunc("POST /api/auth/password", s.changeOwnPassword)

	mux.HandleFunc("GET /api/keys", s.listKeys)
	mux.HandleFunc("POST /api/keys", s.createKey)
	mux.HandleFunc("DELETE /api/keys/{id}", s.revokeKey)

	mux.HandleFunc("GET /api/users", s.listUsers)
	mux.HandleFunc("POST /api/users", s.createUser)
	mux.HandleFunc("PUT /api/users/{id}", s.updateUser)
	mux.HandleFunc("DELETE /api/users/{id}", s.deleteUser)

	mux.HandleFunc("GET /api/spaces", s.listSpaces)
	mux.HandleFunc("POST /api/spaces", s.createSpace)
	mux.HandleFunc("DELETE /api/spaces/{id}", s.deleteSpace)
	mux.HandleFunc("GET /api/spaces/{id}/members", s.listMembers)
	mux.HandleFunc("POST /api/spaces/{id}/members", s.addMember)
	mux.HandleFunc("DELETE /api/spaces/{id}/members/{user}", s.removeMember)
}

// whoami is what the console loads first: it decides whether to show a login
// screen, an admin menu, or neither.
func (s *Server) whoami(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	out := map[string]any{
		"multi_user": s.Auth != nil && s.Auth.Enabled(),
		"anonymous":  p.Anonymous,
		"admin":      p.IsAdmin(),
		"name":       p.Name(),
	}
	if !p.Anonymous && !p.Unrestricted {
		out["user"] = p.User
		spaces := []map[string]any{}
		all, err := s.Auth.Spaces()
		if err == nil {
			for _, sp := range all {
				if p.CanRead(sp.ID) {
					spaces = append(spaces, map[string]any{
						"id": sp.ID, "name": sp.Name, "prefix": sp.Prefix,
						"kind": sp.Kind, "writable": p.CanWrite(sp.ID)})
				}
			}
		}
		out["spaces"] = spaces
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil || !s.Auth.Enabled() {
		writeErr(w, http.StatusBadRequest, "this instance has no accounts")
		return
	}
	var in struct{ Name, Password string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, err := s.Auth.Authenticate(in.Name, in.Password)
	if err != nil {
		// One message for both "no such account" and "wrong password": the
		// difference is exactly what an attacker enumerating names wants.
		writeErr(w, http.StatusUnauthorized, "wrong name or password")
		return
	}
	token, err := s.Auth.StartSession(u.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
		MaxAge: int(auth.SessionTTL.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"user": u})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && s.Auth != nil {
		_ = s.Auth.EndSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	var in struct{ Current, New string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	p := principal(r)
	if _, err := s.Auth.Authenticate(p.User.Name, in.Current); err != nil {
		writeErr(w, http.StatusUnauthorized, "wrong password")
		return
	}
	if err := s.Auth.SetPassword(p.User.ID, in.New); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Changing a password ends every session, including this one.
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sessions_ended": true})
}

// ------------------------------------------------------------------- keys

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	keys, err := s.Auth.ListAPIKeys(principal(r).User.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if keys == nil {
		keys = []auth.APIKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// createKey returns the key value exactly once. It is stored hashed, so there
// is no later opportunity to show it — which is the property that makes a leaked
// index harmless.
func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	var in struct{ Label string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	key, rec, err := s.Auth.CreateAPIKey(principal(r).User.ID, in.Label)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"key": key, "record": rec,
		"note": "this value is shown once and cannot be recovered"})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if err := s.Auth.RevokeAPIKey(principal(r).User.ID, r.PathValue("id")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ------------------------------------------------------------------ users

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	users, err := s.Auth.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if users == nil {
		users = []auth.User{}
	}
	writeJSON(w, http.StatusOK, users)
}

// createUser is the one route that is open on a fresh instance: creating the
// first account is what turns multi-user on, and there is nobody to authorize
// it yet. Once an account exists it is administrators only.
func (s *Server) createUser(w http.ResponseWriter, r *http.Request) {
	first := s.Auth != nil && !s.Auth.Enabled()
	if !first && !s.requireAdmin(w, r) {
		return
	}
	if s.Auth == nil {
		writeErr(w, http.StatusNotImplemented, "accounts are unavailable")
		return
	}
	var in struct{ Name, Display, Password, Role string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, err := s.Auth.Create(in.Name, in.Display, in.Password, in.Role)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Every account gets somewhere of its own to write.
	if _, err := s.Auth.EnsurePersonalSpace(u); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// New spaces change which rows are readable, and rows carry their space.
	s.reindexSpaces()
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct{ Role, Password string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	id := r.PathValue("id")
	if in.Role != "" {
		if err := s.Auth.SetRole(id, in.Role); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if in.Password != "" {
		if err := s.Auth.SetPassword(id, in.Password); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	u, err := s.Auth.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.Auth.Delete(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ----------------------------------------------------------------- spaces

func (s *Server) listSpaces(w http.ResponseWriter, r *http.Request) {
	if s.Auth == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	all, err := s.Auth.Spaces()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := principal(r)
	out := []map[string]any{}
	for _, sp := range all {
		if !p.CanRead(sp.ID) {
			continue // a space you cannot read is not a space you know about
		}
		out = append(out, map[string]any{"id": sp.ID, "name": sp.Name,
			"prefix": sp.Prefix, "kind": sp.Kind, "owner": sp.Owner,
			"created": sp.Created, "writable": p.CanWrite(sp.ID)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createSpace(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct{ Name, Prefix string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.HasPrefix(auth.NormalizePrefix(in.Prefix), auth.PersonalPrefix) {
		writeErr(w, http.StatusBadRequest, "users/ is reserved for personal spaces")
		return
	}
	sp, err := s.Auth.CreateSpace(in.Name, in.Prefix, auth.KindShared, "")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.reindexSpaces()
	writeJSON(w, http.StatusCreated, sp)
}

func (s *Server) deleteSpace(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.Auth.DeleteSpace(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.reindexSpaces()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	members, err := s.Auth.Members(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if members == nil {
		members = []auth.Member{}
	}
	writeJSON(w, http.StatusOK, members)
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var in struct{ User, Role string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	u, err := s.Auth.ByName(in.User)
	if err != nil {
		if u, err = s.Auth.Get(in.User); err != nil {
			writeErr(w, http.StatusNotFound, "no such user")
			return
		}
	}
	if err := s.Auth.AddMember(r.PathValue("id"), u.ID, in.Role); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.Auth.RemoveMember(r.PathValue("id"), r.PathValue("user")); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// reindexSpaces re-stamps every row with the space its path now belongs to.
//
// Adding or removing a space changes which rows are visible to whom, and rows
// carry their space so ranking can filter without a join. Recomputing it in
// SQL keeps the two definitions from drifting — the alternative, re-reading
// and re-embedding the vault, would cost minutes and change nothing else.
func (s *Server) reindexSpaces() {
	if s.Auth == nil {
		return
	}
	s.forgetSpaces()
	spaces, err := s.Auth.Spaces()
	if err != nil {
		return
	}
	if err := s.Index.RestampSpaces(func(path string) string {
		return auth.SpaceOf(path, spaces)
	}); err != nil {
		return
	}
}
