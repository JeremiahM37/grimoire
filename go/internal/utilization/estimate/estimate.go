// Package estimate turns logged memory exposures into lift estimates.
//
// The question it answers is causal: how much does showing memory M change
// the outcome of the action it was eligible for? The data is a randomized
// holdout: each eligible injection is withheld with a known probability, and
// the record keeps whether the memory was exposed, the probability it had of
// being exposed (the propensity), and an outcome score. Everything here is
// pure arithmetic over those records; there is no I/O and no clock.
//
// Three layers:
//
//   - Lift: IPS and doubly-robust estimates of E[Y(exposed)] - E[Y(withheld)]
//     with analytic or bootstrap confidence intervals, and a minimum-n guard
//     that answers Insufficient instead of a number.
//   - Pool: per-memory lifts pooled by memory kind, with empirical-Bayes
//     (normal-normal, DerSimonian-Laird) shrinkage of each memory toward its
//     kind.
//   - Utility: a MemRL-style running value that is updated only when the
//     memory was exposed, because a withheld memory teaches nothing about
//     the memory.
//
// Observational numbers (no randomization) must not be fed in: the estimators
// are only unbiased because the propensities are known.
package estimate

import (
	"errors"
	"math"
	"math/rand/v2"
	"sort"
)

// Record is one eligible injection.
type Record struct {
	Exposed bool
	// Propensity is the probability that this injection was exposed, as
	// decided by the randomizer before the draw. Must be in (0, 1).
	Propensity float64
	// Outcome is the score of the action or session; higher is better.
	// Binary 0/1 and bounded scores are the intended inputs.
	Outcome float64
	// Stratum optionally groups records for the doubly-robust outcome model
	// (for example a tool name or a task class). Empty means one stratum.
	Stratum string
}

// Method selects the estimator.
type Method int

const (
	// IPS is the Horvitz-Thompson inverse-propensity estimator. Unbiased,
	// high variance.
	IPS Method = iota
	// DR is the doubly-robust (augmented IPS) estimator with a cross-fitted
	// stratum-mean outcome model. Unbiased, usually much lower variance.
	DR
)

func (m Method) String() string {
	if m == DR {
		return "dr"
	}
	return "ips"
}

// Status says whether an estimate may be shown as a number.
type Status string

const (
	OK           Status = "ok"
	Insufficient Status = "insufficient"
)

// Options configure Lift. The zero value is usable.
type Options struct {
	Method Method
	// MinExposed and MinWithheld are the fewest records per arm below which
	// the result is Insufficient. Defaults 10 and 10.
	MinExposed  int
	MinWithheld int
	// Level is the confidence level, default 0.95.
	Level float64
	// Folds is the number of cross-fitting folds for DR, default 5.
	Folds int
	// ClipMin bounds the propensity away from 0 and 1 for the weights.
	// Default 0.01. A record outside (0,1) is an error, not clipped.
	ClipMin float64
}

func (o Options) withDefaults() Options {
	if o.MinExposed == 0 {
		o.MinExposed = 10
	}
	if o.MinWithheld == 0 {
		o.MinWithheld = 10
	}
	if o.Level == 0 {
		o.Level = 0.95
	}
	if o.Folds == 0 {
		o.Folds = 5
	}
	if o.ClipMin == 0 {
		o.ClipMin = 0.01
	}
	return o
}

// Estimate is a lift with its uncertainty.
type Estimate struct {
	Status    Status
	Method    Method
	Lift      float64 // E[Y|exposed] - E[Y|withheld]; meaningful only when Status is OK
	SE        float64
	Lo, Hi    float64
	N         int
	NExposed  int
	NWithheld int
	// Reason is set when Status is Insufficient.
	Reason string
}

// ErrBadRecord is returned for a propensity outside (0,1) or a non-finite outcome.
var ErrBadRecord = errors.New("estimate: propensity must be in (0,1) and outcome finite")

func validate(recs []Record) error {
	for _, r := range recs {
		if !(r.Propensity > 0 && r.Propensity < 1) || math.IsNaN(r.Outcome) || math.IsInf(r.Outcome, 0) {
			return ErrBadRecord
		}
	}
	return nil
}

func counts(recs []Record) (exp, wh int) {
	for _, r := range recs {
		if r.Exposed {
			exp++
		} else {
			wh++
		}
	}
	return
}

func clip(p, c float64) float64 { return math.Min(1-c, math.Max(c, p)) }

// scores returns the per-record influence-function values whose mean is the
// lift estimate. For IPS: T*Y/e - (1-T)*Y/(1-e). For DR the outcome model
// terms are added; mu is nil for IPS.
func scores(recs []Record, o Options, rng *rand.Rand) []float64 {
	n := len(recs)
	psi := make([]float64, n)
	var mu1, mu0 []float64
	if o.Method == DR {
		mu1, mu0 = crossFit(recs, o.Folds, rng)
	}
	for i, r := range recs {
		e := clip(r.Propensity, o.ClipMin)
		var a, b float64 // contribution of the exposed and withheld worlds
		if r.Exposed {
			a = r.Outcome / e
		} else {
			b = r.Outcome / (1 - e)
		}
		if o.Method == DR {
			t := 0.0
			if r.Exposed {
				t = 1
			}
			a = mu1[i] + t*(r.Outcome-mu1[i])/e
			b = mu0[i] + (1-t)*(r.Outcome-mu0[i])/(1-e)
		}
		psi[i] = a - b
	}
	return psi
}

// crossFit returns, per record, the predicted outcome under exposure and under
// withholding from stratum-by-arm means fitted on the other folds. Folds are
// assigned round-robin after a deterministic shuffle so the result does not
// depend on record order.
func crossFit(recs []Record, folds int, rng *rand.Rand) (mu1, mu0 []float64) {
	n := len(recs)
	if folds < 2 {
		folds = 2
	}
	if folds > n {
		folds = n
	}
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	if rng != nil {
		rng.Shuffle(n, func(i, j int) { perm[i], perm[j] = perm[j], perm[i] })
	}
	fold := make([]int, n)
	for k, idx := range perm {
		fold[idx] = k % folds
	}
	mu1 = make([]float64, n)
	mu0 = make([]float64, n)
	type acc struct{ s, n float64 }
	for f := 0; f < folds; f++ {
		cell := map[[2]string]acc{} // {stratum, arm}
		arm := [2]acc{}
		for i, r := range recs {
			if fold[i] == f {
				continue
			}
			a := 0
			if r.Exposed {
				a = 1
			}
			k := [2]string{r.Stratum, string(rune('0' + a))}
			c := cell[k]
			c.s += r.Outcome
			c.n++
			cell[k] = c
			arm[a].s += r.Outcome
			arm[a].n++
		}
		pred := func(stratum string, a int) float64 {
			if c := cell[[2]string{stratum, string(rune('0' + a))}]; c.n >= 2 {
				return c.s / c.n
			}
			if arm[a].n > 0 {
				return arm[a].s / arm[a].n
			}
			return 0
		}
		for i, r := range recs {
			if fold[i] != f {
				continue
			}
			mu1[i] = pred(r.Stratum, 1)
			mu0[i] = pred(r.Stratum, 0)
		}
	}
	return
}

func meanVar(x []float64) (m, v float64) {
	n := float64(len(x))
	for _, a := range x {
		m += a
	}
	m /= n
	for _, a := range x {
		v += (a - m) * (a - m)
	}
	if n > 1 {
		v /= n - 1
	}
	return
}

func insufficient(o Options, recs []Record, reason string) Estimate {
	e, w := counts(recs)
	return Estimate{Status: Insufficient, Method: o.Method, N: len(recs), NExposed: e, NWithheld: w, Reason: reason}
}

func guard(recs []Record, o Options) (Estimate, bool) {
	e, w := counts(recs)
	if e < o.MinExposed {
		return insufficient(o, recs, "too few exposed records"), false
	}
	if w < o.MinWithheld {
		return insufficient(o, recs, "too few withheld records"), false
	}
	return Estimate{}, true
}

// Lift estimates the effect of exposure with a normal-approximation interval
// from the estimator's influence function. DR uses a fixed cross-fitting
// split, so the result is deterministic.
func Lift(recs []Record, o Options) (Estimate, error) {
	o = o.withDefaults()
	if err := validate(recs); err != nil {
		return Estimate{}, err
	}
	if est, ok := guard(recs, o); !ok {
		return est, nil
	}
	psi := scores(recs, o, rand.New(rand.NewPCG(1, 2)))
	m, v := meanVar(psi)
	se := math.Sqrt(v / float64(len(psi)))
	z := zQuantile(1 - (1-o.Level)/2)
	e, w := counts(recs)
	return Estimate{Status: OK, Method: o.Method, Lift: m, SE: se, Lo: m - z*se, Hi: m + z*se,
		N: len(recs), NExposed: e, NWithheld: w}, nil
}

// LiftBootstrap is Lift with a percentile-bootstrap interval over records
// (B resamples, seeded). Prefer it for small n or skewed outcomes. The point
// estimate and SE are those of the full sample and the bootstrap spread
// respectively.
func LiftBootstrap(recs []Record, o Options, B int, seed uint64) (Estimate, error) {
	o = o.withDefaults()
	if err := validate(recs); err != nil {
		return Estimate{}, err
	}
	if est, ok := guard(recs, o); !ok {
		return est, nil
	}
	if B < 100 {
		B = 100
	}
	base, _ := Lift(recs, o)
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	n := len(recs)
	draws := make([]float64, 0, B)
	buf := make([]Record, n)
	for b := 0; b < B; b++ {
		for i := range buf {
			buf[i] = recs[rng.IntN(n)]
		}
		e, w := counts(buf)
		if e == 0 || w == 0 {
			continue // an arm vanished from this resample; skip it
		}
		psi := scores(buf, o, rng)
		m, _ := meanVar(psi)
		draws = append(draws, m)
	}
	if len(draws) < B/2 {
		return insufficient(o, recs, "bootstrap resamples lost an arm too often"), nil
	}
	sort.Float64s(draws)
	a := (1 - o.Level) / 2
	_, v := meanVar(draws)
	base.SE = math.Sqrt(v)
	base.Lo = quantile(draws, a)
	base.Hi = quantile(draws, 1-a)
	return base, nil
}

func quantile(sorted []float64, q float64) float64 {
	pos := q * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	f := pos - float64(lo)
	return sorted[lo]*(1-f) + sorted[hi]*f
}

// zQuantile is the standard normal quantile (Acklam's rational approximation,
// relative error below 1.2e-9).
func zQuantile(p float64) float64 {
	a := []float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := []float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := []float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := []float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const pl = 0.02425
	switch {
	case p <= 0:
		return math.Inf(-1)
	case p >= 1:
		return math.Inf(1)
	case p < pl:
		q := math.Sqrt(-2 * math.Log(p))
		return (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p > 1-pl:
		q := math.Sqrt(-2 * math.Log(1-p))
		return -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	q := p - 0.5
	r := q * q
	return (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q / (((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
}
