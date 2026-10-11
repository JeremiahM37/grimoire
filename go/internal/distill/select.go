// Package distill selects the stable facts that are safe to bake into a small
// model's weights, and writes the training and held-out data for doing so.
//
// Retrieval answers a question by looking a fact up at the moment it is asked,
// so a fact that changes is corrected by editing a note. Weights do not work
// that way: a fact baked in is recalled without being looked up, and nothing
// removes it when it goes stale. So this package is deliberately conservative.
// It admits a fact only when every check passes, and it records a reason for
// every fact it refuses, so the choice can be audited and diffed later.
//
// Nothing here modifies memory, and nothing here calls a model. Selection and
// example generation are deterministic: the same vault on the same day yields
// the same bytes. docs/PARAMETRIC.md is the recipe and the honest limits.
package distill

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Exclusion reasons. Each excluded fact gets exactly one, the first check it
// failed in the order Select applies them, so counts add up.
const (
	ReasonSealed          = "sealed"
	ReasonPrivate         = "private"
	ReasonSensitive       = "sensitive"
	ReasonSuperseded      = "superseded"
	ReasonExpired         = "expired"
	ReasonValidityEnded   = "validity_ended"
	ReasonImage           = "image"
	ReasonOpenChallenge   = "open_challenge"
	ReasonDisputed        = "disputed"
	ReasonImported        = "imported"
	ReasonUntrusted       = "untrusted_origin"
	ReasonUnverified      = "unverified_evidence"
	ReasonInferred        = "inferred"
	ReasonAgentUnverified = "agent_unverified"
	ReasonNoTimestamp     = "no_timestamp"
	ReasonTooYoung        = "too_young"
	ReasonTooFewUses      = "too_few_uses"
)

// DefaultMinAgeDays is the age a fact must reach before it may be baked in. A
// week-old belief is still being revised; a month-old one has survived some
// use. The CLI default matches.
const DefaultMinAgeDays = 30

// Candidate is one stored fact with the note it lives in and how often it has
// been recalled and used. Uses comes from the index's usage counter, which is
// derived data and resets on reindex; it is passed in so Select stays pure.
type Candidate struct {
	Entry memory.Entry
	Note  string
	Uses  int
}

// Options are the selection thresholds. Now is passed explicitly so a test and
// a rerun on another day can agree about what "old" means.
type Options struct {
	Now        time.Time
	MinAgeDays int
	MinUses    int
}

// Exclusion records why one fact was not selected.
type Exclusion struct {
	ID     string `json:"id"`
	Note   string `json:"note"`
	Reason string `json:"reason"`
}

// Result is the outcome of one selection. Selected and Excluded are sorted by
// note then id, so the same input always produces the same output.
type Result struct {
	Selected []Candidate
	Excluded []Exclusion
	Counts   map[string]int
}

// Select applies the bake-in rules to every candidate. A fact is selected only
// when it is not private, sensitive or sealed; not superseded, expired or past
// its validity; not an image caption; not in an open challenge or dispute; not
// from text other people can write; a human assertion or an agent fact with
// verified evidence; not inferred; at least MinAgeDays old; and, when MinUses
// is set, used at least that often.
func Select(cands []Candidate, opt Options) (Result, error) {
	if opt.Now.IsZero() {
		return Result{}, fmt.Errorf("distill: Options.Now is required")
	}
	if opt.MinAgeDays < 0 || opt.MinUses < 0 {
		return Result{}, fmt.Errorf("distill: thresholds must not be negative")
	}
	sorted := append([]Candidate(nil), cands...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Note != sorted[j].Note {
			return sorted[i].Note < sorted[j].Note
		}
		return sorted[i].Entry.ID < sorted[j].Entry.ID
	})

	// A dispute is unresolved while the challenger is still live. Either side
	// of an open dispute is refused: the challenged fact because someone
	// contests it, and the challenger because it contests something.
	challenged := map[string]bool{}
	for _, c := range sorted {
		if !c.Entry.Superseded() && c.Entry.Challenges != "" {
			challenged[c.Entry.Challenges] = true
		}
	}

	res := Result{Counts: map[string]int{}}
	for _, c := range sorted {
		reason := refusal(c, opt, challenged)
		if reason == "" {
			res.Selected = append(res.Selected, c)
			continue
		}
		res.Excluded = append(res.Excluded, Exclusion{ID: c.Entry.ID, Note: c.Note, Reason: reason})
		res.Counts[reason]++
	}
	return res, nil
}

// refusal returns the first reason a candidate may not be baked in, or "".
// The order is the policy: a hidden fact is reported as hidden even if it is
// also old, because visibility is the reason a person would act on.
func refusal(c Candidate, opt Options, challenged map[string]bool) string {
	e := c.Entry
	switch e.Visibility {
	case memory.VisSealed:
		return ReasonSealed
	case memory.VisPrivate:
		return ReasonPrivate
	case memory.VisSensitive:
		return ReasonSensitive
	}
	if e.Superseded() {
		return ReasonSuperseded
	}
	if e.ExpiredAt(opt.Now) {
		return ReasonExpired
	}
	if t, ok := memory.ParseValidity(e.ValidTo); ok && !opt.Now.Before(t) {
		return ReasonValidityEnded
	}
	if e.Image != "" {
		return ReasonImage
	}
	if e.Challenges != "" {
		return ReasonOpenChallenge
	}
	if challenged[e.ID] {
		return ReasonDisputed
	}
	switch e.Basis() {
	case memory.BasisImported:
		return ReasonImported
	case memory.BasisPulled:
		return ReasonUntrusted
	}
	if e.Untrusted() {
		return ReasonUntrusted
	}
	if e.Verification == memory.UnverifiedEvidence {
		return ReasonUnverified
	}
	if e.Basis() == memory.BasisInferred {
		return ReasonInferred
	}
	if !e.HumanAuthored() && e.Verification != memory.VerifiedEvidence {
		return ReasonAgentUnverified
	}
	written, ok := parseWhen(e.Stamp)
	if !ok {
		return ReasonNoTimestamp
	}
	if opt.Now.Sub(written) < time.Duration(opt.MinAgeDays)*24*time.Hour {
		return ReasonTooYoung
	}
	if opt.MinUses > 0 && c.Uses < opt.MinUses {
		return ReasonTooFewUses
	}
	return ""
}

// parseWhen reads a bullet stamp the way the memory package writes it. Stamps
// are local wall-clock time, so they are read in local time too.
func parseWhen(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation(memory.StampFormat, s, time.Local); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, true
	}
	return time.Time{}, false
}
