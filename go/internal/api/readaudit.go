package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/auth"
	"github.com/JeremiahM37/grimoire/go/internal/readlog"
)

// auditRead records an attempt to open one restricted document.
//
// Called at the single-document boundaries only — the note route, the
// attachment route and the vault export, which are the three ways a document's
// bytes leave this server. Search is deliberately not audited; see the package
// comment on readlog.
//
// Must not be called while a result set is open, because it may read the
// note's reader list. Every current caller holds a materialized slice.
func (s *Server) auditRead(r *http.Request, path string, allowed bool) {
	if s.Reads == nil {
		return
	}
	c := callerOf(r)
	if !c.enabled {
		// No accounts: nothing is restricted, and there is nobody to audit.
		return
	}
	space := c.spaceOf(path)
	acl := ""
	if allowed {
		// A denied read already failed a check; asking the database again for
		// a reader list it may not even have is pointless work on the path an
		// attacker controls. Treat every denial as restricted, because it is.
		acl = s.aclOf(r, path)
		if strings.TrimSpace(acl) == "" && space == auth.CommonsID {
			return // an ordinary note everyone can read
		}
	}
	p := c.principal
	ev := readlog.Event{
		Path:    path,
		Space:   space,
		Allowed: allowed,
		Route:   r.Method + " " + r.URL.Path,
		Addr:    clientAddr(r),
	}
	if !p.Anonymous && !p.Unrestricted {
		ev.User, ev.Name = p.User.ID, p.User.Name
	}
	s.Reads.Record(ev)
}

// readAudit lists recorded reads. Administrators only: the trail is a record
// of which people opened which sensitive documents, which is itself among the
// more sensitive things this server holds.
func (s *Server) readAudit(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	q := readlog.Query{
		Path:   r.URL.Query().Get("path"),
		User:   r.URL.Query().Get("user"),
		Denied: r.URL.Query().Get("denied") == "1",
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil {
		q.Limit = n
	}
	rows, err := s.Reads.Recent(q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []readlog.Row{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reads": rows, "dropped": s.Reads.Dropped(), "suppressed": s.Reads.Suppressed()})
}
