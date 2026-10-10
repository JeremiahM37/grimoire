package memory

import (
	"fmt"
	"strings"
	"time"
)

// Validity is when a fact was true IN THE WORLD, as opposed to when the store
// believed it (Stamp / SupersededAt) — the second axis of the bi-temporal
// model. ValidFrom is inclusive and ValidTo exclusive, so a fact is true on
// [ValidFrom, ValidTo). An empty bound is unbounded on that side, and a fact
// with neither bound is always valid.
//
// A bound is either RFC3339 or a calendar date (YYYY-MM-DD). A date means
// midnight UTC at its start: validity is a statement about the world, and the
// server's local timezone must not change which facts answer a question.

// dateLayout is the calendar-date form of a validity bound.
const dateLayout = "2006-01-02"

// ParseValidity reads a validity bound. It reports false for an empty or
// unparseable value; callers treat that as "no bound", which is the only
// reading that keeps a hand-edited bullet from hiding facts.
func ParseValidity(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(dateLayout, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// NormValidity is the canonical stored form of a validity bound supplied by a
// caller: RFC3339 in UTC, to the second. Empty stays empty. A value that is
// neither RFC3339 nor a date is an error, because a caller who wrote one
// meant a bound and a silently-unbounded fact is the wrong answer.
func NormValidity(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	t, ok := ParseValidity(s)
	if !ok {
		return "", fmt.Errorf("%q is not an RFC3339 time or a YYYY-MM-DD date", s)
	}
	return FormatValidity(t), nil
}

// FormatValidity renders an instant in the canonical validity form. Canonical
// strings are fixed-width and UTC, so the index can compare them as text.
func FormatValidity(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ValidAt reports whether this fact was true at an instant in the world.
func (e Entry) ValidAt(t time.Time) bool { return e.ValidAtAsOf(t, time.Time{}) }

// ValidDuring reports whether this fact was true at some instant in
// [since, until]. A zero bound is open on that side. The check is interval
// overlap, not containment: a fact true for one day inside a year-long range
// matches the range.
func (e Entry) ValidDuring(since, until time.Time) bool {
	return e.ValidDuringAsOf(since, until, time.Time{})
}

// ValidAtAsOf is ValidAt as it would have been answered at belief instant
// asOf. A zero asOf means now.
//
// The belief axis matters here because a supersession CLOSES the old fact's
// valid_to at the new fact's valid_from, and that closure is written into the
// old bullet with no time of its own. At asOf, a fact still standing in the
// store's belief had not yet been closed, so its valid_to is unknown and is
// not applied. Applying it would answer "what did we believe about March on
// Aug 13" with a bound learned on Aug 15.
func (e Entry) ValidAtAsOf(t, asOf time.Time) bool {
	if from, ok := ParseValidity(e.ValidFrom); ok && t.Before(from) {
		return false
	}
	if to, ok := e.validTo(asOf); ok && !t.Before(to) {
		return false
	}
	return true
}

// ValidDuringAsOf is ValidDuring as it would have been answered at belief
// instant asOf. A zero asOf means now. See ValidAtAsOf for the belief rule.
func (e Entry) ValidDuringAsOf(since, until, asOf time.Time) bool {
	if from, ok := ParseValidity(e.ValidFrom); ok && !until.IsZero() && from.After(until) {
		return false
	}
	if to, ok := e.validTo(asOf); ok && !since.IsZero() && !to.After(since) {
		return false
	}
	return true
}

// validTo is the upper validity bound as known at belief instant asOf. A zero
// asOf is "now", where every recorded bound is known.
func (e Entry) validTo(asOf time.Time) (time.Time, bool) {
	if !asOf.IsZero() && e.Superseded() {
		if replaced, ok := parseStamp(e.SupersededAt); ok && replaced.After(asOf) {
			return time.Time{}, false
		}
	}
	return ParseValidity(e.ValidTo)
}

// CloseValidity ends a superseded fact's validity where its replacement's
// begins. It reports whether it changed anything.
//
// It never overwrites a valid_to already present, because that is either a
// person's claim or an earlier closure, and both outrank an inference. It also
// does nothing when the replacement starts no later than the old fact did: that
// would produce an empty or inverted interval, which is a history correction
// the caller has to make explicitly rather than one this rule can guess.
// replacementFrom must already be in canonical form (see NormValidity).
func CloseValidity(old *Entry, replacementFrom string) bool {
	if old.ValidTo != "" {
		return false
	}
	from, ok := ParseValidity(replacementFrom)
	if !ok {
		return false
	}
	if start, ok := ParseValidity(old.ValidFrom); ok && !from.After(start) {
		return false
	}
	old.ValidTo = replacementFrom
	return true
}
