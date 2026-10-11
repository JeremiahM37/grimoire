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

// attestChecker resolves evidence for this caller. The pulled-note bodies a URL
// check reads are loaded once per checker, however many URLs and facts one
// request cites.
func (s *Server) attestChecker(r *http.Request) attest.Checker {
	var pulled []string
	loaded := false
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
		Hash: func(hex string) bool { return s.storedHashVisible(r, hex) },
		Fetched: func(u string) bool {
			if !loaded {
				pulled, loaded = s.pulledBodies(r), true
			}
			for _, raw := range pulled {
				if containsURL(raw, u) {
					return true
				}
			}
			return false
		},
		Entry: func(id string) bool { return s.entryVisible(r, id) },
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

// pulledBodies returns the text of every note the caller may read that the
// store pulled from elsewhere (a note with an origin), bounded by
// pulledURLScanLimit. Trust is derived from the origin, so a note with one is
// exactly the population that was fetched.
func (s *Server) pulledBodies(r *http.Request) []string {
	if s.Index == nil || s.Index.DB == nil || s.Vault == nil {
		return nil
	}
	paths := s.queryStrings(
		"SELECT path FROM notes WHERE origin <> '' LIMIT ?", pulledURLScanLimit)
	var out []string
	for _, p := range paths {
		if !s.canRead(r, p) {
			continue
		}
		if n, err := s.Vault.Read(p); err == nil {
			out = append(out, n.Raw)
		}
	}
	return out
}

// containsURL reports whether raw contains u as a whole URL, not as the prefix
// of a longer one: "https://x.com" is not evidence that "https://x.com/a/b"
// was fetched, and the other way round is only true up to sentence punctuation.
func containsURL(raw, u string) bool {
	if u == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(raw[from:], u)
		if i < 0 {
			return false
		}
		end := from + i + len(u)
		if end >= len(raw) || !urlContinues(raw, end) {
			return true
		}
		from += i + 1
	}
}

// urlContinues reports whether the byte at raw[i] extends a URL that ended at
// i. A trailing "." or "," only extends it when a URL character follows.
func urlContinues(raw string, i int) bool {
	c := raw[i]
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case strings.IndexByte("/_%~#?&=-+:@", c) >= 0:
		return true
	case c == '.' || c == ',':
		return i+1 < len(raw) && urlContinues(raw, i+1) && raw[i+1] != '.' && raw[i+1] != ','
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
