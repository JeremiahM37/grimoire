package api

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// History, rollback and the operational view of stored credentials.
//
// A secrets store that can only be written to is a store people are afraid to
// rotate, and a credential nobody dares rotate is the one that leaks. These
// routes exist so the two operations a person actually performs — "put the new
// key in" and "no, put it back" — are both available, and so the questions
// that decide when to do either (what expires, what nothing has used) can be
// answered without reading any value.

// secretDetails lists secrets with everything except their values.
//
// A separate route from GET /api/secrets, which stays a bare list of names.
// The old shape is what the console and every existing script read, and
// widening it in place would have started shipping timestamps and use counts
// to callers that asked for names.
func (s *Server) secretDetails(w http.ResponseWriter, r *http.Request) {
	// prefix scopes the listing to one namespace. Whole-segment matched, so
	// "prod" does not select "production/…".
	prefix := r.URL.Query().Get("prefix")
	info, err := s.Secrets.Under(prefix)
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	attention := 0
	for _, i := range info {
		if i.Status != secrets.StatusOK {
			attention++
		}
	}
	groups, _ := s.Secrets.Prefixes()
	writeJSON(w, http.StatusOK, map[string]any{
		"secrets":    info,
		"prefix":     prefix,
		"namespaces": groups,
		// Counted server-side so every surface agrees on the number, and so a
		// client that only wants the badge does not have to understand the
		// status vocabulary.
		"needs_attention":      attention,
		"expiring_within_days": secrets.ExpiringSoonDays,
		"scope": "Values are never included. This is what you need to decide " +
			"what to rotate, not what to use.",
	})
}

// secretVersions lists the retained history for one secret, without values.
func (s *Server) secretVersions(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	vers, err := s.Secrets.Versions(name)
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":     name,
		"versions": vers,
		"retained": secrets.MaxVersions,
	})
}

// restoreSecret makes a previous version current.
func (s *Server) restoreSecret(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Version *int   `json:"version"`
	}
	err := json.NewDecoder(r.Body).Decode(&in)
	name := in.Name
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	if err != nil || in.Version == nil {
		// The most recent previous value is what a rollback almost always
		// means, so it is the default rather than a required argument.
		zero := 0
		in.Version = &zero
	}
	err = s.Secrets.Restore(name, *in.Version)
	if s.vaultLocked(w, err) {
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.Broker.Record("restore", name, "version="+strconv.Itoa(*in.Version))
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name, "restored": *in.Version,
		"note": "The value this replaced is itself retained, so this is undoable.",
	})
}
