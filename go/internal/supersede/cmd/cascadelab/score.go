package main

// Scoring of gate answers against the generator's labels: accuracy, log loss,
// Brier, ECE, AUC and a reliability table, with an optional Platt fit on the
// calibration split.

import (
	"fmt"
	"math"
	"sort"

	"github.com/JeremiahM37/grimoire/go/internal/supersede"
)

// Platt is a sigmoid over the logit: p = 1/(1+exp(-(a*logit(p0)+b))).
type Platt struct{ A, B float64 }

func logit(p float64) float64 {
	p = math.Min(math.Max(p, 1e-6), 1-1e-6)
	return math.Log(p / (1 - p))
}

// Apply returns the calibrated probability for a raw probability.
func (c Platt) Apply(p float64) float64 {
	return 1 / (1 + math.Exp(-(c.A*logit(p) + c.B)))
}

// FitPlatt fits A and B by gradient descent on the log loss.
func FitPlatt(raw []float64, y []bool) Platt {
	a, b := 1.0, 0.0
	const lr, steps = 0.05, 4000
	n := float64(len(raw))
	for s := 0; s < steps; s++ {
		var ga, gb float64
		for i, p := range raw {
			x := logit(p)
			q := 1 / (1 + math.Exp(-(a*x + b)))
			t := 0.0
			if y[i] {
				t = 1
			}
			ga += (q - t) * x
			gb += q - t
		}
		a -= lr * ga / n
		b -= lr * gb / n
	}
	return Platt{a, b}
}

// Metrics summarises probabilities p against labels y.
type Metrics struct {
	N, Pos      int
	Acc         float64
	LogLoss     float64
	Brier       float64
	ECE         float64
	AUC         float64
	Reliability []Bin
}

// Bin is one reliability bin.
type Bin struct {
	Lo, Hi   float64
	N        int
	MeanPred float64
	FracPos  float64
}

func computeMetrics(p []float64, y []bool) Metrics {
	m := Metrics{N: len(p)}
	const bins = 10
	sums := make([]struct {
		n         int
		pred, pos float64
	}, bins)
	var ll, br, correct float64
	for i, q := range p {
		t := 0.0
		if y[i] {
			t = 1
			m.Pos++
		}
		qq := math.Min(math.Max(q, 1e-9), 1-1e-9)
		ll += -(t*math.Log(qq) + (1-t)*math.Log(1-qq))
		br += (q - t) * (q - t)
		if (q >= 0.5) == y[i] {
			correct++
		}
		k := int(q * bins)
		if k == bins {
			k = bins - 1
		}
		sums[k].n++
		sums[k].pred += q
		sums[k].pos += t
	}
	n := float64(len(p))
	m.Acc = correct / n
	m.LogLoss = ll / n
	m.Brier = br / n
	for k := range sums {
		if sums[k].n == 0 {
			continue
		}
		b := Bin{Lo: float64(k) / bins, Hi: float64(k+1) / bins, N: sums[k].n,
			MeanPred: sums[k].pred / float64(sums[k].n), FracPos: sums[k].pos / float64(sums[k].n)}
		m.ECE += float64(sums[k].n) / n * math.Abs(b.MeanPred-b.FracPos)
		m.Reliability = append(m.Reliability, b)
	}
	m.AUC = auc(p, y)
	return m
}

// auc is the Mann-Whitney probability that a positive outranks a negative.
func auc(p []float64, y []bool) float64 {
	idx := make([]int, len(p))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return p[idx[a]] < p[idx[b]] })
	var rankSum float64
	var pos, neg int
	for r := 0; r < len(idx); {
		j := r
		for j+1 < len(idx) && p[idx[j+1]] == p[idx[r]] {
			j++
		}
		avg := float64(r+j)/2 + 1
		for k := r; k <= j; k++ {
			if y[idx[k]] {
				rankSum += avg
				pos++
			} else {
				neg++
			}
		}
		r = j + 1
	}
	if pos == 0 || neg == 0 {
		return math.NaN()
	}
	return (rankSum - float64(pos)*float64(pos+1)/2) / (float64(pos) * float64(neg))
}

// labelled joins pairs with their cached probabilities, for one split.
func labelled(pairs []supersede.Pair, cache map[string]Answer, split string) ([]supersede.Pair, []float64) {
	var ps []supersede.Pair
	var raw []float64
	for _, p := range pairs {
		if p.Split != split {
			continue
		}
		a, ok := cache[p.ID]
		if !ok {
			continue
		}
		ps = append(ps, p)
		raw = append(raw, a.Replace)
	}
	return ps, raw
}

func printMetrics(label string, m Metrics) {
	fmt.Printf("%-28s n=%4d pos=%4d acc=%.3f logloss=%.3f brier=%.3f ece=%.3f auc=%.3f\n",
		label, m.N, m.Pos, m.Acc, m.LogLoss, m.Brier, m.ECE, m.AUC)
}

func printReliability(m Metrics) {
	fmt.Println("  bin          n   mean_pred  frac_replace")
	for _, b := range m.Reliability {
		fmt.Printf("  [%.1f,%.1f)  %4d   %.3f      %.3f\n", b.Lo, b.Hi, b.N, b.MeanPred, b.FracPos)
	}
}
