// Package attest checks that the evidence an agent cites for a fact resolves.
//
// Evidence used to be recorded and never looked at. An agent could write
// `ev=memory/runbook.md` for a fact it never read, and the store would call the
// fact `observed`. This package is the check that sits between the claim and
// the label: each reference is resolved against what the store actually holds,
// and the write path records the outcome (memory.Entry.Verification).
//
// What is verified is existence, not truth:
//
//	note    a vault path exists and the caller may read it; if a fragment
//	        follows '#', that text (or a heading with that slug) is in the note
//	sha256:<hex>  a content hash of a stored document or connector item
//	url     the URL appears in the text of a note the store pulled
//	        (a note with an origin), so it was really fetched
//	<12 hex>      a memory entry id the caller may read
//
// A reference that does not resolve, or that the Checker has no way to
// resolve, is unverified. Nothing here fetches anything or calls a network.
package attest

import (
	"regexp"
	"strings"
)

// Kinds a reference can be classified as.
const (
	KindNote    = "note"
	KindHash    = "hash"
	KindURL     = "url"
	KindEntry   = "entry"
	KindUnknown = "unknown"
)

// Reasons a reference did or did not resolve. The reason is for the reader
// of an explain; the write path only needs OK.
const (
	ReasonOK           = "ok"
	ReasonNoNote       = "no_such_note"  // also a note the caller may not read
	ReasonQuoteMissing = "quote_missing" // the note exists; the fragment is not in it
	ReasonNoHash       = "no_such_hash"
	ReasonNotFetched   = "not_fetched"
	ReasonNoEntry      = "no_such_entry"
	ReasonNotChecked   = "not_checked" // no resolver is wired for this kind
	ReasonUnrecognised = "unrecognised"
	ReasonEmpty        = "empty"
)

// Result is the outcome for one evidence reference, in the order given.
type Result struct {
	Ref    string `json:"ref"`
	Kind   string `json:"kind"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

// Checker supplies what the references are resolved against. Every field is
// optional; a nil field makes that kind ReasonNotChecked, which is unverified.
// Keeping them as functions lets the caller apply its own access rules: the
// API passes a Note that returns false for a note the caller may not read, so
// an unreadable note is reported as absent, the same as requireRead does.
type Checker struct {
	// Note returns the body of a vault note, and false when it does not exist
	// or the caller may not read it.
	Note func(rel string) (body string, ok bool)
	// Hash reports whether a content hash (lower-case hex) belongs to a stored
	// document or connector item.
	Hash func(hex string) bool
	// Fetched reports whether a URL was pulled into the store.
	Fetched func(url string) bool
	// Entry reports whether a memory entry id exists and the caller may read it.
	Entry func(id string) bool
}

var (
	hashRE  = regexp.MustCompile(`^sha256:([0-9a-f]{64})$`)
	entryRE = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// Verify resolves each reference. It returns one Result per non-empty input,
// in order. An empty input yields an empty slice, not an error: a fact with no
// evidence has nothing to verify.
func (c Checker) Verify(evidence []string) []Result {
	out := make([]Result, 0, len(evidence))
	for _, raw := range evidence {
		ref := strings.TrimSpace(raw)
		if ref == "" {
			continue
		}
		out = append(out, c.one(ref))
	}
	return out
}

// Status summarises a set of results for the write path: "verified" when every
// reference resolved, "unverified" when any did not, and "" when there were no
// references at all. The caller stores the string; see memory.Entry.Verification.
func Status(results []Result) string {
	if len(results) == 0 {
		return ""
	}
	for _, r := range results {
		if !r.OK {
			return "unverified"
		}
	}
	return "verified"
}

func (c Checker) one(ref string) Result {
	switch {
	case strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://"):
		// Checked before any '#' split: a URL's fragment is part of the URL.
		return c.url(ref)
	case strings.HasPrefix(ref, "sha256:"):
		return c.hash(ref)
	case entryRE.MatchString(ref):
		return c.entry(ref)
	}
	return c.note(ref)
}

func (c Checker) url(ref string) Result {
	r := Result{Ref: ref, Kind: KindURL}
	switch {
	case c.Fetched == nil:
		r.Reason = ReasonNotChecked
	case c.Fetched(ref):
		r.OK, r.Reason = true, ReasonOK
	default:
		r.Reason = ReasonNotFetched
	}
	return r
}

func (c Checker) hash(ref string) Result {
	r := Result{Ref: ref, Kind: KindHash}
	m := hashRE.FindStringSubmatch(ref)
	switch {
	case m == nil:
		r.Reason = ReasonUnrecognised
	case c.Hash == nil:
		r.Reason = ReasonNotChecked
	case c.Hash(m[1]):
		r.OK, r.Reason = true, ReasonOK
	default:
		r.Reason = ReasonNoHash
	}
	return r
}

func (c Checker) entry(ref string) Result {
	r := Result{Ref: ref, Kind: KindEntry}
	switch {
	case c.Entry == nil:
		r.Reason = ReasonNotChecked
	case c.Entry(ref):
		r.OK, r.Reason = true, ReasonOK
	default:
		r.Reason = ReasonNoEntry
	}
	return r
}

// note resolves a vault path with an optional '#fragment'. The fragment is a
// quote to find in the body, or a heading slug, whichever matches first.
func (c Checker) note(ref string) Result {
	r := Result{Ref: ref, Kind: KindNote}
	path, frag, hasFrag := strings.Cut(ref, "#")
	path = strings.TrimSpace(path)
	if path == "" {
		r.Kind, r.Reason = KindUnknown, ReasonUnrecognised
		return r
	}
	if c.Note == nil {
		r.Reason = ReasonNotChecked
		return r
	}
	body, ok := c.Note(path)
	if !ok {
		r.Reason = ReasonNoNote
		return r
	}
	if !hasFrag || strings.TrimSpace(frag) == "" {
		r.OK, r.Reason = true, ReasonOK
		return r
	}
	if containsQuote(body, frag) || hasHeading(body, frag) {
		r.OK, r.Reason = true, ReasonOK
		return r
	}
	r.Reason = ReasonQuoteMissing
	return r
}

// containsQuote reports whether the quote occurs in the body. Whitespace is
// collapsed on both sides, so a quote that wraps a line still matches; case
// is kept, because a quote is an exact claim about the text.
func containsQuote(body, quote string) bool {
	return strings.Contains(collapse(body), collapse(quote))
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// hasHeading reports whether a markdown heading in the body slugs to frag,
// so a link such as note.md#deploy-steps resolves without the text being quoted.
func hasHeading(body, frag string) bool {
	want := strings.ToLower(strings.TrimSpace(frag))
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(t, "#") {
			continue
		}
		if slug(strings.TrimLeft(t, "#")) == want {
			return true
		}
	}
	return false
}

// slug is the GitHub-style heading anchor: lower case, spaces to hyphens,
// other punctuation dropped.
func slug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case r == ' ' || r == '-':
			b.WriteByte('-')
		case r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127:
			b.WriteRune(r)
		}
	}
	return b.String()
}
