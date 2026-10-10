package estimate

import (
	"fmt"

	"github.com/JeremiahM37/grimoire/go/internal/utilization"
)

// Causal adapts Lift to the trace's utilization.Estimator: a doubly-robust
// estimate stratified by the pending tool (or the stage, at prompt time),
// with the propensity of the realised arm taken from the holdout record.
type Causal struct{ Opt Options }

// NewCausal is the estimator the server installs: doubly robust, defaults
// for the minimum arm sizes and the confidence level.
func NewCausal() Causal { return Causal{Opt: Options{Method: DR}} }

func (c Causal) Name() string { return c.Opt.Method.String() }

func (c Causal) Lift(recs []utilization.Record) (utilization.Estimate, error) {
	in := make([]Record, 0, len(recs))
	for _, r := range recs {
		stratum := r.Tool
		if stratum == "" {
			stratum = r.Stage
		}
		in = append(in, Record{Exposed: r.Treated, Propensity: 1 - r.PWithhold, Outcome: r.Y, Stratum: stratum})
	}
	e, err := Lift(in, c.Opt)
	if err != nil {
		return utilization.Estimate{}, err
	}
	out := utilization.Estimate{Method: e.Method.String(), N: e.N, Treated: e.NExposed, Control: e.NWithheld}
	if e.Status != OK {
		return out, fmt.Errorf("%w: %s", utilization.ErrInsufficient, e.Reason)
	}
	out.Effect, out.Lo, out.Hi = e.Lift, e.Lo, e.Hi
	return out, nil
}
