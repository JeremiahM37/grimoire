package estimate

import (
	"math"
	"sort"
)

// MemoryData is the logged records of one memory.
type MemoryData struct {
	ID      string
	Kind    string // for example "rule", "preference", "fact", "procedure"
	Records []Record
}

// MemoryEstimate is one memory's lift, raw and shrunk toward its kind.
type MemoryEstimate struct {
	ID   string
	Kind string
	Raw  Estimate
	// Shrunk is the empirical-Bayes posterior mean and interval. It is
	// reported only when Raw is OK; a memory with too little data gets
	// Status Insufficient here as well, and the kind's pooled value is
	// available separately in KindEstimate (a prior, not a measurement).
	Shrunk Estimate
	// Weight is the share of the raw estimate kept (1 means no shrinkage).
	Weight float64
}

// KindEstimate is the random-effects pooled lift for a memory kind.
type KindEstimate struct {
	Kind   string
	Status Status
	Mean   float64 // pooled mean lift
	SE     float64
	Lo, Hi float64
	Tau2   float64 // between-memory variance of true lifts
	K      int     // memories contributing
	Reason string
}

// PoolOptions configure Pool.
type PoolOptions struct {
	Lift Options
	// MinMemories is the fewest measurable memories a kind needs before it
	// is pooled or used as a shrinkage target. Default 3.
	MinMemories int
}

// Pool estimates each memory's lift, pools by kind with a DerSimonian-Laird
// random-effects model, and shrinks each memory toward its kind mean:
//
//	B_i   = se_i^2 / (se_i^2 + tau^2)
//	theta = B_i*mu_kind + (1-B_i)*raw_i
//	var   = 1 / (1/se_i^2 + 1/tau^2) + B_i^2 * se_mu^2
//
// A kind with fewer than MinMemories measurable memories is not pooled and
// its memories keep their raw estimate (Weight 1).
func Pool(mems []MemoryData, o PoolOptions) ([]MemoryEstimate, []KindEstimate, error) {
	if o.MinMemories == 0 {
		o.MinMemories = 3
	}
	lo := o.Lift.withDefaults()
	out := make([]MemoryEstimate, len(mems))
	byKind := map[string][]int{}
	for i, m := range mems {
		est, err := Lift(m.Records, o.Lift)
		if err != nil {
			return nil, nil, err
		}
		out[i] = MemoryEstimate{ID: m.ID, Kind: m.Kind, Raw: est, Weight: 1}
		if est.Status == OK && est.SE > 0 {
			byKind[m.Kind] = append(byKind[m.Kind], i)
		} else if est.Status == OK {
			// zero variance (for instance constant outcomes): keep raw.
			out[i].Shrunk = est
		} else {
			out[i].Shrunk = est
		}
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var kes []KindEstimate
	z := zQuantile(1 - (1-lo.Level)/2)
	seen := map[string]bool{}
	for _, k := range kinds {
		seen[k] = true
		idx := byKind[k]
		if len(idx) < o.MinMemories {
			kes = append(kes, KindEstimate{Kind: k, Status: Insufficient, K: len(idx), Reason: "too few measurable memories"})
			for _, i := range idx {
				out[i].Shrunk = out[i].Raw
			}
			continue
		}
		y := make([]float64, len(idx))
		v := make([]float64, len(idx))
		for j, i := range idx {
			y[j] = out[i].Raw.Lift
			v[j] = out[i].Raw.SE * out[i].Raw.SE
		}
		mu, se, tau2 := randomEffects(y, v)
		kes = append(kes, KindEstimate{Kind: k, Status: OK, Mean: mu, SE: se, Lo: mu - z*se, Hi: mu + z*se, Tau2: tau2, K: len(idx)})
		for j, i := range idx {
			var b float64
			if v[j]+tau2 > 0 {
				b = v[j] / (v[j] + tau2)
			}
			post := b*mu + (1-b)*y[j]
			var pv float64
			if tau2 > 0 {
				pv = 1 / (1/v[j] + 1/tau2)
			} else {
				pv = 0 // tau2 = 0: all memories share one lift; shrunk fully to mu
			}
			// the kind mean is itself estimated: carry its uncertainty
			// through the share of it that the posterior uses.
			pv += b * b * se * se
			if tau2 == 0 {
				pv = se * se
			}
			psd := math.Sqrt(pv)
			s := out[i].Raw
			s.Lift, s.SE, s.Lo, s.Hi = post, psd, post-z*psd, post+z*psd
			out[i].Shrunk = s
			out[i].Weight = 1 - b
		}
	}
	// kinds with no measurable memory at all
	var rest []string
	for _, m := range mems {
		if !seen[m.Kind] {
			seen[m.Kind] = true
			rest = append(rest, m.Kind)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		kes = append(kes, KindEstimate{Kind: k, Status: Insufficient, Reason: "no measurable memories"})
	}
	return out, kes, nil
}

// randomEffects is the DerSimonian-Laird estimator: pooled mean, its
// standard error, and the between-study variance tau2 (floored at 0).
func randomEffects(y, v []float64) (mu, se, tau2 float64) {
	k := float64(len(y))
	var sw, swy, swy2, sw2 float64
	for i := range y {
		w := 1 / v[i]
		sw += w
		swy += w * y[i]
		swy2 += w * y[i] * y[i]
		sw2 += w * w
	}
	fixed := swy / sw
	q := swy2 - swy*swy/sw
	c := sw - sw2/sw
	if c > 0 {
		tau2 = math.Max(0, (q-(k-1))/c)
	}
	var rw, rwy float64
	for i := range y {
		w := 1 / (v[i] + tau2)
		rw += w
		rwy += w * y[i]
	}
	if rw == 0 {
		return fixed, math.Sqrt(1 / sw), tau2
	}
	return rwy / rw, math.Sqrt(1 / rw), tau2
}
