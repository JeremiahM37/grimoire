// Package utilization defines what the live per-memory trace hands to an
// estimator of a memory's causal effect, and the interface an estimator
// implements. The trace (internal/adherence/trace.go) logs randomised
// withholding decisions with their propensity; turning those records into a
// lift with a confidence interval is the estimator's job
// (internal/utilization/estimate: IPS, doubly robust, shrinkage). This package
// only fixes the shape so the two can be built apart and joined later.
package utilization

import "errors"

// Record is one decision point: an eligible memory was, or was not, put in
// front of an agent, and what followed. No text: ids, hashes and codes only.
type Record struct {
	Target  string // fact:<id> or note:<path>
	Kind    string // rule, procedure, preference, fact, reference ("" unknown)
	Session string // opaque session hash
	Stage   string // prompt or action
	Tool    string // tool of the pending action (action stage), else ""
	TS      int64  // unix seconds

	// Treated: the memory was injected. A withheld item has Treated=false and
	// is the "would have injected" arm.
	Treated bool
	// PWithhold is the probability this item was withheld at this decision
	// (the configured holdout rate). The probability of the realised arm is
	// PWithhold if !Treated, else 1-PWithhold. Always in (0,1) here: items
	// that were never eligible for withholding are not records.
	PWithhold float64

	// Y is the outcome in [0,1]: the share of bad outcomes (tool error,
	// failing tests, a later re-edit or revert, thrash, a denial, a user
	// correction) among the up-to-Window actions that followed, counting only
	// actions whose outcome was observable. Lower is better. Actions is how
	// many such actions there were; a record with Actions==0 is not returned.
	Y       float64
	Actions int
	// Influenced: the memory's uptake evidence was linked to one of those
	// actions (Treated records only).
	Influenced bool
	// Uptake is set when the memory showed in the agent's work in any way
	// (tag cited or a fingerprint matched).
	Uptake bool
}

// Estimate is an effect estimate on Y. Effect is treated minus withheld, so a
// negative Effect means the memory lowered the bad-outcome share.
type Estimate struct {
	Method  string
	Effect  float64
	Lo, Hi  float64 // 95% interval
	N       int     // records used
	Treated int
	Control int
	Note    string
}

// ErrNoEstimator is returned when no estimator is installed.
var ErrNoEstimator = errors.New("no causal estimator installed")

// Estimator turns decision records into an effect with an interval.
// Implementations must not assume equal arm sizes and must use PWithhold.
type Estimator interface {
	Name() string
	Lift(recs []Record) (Estimate, error)
}

// None is the estimator used until a real one is plugged in.
type None struct{}

func (None) Name() string                    { return "none" }
func (None) Lift([]Record) (Estimate, error) { return Estimate{}, ErrNoEstimator }
