package rankfit

import (
	"math"
	"sort"
)

// Off-policy evaluation of linear rankers on logged slates.
//
// A policy reranks each logged slate by its own score w.x. Its value is the
// expected sum over the slate of exam(rank) times relevance, the quantity a
// position-biased click model rewards. The logger showed every slate in its
// own order and withheld some items; an item's use is observed only if it was
// shown, with probability PShow, and in position Pos. So for an item j placed at
// rank r under the target policy:
//
//	IPS term  = Shown_j * Used_j / (PShow_j * exam(Pos_j)) * exam(r)
//
// has expectation exam(r) * relevance_j, because E[Used | shown, Pos] =
// exam(Pos) * relevance. Summed over the slate and averaged over slates, this
// is the value of the policy. Held items contribute zero to the correction
// term but are still ranked, so the policy sees the same slate the logger did.
//
// The doubly robust form adds a direct term from a relevance model r^(x) fitted
// on other slates (two folds, cross-fitted):
//
//	DR term   = sum_j exam(r_j) r^(x_j)  +  sum_j Shown_j/PShow_j *
//	            (Used_j - exam(Pos_j) r^(x_j)) * exam(r_j)/exam(Pos_j)
//
// It is unbiased if either the propensities or r^ is right.

// Ranks returns, for each item of a slate, its 1-based rank when the slate is
// ordered by w.x (highest first, ties by input order).
func Ranks(slate []Item, w []float64) []int {
	idx := make([]int, len(slate))
	score := make([]float64, len(slate))
	for i, it := range slate {
		idx[i] = i
		for d := 0; d < Dims && d < len(it.Feat); d++ {
			score[i] += w[d] * it.Feat[d]
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return score[idx[a]] > score[idx[b]] })
	rank := make([]int, len(slate))
	for r, i := range idx {
		rank[i] = r + 1
	}
	return rank
}

// EvaluateIPS returns the inverse-propensity value of policy w on slates. exam
// must cover every position used in the slates.
func EvaluateIPS(slates [][]Item, exam []float64, w []float64) float64 {
	if len(slates) == 0 {
		return 0
	}
	total := 0.0
	for _, s := range slates {
		rank := Ranks(s, w)
		for j, it := range s {
			if !it.Shown || it.Used == 0 {
				continue
			}
			ps := math.Max(it.PShow, 0.05)
			total += it.Used / (ps * exam[it.Pos-1]) * exam[rank[j]-1]
		}
	}
	return total / float64(len(slates))
}

// EvaluateDR returns the doubly robust value of policy w on slates. Slates are
// split into two folds by index; the relevance model for each fold is fitted
// on the other fold.
func EvaluateDR(slates [][]Item, exam []float64, w []float64, o Options) (float64, error) {
	o = o.withDefaults()
	if len(slates) == 0 {
		return 0, nil
	}
	var models [2][]float64
	for f := 0; f < 2; f++ {
		var train []Item
		for i, s := range slates {
			if i%2 != f {
				train = append(train, s...)
			}
		}
		m, err := FitPropensity(train, exam, o)
		if err != nil {
			return 0, err
		}
		models[f] = m
	}
	total := 0.0
	for i, s := range slates {
		m := models[i%2]
		rank := Ranks(s, w)
		for j, it := range s {
			rhat := dot(m, it.Feat)
			total += exam[rank[j]-1] * rhat
			if !it.Shown {
				continue
			}
			ps := math.Max(it.PShow, 0.05)
			e := exam[it.Pos-1]
			total += (it.Used - e*rhat) / ps * exam[rank[j]-1] / e
		}
	}
	return total / float64(len(slates)), nil
}

// TrueValue is the expected value of policy w over slates, using the true
// relevance supplied per item (for simulation only).
func TrueValue(slates [][]Item, rel [][]float64, trueExam []float64, w []float64) float64 {
	total := 0.0
	for si, s := range slates {
		rank := Ranks(s, w)
		for j := range s {
			total += trueExam[rank[j]-1] * rel[si][j]
		}
	}
	return total / float64(len(slates))
}

func dot(w, x []float64) float64 {
	s := 0.0
	for i := range w {
		if i < len(x) {
			s += w[i] * x[i]
		}
	}
	return s
}
