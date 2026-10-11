// Package conflict scores the sides of a disagreement so a person adjudicating
// one can see which claim the evidence favours, and why.
//
// It never decides. A dispute is settled by a person (see docs/DISPUTES.md), and
// nothing here writes, supersedes or retracts. The score is a suggestion with
// its reasons attached, so the adjudicator can disagree with it term by term.
//
// Each side is scored on five terms, each in [0,1] and weighted:
//
//	authority     human 1.0, agent 0.5, pulled 0.0
//	verification  verified 1.0, no evidence 0.4, unverified 0.0 (see internal/attest)
//	corroboration independent origins asserting the same value: 0 extra -> 0,
//	              one extra -> 0.5, two or more -> 1.0
//	recency       how recently the fact became true, decaying over a year
//	reliability   a per-agent value from ReliabilityFn, 0.5 when none is supplied
//
// Human authority is also an ordering rule, not only a weight: a human side
// ranks above every non-human side whatever its total. The lattice says a
// person's fact outranks an agent's, and a score that could outweigh that would
// be the lattice being quietly overruled by arithmetic.
package conflict

import (
	"math"
	"sort"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Weights of the five terms. They sum to 1, so a side with every term at its
// maximum scores 1.
const (
	WeightAuthority     = 0.35
	WeightVerification  = 0.25
	WeightCorroboration = 0.20
	WeightRecency       = 0.05
	WeightReliability   = 0.15
)

// DefaultReliability is used for an agent with no supplied reliability. Half
// is the uninformative value: it neither helps nor hurts an agent nobody has
// measured.
const DefaultReliability = 0.5

// ReliabilityFn supplies a per-agent reliability in [0,1]. Nothing in this
// repository supplies one yet; the interface is here so a measured value can be
// plugged in without changing the scorer.
type ReliabilityFn func(agent string) float64

// Options are the inputs that are not properties of a side. Now anchors the
// recency term; a zero Now makes recency 0 for every side, which keeps a caller
// that has no clock from being scored on one by accident.
type Options struct {
	Now         time.Time
	Reliability ReliabilityFn
}

// Candidate is one side of a disagreement, as the scorer reads it.
type Candidate struct {
	ID        string
	Agent     string
	Authority memory.Authority
	// Verification is memory.VerifiedEvidence, memory.UnverifiedEvidence, or
	// empty when the fact cites nothing.
	Verification string
	// Corroboration is how many independent origins assert this value,
	// counting this side. Values below 1 are read as 1.
	Corroboration int
	// ValidFrom is when the fact became true. Zero means unknown.
	ValidFrom time.Time
	// HopDepth is how many derivations or relays separate the fact from its
	// source (0 for a direct statement). It only breaks an exact tie in Rank,
	// after authority and total, so it never changes a total.
	HopDepth int
}

// Breakdown is the weighted contribution of each term, and their sum. The
// fields are the reasons a person reads: a total is only useful if its parts
// are visible.
type Breakdown struct {
	Authority     float64 `json:"authority"`
	Verification  float64 `json:"verification"`
	Corroboration float64 `json:"corroboration"`
	Recency       float64 `json:"recency"`
	Reliability   float64 `json:"reliability"`
	Total         float64 `json:"total"`
}

// Scored is a candidate with its breakdown.
type Scored struct {
	Candidate Candidate
	Breakdown Breakdown
}

// Score computes the breakdown for one candidate. It is deterministic in its
// inputs: the same candidate and options always give the same numbers.
func Score(c Candidate, o Options) Breakdown {
	var b Breakdown
	b.Authority = WeightAuthority * authorityTerm(c.Authority)
	b.Verification = WeightVerification * verificationTerm(c.Verification)
	b.Corroboration = WeightCorroboration * corroborationTerm(c.Corroboration)
	b.Recency = WeightRecency * recencyTerm(c.ValidFrom, o.Now)
	b.Reliability = WeightReliability * reliabilityTerm(c.Agent, o.Reliability)
	b.Total = b.Authority + b.Verification + b.Corroboration + b.Recency + b.Reliability
	return b
}

// Rank scores every candidate and orders them best first. Ordering is by
// authority first (human above everything else), then by total, then by the
// newer fact, then by id, so two runs over the same input agree exactly.
func Rank(cands []Candidate, o Options) []Scored {
	out := make([]Scored, 0, len(cands))
	for _, c := range cands {
		out = append(out, Scored{Candidate: c, Breakdown: Score(c, o)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Candidate, out[j].Candidate
		ah, bh := a.Authority == memory.AuthorityHuman, b.Authority == memory.AuthorityHuman
		if ah != bh {
			return ah
		}
		if ta, tb := out[i].Breakdown.Total, out[j].Breakdown.Total; ta != tb {
			return ta > tb
		}
		if a.HopDepth != b.HopDepth {
			return a.HopDepth < b.HopDepth
		}
		if !a.ValidFrom.Equal(b.ValidFrom) {
			return a.ValidFrom.After(b.ValidFrom)
		}
		return a.ID < b.ID
	})
	return out
}

func authorityTerm(a memory.Authority) float64 {
	switch a {
	case memory.AuthorityHuman:
		return 1
	case memory.AuthorityPulled:
		return 0
	}
	return 0.5
}

func verificationTerm(v string) float64 {
	switch v {
	case memory.VerifiedEvidence:
		return 1
	case memory.UnverifiedEvidence:
		return 0
	}
	// No evidence is neither proof nor a failed check. It sits between them.
	return 0.4
}

func corroborationTerm(n int) float64 {
	if n < 1 {
		n = 1
	}
	return float64(min(n-1, 2)) / 2
}

func recencyTerm(validFrom, now time.Time) float64 {
	if validFrom.IsZero() || now.IsZero() {
		return 0
	}
	ageDays := now.Sub(validFrom).Hours() / 24
	if ageDays < 0 {
		ageDays = 0 // a fact dated in the future is not more recent than now
	}
	return 1 / (1 + ageDays/365)
}

func reliabilityTerm(agent string, fn ReliabilityFn) float64 {
	r := DefaultReliability
	if fn != nil {
		r = fn(agent)
	}
	if math.IsNaN(r) || r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}
