package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

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
	// Who read it, as opposed to on whose account. agentFor prefers a verified
	// network identity over the name the caller sent, so on a tailnet this is
	// the machine rather than a string an agent chose for itself.
	ev.Agent = agentFor(r)
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

// GET /api/admin/reads/anomalies — the audit trail read back.
//
// The trail was written for the incident and, like every audit log, was never
// queried. This is the query, offered where an operator already looks. See
// internal/readlog/anomaly.go for why breadth rather than depth is the signal,
// and why this reports rather than alerts.
func (s *Server) readAnomalies(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	opt := readlog.Options{OnlyUser: strings.TrimSpace(r.URL.Query().Get("user"))}
	for name, dst := range map[string]*int{
		"breadth": &opt.Breadth,
		"denials": &opt.Denials,
	} {
		if v := strings.TrimSpace(r.URL.Query().Get(name)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				writeErr(w, http.StatusBadRequest, name+" must be a positive number")
				return
			}
			*dst = n
		}
	}
	if v := strings.TrimSpace(r.URL.Query().Get("window")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			writeErr(w, http.StatusBadRequest, "window must be a duration like 5m")
			return
		}
		opt.Window = d
	}
	if v := strings.TrimSpace(r.URL.Query().Get("since")); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "since must be RFC3339")
			return
		}
		opt.Since = t
	}

	// Anything buffered but not yet written would otherwise be invisible to a
	// scan run seconds after the reads it is about — which is exactly when an
	// operator runs one.
	s.Reads.Flush()

	found, err := s.Reads.Anomalies(opt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if found == nil {
		found = []readlog.Anomaly{}
	}
	opts := opt
	writeJSON(w, http.StatusOK, map[string]any{
		"anomalies": found,
		// The thresholds the answer was computed with, echoed back. A caller
		// shown "3 anomalies" cannot judge them without knowing what counted
		// as one, and a UI that hardcoded its own copy of the defaults would
		// drift from the server's the first time either changed.
		"window":  opts.WithDefaultsPublic().Window.String(),
		"breadth": opts.WithDefaultsPublic().Breadth,
		"denials": opts.WithDefaultsPublic().Denials,
		// Whether the trail can answer at all. On a single-user instance
		// nothing is restricted, so nothing is ever recorded — and an empty
		// answer there means "not applicable", not "all clear".
		"records": s.Reads.Count(),
	})
}
