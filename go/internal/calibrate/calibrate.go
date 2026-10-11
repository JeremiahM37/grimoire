// Package calibrate learns from what happened to agent memory afterwards.
//
// Retention and authority were both fixed by hand: a 90-day half-life, an
// importance rating, and a lattice that says a person outranks an agent which
// outranks pulled text. Nothing closed the loop from "this fact was later
// contradicted by a person" back to "the agent that wrote it deserves less
// trust", or from "nobody has recalled this in a year" to "this is a fair
// candidate to forget". This package closes that loop, with two deliberate
// limits that the rest of the design depends on:
//
//   - It learns from CORRECTION signals only: a challenge, a settled dispute,
//     a person retracting a fact, and an independent agent re-confirming one.
//     Those are sparse. Most facts are never corrected and never confirmed, and
//     a fact nobody looked at is silence, not agreement. The numbers here are
//     therefore a measure of how often an agent's claims have been corrected,
//     not a measure of whether they are true.
//
//   - Every estimate is a Beta posterior with a weak prior, reported as a
//     conservative lower bound. One event moves the bound a little, not a lot.
//     See docs/CALIBRATION.md for the simulation that says how many events a
//     ranking needs before it stops changing.
//
// The package is pure over snapshots, plus a small persistent counter table
// (store.go) that the existing write paths bump. Nothing here deletes anything;
// retention only orders proposals, and deletion stays a human or policy call.
package calibrate

import "math"

// Prior is Beta(2,2): the equivalent of four pseudo-observations centred on
// one half. It is weak enough that a handful of real events dominate it, and
// strong enough that an agent with no record is not scored at 0 or 1.
const (
	priorAlpha = 2.0
	priorBeta  = 2.0
)

// DefaultZ is the normal quantile used for the lower bound: 1.645 is the 95%
// one-sided bound, so the reported reliability is one a sceptic would accept
// as "at least this good".
const DefaultZ = 1.645

// Event is one correction signal attributed to the agent that wrote a fact.
type Event int

const (
	// Confirmed: an independent agent re-checked the fact and found it still
	// true (the same text written back by a different writer).
	Confirmed Event = iota
	// Challenged: a later write contradicted the fact and could not overwrite
	// it, because the writer's authority was lower. The contest is still open
	// or has been settled in the challenger's favour by means other than a
	// person's decision.
	Challenged
	// SupersededByHuman: a person ruled against the fact in a settled
	// challenge, or replaced it.
	SupersededByHuman
	// Deleted: a person retracted the fact.
	Deleted
)

// Counts is how many events of each kind an agent has accumulated. Both the
// persistent table and a snapshot reduce to this, so they share one formula.
type Counts struct {
	Confirmed         int
	Challenged        int
	SupersededByHuman int
	Deleted           int
}

// Add records one event.
func (c *Counts) Add(ev Event) {
	switch ev {
	case Confirmed:
		c.Confirmed++
	case Challenged:
		c.Challenged++
	case SupersededByHuman:
		c.SupersededByHuman++
	case Deleted:
		c.Deleted++
	}
}

// Good is the number of positive signals (confirmations).
func (c Counts) Good() int { return c.Confirmed }

// Bad is the number of negative signals. Every correction counts once,
// whatever its kind: a deletion is not worse evidence than a challenge, it is
// just a stronger form of the same thing.
func (c Counts) Bad() int { return c.Challenged + c.SupersededByHuman + c.Deleted }

// Total is every signal the agent has produced.
func (c Counts) Total() int { return c.Good() + c.Bad() }

// Posterior is a Beta(Alpha, Beta) distribution over an agent's underlying
// rate of facts that stand.
type Posterior struct {
	Alpha, Beta float64
}

// Update returns the posterior after good positive and bad negative events
// over the prior.
func Update(good, bad int) Posterior {
	return Posterior{Alpha: priorAlpha + float64(good), Beta: priorBeta + float64(bad)}
}

// Mean is the posterior mean, alpha / (alpha + beta).
func (p Posterior) Mean() float64 { return p.Alpha / (p.Alpha + p.Beta) }

// SD is the posterior standard deviation.
func (p Posterior) SD() float64 {
	s := p.Alpha + p.Beta
	return math.Sqrt(p.Alpha * p.Beta / (s * s * (s + 1)))
}

// LowerBound is the mean minus z standard deviations, clamped to [0,1]. It is
// the normal approximation, which is crude for very small counts and is kept
// because it is monotone in both the good and bad counts, which is what a
// ranking needs.
func (p Posterior) LowerBound(z float64) float64 {
	lb := p.Mean() - z*p.SD()
	return math.Max(0, math.Min(1, lb))
}

// Reliability is the conservative reliability of an agent with these counts.
func Reliability(c Counts) float64 {
	return Update(c.Good(), c.Bad()).LowerBound(DefaultZ)
}
