package api

import (
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/attest"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Evidence verification at write time.
//
// An agent that writes `evidence` is claiming it read the fact from those
// places. attest checks the claim against the store and the write path records
// the outcome as memory.Entry.Verification. The evidence is kept either way: a
// reference that does not resolve is still what the agent said it used, and
// dropping it would hide the claim from the person who reviews it. What changes
// is the basis: unverified evidence cannot make a fact `observed`.
//
// Every lookup goes through the caller's own access rules, so a check cannot be
// used to learn that a restricted note or document exists.

// pulledURLScanLimit bounds how many pulled notes one URL check reads. A URL
// check reads note bodies, which is the only place a pulled URL is recorded;
// past this bound the check reports the URL as not fetched rather than scanning
// an unbounded vault on a write.
const pulledURLScanLimit = 5000

// attestChecker resolves evidence for this caller.
func (s *Server) attestChecker(r *http.Request) attest.Checker {
	return attest.Checker{
		Note: func(rel string) (string, bool) {
			rel = normPath(rel)
			if s.Vault == nil || !s.canRead(r, rel) {
				return "", false
			}
			n, err := s.Vault.Read(rel)
			if err != nil {
				return "", false
			}
			return n.Body, true
		},
		Hash:    func(hex string) bool { return s.storedHashVisible(r, hex) },
		Fetched: func(u string) bool { return s.pulledURLVisible(r, u) },
		Entry:   func(id string) bool { return s.entryVisible(r, id) },
	}
}

// storedHashVisible reports whether a visible imported document has this
// content hash (sha256 of its original bytes, as documents.Store records it).
//
// Connector items are deliberately not matched here. Their stored hash is a
// truncated digest over a composite of fields plus a trust suffix, so it is not
// the hash of any content a person could cite, and matching it would make the
// check look broader than it is.
func (s *Server) storedHashVisible(r *http.Request, hex string) bool {
	if s.Documents == nil {
		return false
	}
	recs, err := s.Documents.List()
	if err != nil {
		return false
	}
	for _, rec := range recs {
		if rec.Hash == hex && s.canRead(r, normPath(rec.Path)) {
			return true
		}
	}
	return false
}

// queryStrings reads the first column of a query into a slice and closes the
// rows before returning. Access checks run after this, never inside a row
// loop: the database is a single connection, and a query nested in an open
// result set waits on itself.
func (s *Server) queryStrings(query string, args ...any) []string {
	rows, err := s.Index.DB.Query(query, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if rows.Scan(&v) == nil {
			out = append(out, v)
		}
	}
	return out
}

// pulledURLVisible reports whether a URL appears in the text of a note the
// store pulled from elsewhere (a note with an origin). Trust is derived from the
// origin, so a note with one is exactly the population that was fetched.
func (s *Server) pulledURLVisible(r *http.Request, u string) bool {
	if s.Index == nil || s.Index.DB == nil || s.Vault == nil {
		return false
	}
	paths := s.queryStrings(
		"SELECT path FROM notes WHERE origin <> '' LIMIT ?", pulledURLScanLimit)
	for _, p := range paths {
		if !s.canRead(r, p) {
			continue
		}
		if n, err := s.Vault.Read(p); err == nil && strings.Contains(n.Raw, u) {
			return true
		}
	}
	return false
}

// entryVisible reports whether a memory entry with this id exists and the
// caller may read the note it lives in.
func (s *Server) entryVisible(r *http.Request, id string) bool {
	if s.Index == nil || s.Index.DB == nil {
		return false
	}
	rows, err := s.Index.DB.Query("SELECT note, acl FROM memory_entries WHERE id=?", id)
	if err != nil {
		return false
	}
	type where struct{ note, acl string }
	var found []where
	for rows.Next() {
		var w where
		if rows.Scan(&w.note, &w.acl) == nil {
			found = append(found, w)
		}
	}
	rows.Close()
	for _, w := range found {
		if s.canReadNote(r, w.note, w.acl) {
			return true
		}
	}
	return false
}

// verifyEvidence is the write path's verdict on a fact's evidence: "" when it
// cites nothing, otherwise memory.VerifiedEvidence or memory.UnverifiedEvidence.
func (s *Server) verifyEvidence(r *http.Request, evidence []string) string {
	if len(evidence) == 0 {
		return ""
	}
	switch attest.Status(s.attestChecker(r).Verify(evidence)) {
	case "verified":
		return memory.VerifiedEvidence
	case "unverified":
		return memory.UnverifiedEvidence
	}
	return ""
}
