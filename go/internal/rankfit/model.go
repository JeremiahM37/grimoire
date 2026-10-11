// Package rankfit fits a small linear ranking model to logged memory
// exposures and estimates how other weight vectors would have done on the
// same logs. It is pure arithmetic: no I/O, no clock, no randomness except
// the bootstrap, which takes an explicit seed.
//
// The logged data has three biases that a plain regression of use on features
// does not remove:
//
//   - Position. A memory shown first is used more often than the same memory
//     shown eighth, whatever its relevance. The examination curve exam(k) is
//     estimated from exploration slates, where the order was shuffled
//     independently of the features, and every use is divided by it.
//   - Selection. The logging policy shows the top of its own ranking, so the
//     features that ranked an item high are the ones correlated with being
//     seen. Weighting by exam(pos)^2 and by 1/P(shown) undoes the part caused
//     by position; the holdout gives P(shown) directly.
//   - Support. Only items in a logged slate have outcomes. Policies are
//     therefore compared by reranking the same slates, never by bringing in
//     items the logger did not show.
//
// Use is a 0/1 outcome observed only for shown items. Held-out items carry no
// outcome; they enter the estimators only through their features and their
// inclusion probability.
package rankfit

import (
	"errors"
	"math"
	"math/rand/v2"
	"sort"
)

// Dims is the number of features. The order is fixed and is the order the
// logs store them in: semantic, keyword, entity, recency, useful, link,
// authority (1 for a human-authored fact, else 0).
const Dims = 7

// FeatureNames labels the components, in order.
var FeatureNames = [Dims]string{"semantic", "keyword", "entity", "recency", "useful", "link", "authority"}

// Tolerance is the normalised L1 distance (see L1) within which a learned
// weight vector counts as recovering the planted one. It was fixed before the
// simulation was run, not chosen from its results.
const Tolerance = 0.10

// Item is one candidate in one logged slate.
type Item struct {
	Slate    string    // slate identifier; items sharing it were ranked together
	Pos      int       // 1-based position of the item in the slate as displayed (0 unknown)
	Explored bool      // the slate's order was randomised for exploration
	Shown    bool      // the item was shown (not withheld by the holdout)
	PShow    float64   // probability this item was shown; 1 if it was never eligible
	Used     float64   // 0 or 1: the agent cited the memory. Meaningful only when Shown
	Feat     []float64 // Dims features
}

// Options configure the learner. The zero value is usable.
type Options struct {
	// ClipShow bounds PShow away from zero so one rare shown item cannot
	// dominate the fit. Default 0.05, so a weight is at most 20.
	ClipShow float64
	// MinPerPos is the fewest explored, shown records each position needs
	// before its examination probability is estimated. Default 20.
	MinPerPos int
	// Ridge is a small diagonal term that keeps the normal equations solvable
	// when a feature is constant. Default 1e-6.
	Ridge float64
}

func (o Options) withDefaults() Options {
	if o.ClipShow == 0 {
		o.ClipShow = 0.05
	}
	if o.MinPerPos == 0 {
		o.MinPerPos = 20
	}
	if o.Ridge == 0 {
		o.Ridge = 1e-6
	}
	return o
}

// ErrNoExploration is returned when there are too few explored, shown records
// to estimate the position curve. Without it the examination propensity is
// unknown, and an IPS estimate would be a number with no basis.
var ErrNoExploration = errors.New("rankfit: not enough explored records to estimate position bias")

// ErrNoData is returned when no record can enter a fit.
var ErrNoData = errors.New("rankfit: no usable records")

// Exam is the examination curve, normalised so that position 1 is 1.
type Exam struct {
	Curve []float64 // Curve[k-1] is the examination of position k
	N     []int     // explored, shown records behind each position
}

// EstimateExam estimates the examination curve for slots 1..k from explored
// slates. Within an explored slate the position is independent of the
// features, so the mean outcome at position k is exam(k) times the mean
// relevance of the slate, and the ratio to position 1 is identified.
func EstimateExam(items []Item, k int, o Options) (Exam, error) {
	o = o.withDefaults()
	sum := make([]float64, k)
	n := make([]int, k)
	for _, it := range items {
		if !it.Explored || !it.Shown || it.Pos < 1 || it.Pos > k {
			continue
		}
		sum[it.Pos-1] += it.Used
		n[it.Pos-1]++
	}
	for _, c := range n {
		if c < o.MinPerPos {
			return Exam{N: n}, ErrNoExploration
		}
	}
	base := sum[0] / float64(n[0])
	if base <= 0 {
		return Exam{N: n}, ErrNoExploration
	}
	curve := make([]float64, k)
	for i := range curve {
		curve[i] = (sum[i] / float64(n[i])) / base
	}
	return Exam{Curve: curve, N: n}, nil
}

// fit solves the weighted least squares problem for the relevance weights.
//
// The target of a shown record is Used/exam(pos), which has expectation
// relevance(x). Its variance grows like 1/exam^2, so each record is weighted by
// exam^2 (equivalently, the fit is on Used directly with regressors scaled by
// exam). Records are further weighted by 1/PShow, clipped. Records without a
// position or outside the curve are skipped. exam nil means no position
// correction: this is the naive fit.
func fit(items []Item, exam []float64, o Options) ([]float64, error) {
	var a [Dims][Dims]float64
	var b [Dims]float64
	n := 0
	for _, it := range items {
		if !it.Shown || len(it.Feat) != Dims {
			continue
		}
		e := 1.0
		if exam != nil {
			if it.Pos < 1 || it.Pos > len(exam) {
				continue
			}
			e = exam[it.Pos-1]
			if e <= 0 {
				continue
			}
		}
		ps := math.Max(it.PShow, o.ClipShow)
		if ps <= 0 || ps > 1 {
			continue
		}
		// Weight on the scaled target: w = exam^2 / PShow, and the target is
		// Used/exam, so the normal equations use x and Used/exam with weight
		// exam^2/PShow. Written out: sum (w) x x' and sum (w) x (Used/exam).
		wt := e * e / ps
		t := it.Used / e
		for i := 0; i < Dims; i++ {
			b[i] += wt * it.Feat[i] * t
			for j := 0; j < Dims; j++ {
				a[i][j] += wt * it.Feat[i] * it.Feat[j]
			}
		}
		n++
	}
	if n == 0 {
		return nil, ErrNoData
	}
	for i := 0; i < Dims; i++ {
		a[i][i] += o.Ridge
	}
	return solve(a, b)
}

// solve solves the Dims x Dims system by Gaussian elimination with partial
// pivoting.
func solve(a [Dims][Dims]float64, b [Dims]float64) ([]float64, error) {
	m := make([][]float64, Dims)
	for i := range m {
		m[i] = make([]float64, Dims+1)
		copy(m[i], a[i][:])
		m[i][Dims] = b[i]
	}
	for c := 0; c < Dims; c++ {
		p := c
		for r := c + 1; r < Dims; r++ {
			if math.Abs(m[r][c]) > math.Abs(m[p][c]) {
				p = r
			}
		}
		if math.Abs(m[p][c]) < 1e-12 {
			return nil, ErrNoData
		}
		m[c], m[p] = m[p], m[c]
		for r := 0; r < Dims; r++ {
			if r == c {
				continue
			}
			f := m[r][c] / m[c][c]
			for k := c; k <= Dims; k++ {
				m[r][k] -= f * m[c][k]
			}
		}
	}
	out := make([]float64, Dims)
	for i := range out {
		out[i] = m[i][Dims] / m[i][i]
	}
	return out, nil
}

// FitPropensity returns the learned weights with position correction. exam
// is the curve from EstimateExam.
func FitPropensity(items []Item, exam []float64, o Options) ([]float64, error) {
	return fit(items, exam, o.withDefaults())
}

// FitNaive regresses use on the features with no position correction and
// no inverse weighting beyond the holdout. This is the learner the trace would
// give if it fitted what it logged.
func FitNaive(items []Item, o Options) ([]float64, error) {
	return fit(items, nil, o.withDefaults())
}

// Bootstrap resamples slates with replacement and refits, returning the 2.5th
// and 97.5th percentile of each weight. Slates, not items, are resampled
// because items in one slate share a context. Seed fixes the draws.
func Bootstrap(slates [][]Item, exam []float64, naive bool, o Options, reps int, seed uint64) (lo, hi []float64, err error) {
	o = o.withDefaults()
	rng := rand.New(rand.NewPCG(seed, 0x9e3779b97f4a7c15))
	draws := make([][]float64, 0, reps)
	for r := 0; r < reps; r++ {
		var items []Item
		for range slates {
			items = append(items, slates[rng.IntN(len(slates))]...)
		}
		var w []float64
		if naive {
			w, err = FitNaive(items, o)
		} else {
			w, err = FitPropensity(items, exam, o)
		}
		if err != nil {
			continue
		}
		draws = append(draws, w)
	}
	if len(draws) == 0 {
		return nil, nil, ErrNoData
	}
	lo, hi = make([]float64, Dims), make([]float64, Dims)
	for d := 0; d < Dims; d++ {
		col := make([]float64, len(draws))
		for i := range draws {
			col[i] = draws[i][d]
		}
		sort.Float64s(col)
		lo[d] = col[int(0.025*float64(len(col)-1))]
		hi[d] = col[int(0.975*float64(len(col)-1))]
	}
	return lo, hi, nil
}

// Normalise scales weights to sum to 1 by absolute value, so two weight
// vectors that differ only in scale compare equal. The learner's scale is not
// identified by use data.
func Normalise(w []float64) []float64 {
	s := 0.0
	for _, v := range w {
		s += math.Abs(v)
	}
	out := make([]float64, len(w))
	if s == 0 {
		return out
	}
	for i, v := range w {
		out[i] = v / s
	}
	return out
}

// L1 is the L1 distance between two weight vectors after normalisation.
func L1(a, b []float64) float64 {
	na, nb := Normalise(a), Normalise(b)
	d := 0.0
	for i := range na {
		d += math.Abs(na[i] - nb[i])
	}
	return d
}

// Cosine is the cosine similarity of two weight vectors.
func Cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}
