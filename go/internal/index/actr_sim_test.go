package index

import (
	"math"
	"math/rand"
	"sort"
	"testing"
	"time"
)

// Simulation of use streams with known ground-truth usefulness.
//
// The latency benchmark has no use history, so this is the evidence for the
// "actr" flag. Time is in whole days. Each day K facts are shown and each
// shown fact is used with its true usefulness u(i,day). Two exposure regimes:
//
//   - ranked: the retriever shows the top K by its own score (the production
//     situation). Uses from these exposures depend on the retriever's past
//     choices, which is the rich-get-richer loop.
//   - randomised: on a fraction eps of days the K shown facts are drawn
//     uniformly. Uses here do not depend on the retriever, so they are an
//     unbiased view of usefulness.
//
// Two learners feed the activation: "naive" learns from every use; "guard"
// learns only from randomised-regime uses. The ground-truth label for next-use
// prediction is a randomised-regime use in the 14 days after a probe, so no
// predictor is scored on its own exposures.
//
// Everything is seeded. Nothing here is tuned against a reported result: the
// constants (K=5, eps=0.2, 14-day window, probes every 30 days) are chosen
// from the design, not from outcomes.

const (
	simDays    = 365
	simK       = 5
	simEps     = 0.2
	simWindow  = 14
	simFixedHL = 90.0
)

type simFact struct {
	create     int
	burst      bool
	base       float64 // stationary usefulness (non-burst)
	burstStart int
	burstEnd   int
}

func (f simFact) useful(day int) float64 {
	if !f.burst {
		return f.base
	}
	if day >= f.burstStart && day < f.burstEnd {
		return 0.7
	}
	return 0.02
}

// simLog holds the use days for one fact, ascending.
type simLog struct{ all, rand []int }

// traceFrom builds an actrTrace from day numbers, keeping the last actrKeep
// exactly and folding the rest, exactly as the production log does.
func traceFrom(create int, days []int) actrTrace {
	tr := actrTrace{Creation: simTime(float64(create))}
	if len(days) > actrKeep {
		old := days[:len(days)-actrKeep]
		tr.Folded = len(old)
		tr.FoldFirst = simTime(float64(old[0]))
		tr.FoldLast = simTime(float64(old[len(old)-1]))
		days = days[len(days)-actrKeep:]
	}
	for _, d := range days {
		tr.Events = append(tr.Events, simTime(float64(d)))
	}
	return tr
}

func simTime(day float64) time.Time { return actrDay(day) }

// activation returns the logistic activation (the ranking term) for a fact at
// day now from one log, or fallback when the fact has no usable creation.
func activation(f simFact, days []int, now int) float64 {
	b, ok := actrBase(traceFrom(f.create, days), actrDay(float64(now)))
	if !ok {
		return 0.5
	}
	return actrRecency(b)
}

// auc is the probability a random positive outranks a random negative, ties
// counted half. Returns NaN when one class is empty.
func auc(scores []float64, labels []bool) float64 {
	type pair struct {
		s float64
		l bool
	}
	ps := make([]pair, len(scores))
	for i := range scores {
		ps[i] = pair{scores[i], labels[i]}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].s < ps[j].s })
	var pos, neg, sum float64
	for i := 0; i < len(ps); {
		j := i
		for j < len(ps) && ps[j].s == ps[i].s {
			j++
		}
		// ranks i+1..j share the average rank for ties
		avg := float64(i+j+1) / 2
		for k := i; k < j; k++ {
			if ps[k].l {
				sum += avg
				pos++
			} else {
				neg++
			}
		}
		i = j
	}
	if pos == 0 || neg == 0 {
		return math.NaN()
	}
	return (sum - pos*(pos+1)/2) / (pos * neg)
}

// precisionAt10 is the share of the 10 highest-scored facts that are positive.
func precisionAt10(scores []float64, labels []bool) float64 {
	idx := make([]int, len(scores))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
	n := 10
	if len(idx) < n {
		n = len(idx)
	}
	hit := 0
	for _, i := range idx[:n] {
		if labels[i] {
			hit++
		}
	}
	return float64(hit) / float64(n)
}

// runStream simulates one stream. learner is "naive" or "guard" and decides
// which log the ranked retriever scores with.
func runStream(seed int64, learner string, facts []simFact) (logs []simLog) {
	rng := rand.New(rand.NewSource(seed))
	logs = make([]simLog, len(facts))
	for day := 0; day < simDays; day++ {
		var exist []int
		for i, f := range facts {
			if f.create <= day {
				exist = append(exist, i)
			}
		}
		if len(exist) == 0 {
			continue
		}
		var shown []int
		randomised := rng.Float64() < simEps
		if randomised {
			perm := rng.Perm(len(exist))
			for _, p := range perm[:minInt(simK, len(exist))] {
				shown = append(shown, exist[p])
			}
		} else {
			type sc struct {
				i int
				s float64
			}
			scored := make([]sc, 0, len(exist))
			for _, i := range exist {
				src := logs[i].all
				if learner == "guard" {
					src = logs[i].rand
				}
				scored = append(scored, sc{i, activation(facts[i], src, day) + 1e-9*rng.Float64()})
			}
			sort.Slice(scored, func(a, b int) bool { return scored[a].s > scored[b].s })
			for _, c := range scored[:minInt(simK, len(scored))] {
				shown = append(shown, c.i)
			}
		}
		for _, i := range shown {
			if rng.Float64() < facts[i].useful(day) {
				logs[i].all = append(logs[i].all, day)
				if randomised {
					logs[i].rand = append(logs[i].rand, day)
				}
			}
		}
	}
	return logs
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// predictionFacts: half stationary (frequency-driven) with usefulness 0.05 to
// 0.5, half bursty (recency-driven), created across the first 200 days.
func predictionFacts(seed int64) []simFact {
	rng := rand.New(rand.NewSource(seed + 1000))
	facts := make([]simFact, 200)
	for i := range facts {
		f := simFact{create: rng.Intn(200)}
		if i%2 == 1 {
			f.burst = true
			f.burstStart = 30 + rng.Intn(simDays-60)
			f.burstEnd = f.burstStart + 21
		} else {
			f.base = 0.05 + 0.45*rng.Float64()
		}
		facts[i] = f
	}
	return facts
}

type probeResult struct{ aucF, aucN, aucG, p10F, p10N, p10G float64 }

// evalPredictors scores every fact at each probe with the three predictors and
// the randomised-regime label for the window after the probe.
func evalPredictors(facts []simFact, logs []simLog) []probeResult {
	var out []probeResult
	for probe := 210; probe+simWindow < simDays; probe += 30 {
		var fixed, naive, guard []float64
		var lab []bool
		for i, f := range facts {
			if f.create > probe {
				continue
			}
			fixed = append(fixed, math.Exp2(-float64(probe-f.create)/simFixedHL))
			naive = append(naive, activation(f, logs[i].all, probe))
			guard = append(guard, activation(f, logs[i].rand, probe))
			positive := false
			for _, d := range logs[i].rand {
				if d > probe && d <= probe+simWindow {
					positive = true
					break
				}
			}
			lab = append(lab, positive)
		}
		var r probeResult
		r.aucF = auc(fixed, lab)
		r.aucN = auc(naive, lab)
		r.aucG = auc(guard, lab)
		r.p10F = precisionAt10(fixed, lab)
		r.p10N = precisionAt10(naive, lab)
		r.p10G = precisionAt10(guard, lab)
		out = append(out, r)
	}
	return out
}

func meanSE(xs []float64) (float64, float64) {
	var ok []float64
	for _, x := range xs {
		if !math.IsNaN(x) {
			ok = append(ok, x)
		}
	}
	if len(ok) == 0 {
		return math.NaN(), math.NaN()
	}
	m := 0.0
	for _, x := range ok {
		m += x
	}
	m /= float64(len(ok))
	v := 0.0
	for _, x := range ok {
		v += (x - m) * (x - m)
	}
	if len(ok) < 2 {
		return m, 0
	}
	return m, math.Sqrt(v/float64(len(ok)-1)) / math.Sqrt(float64(len(ok)))
}

// TestActrSimulationPrediction reports next-use prediction for the fixed
// half-life term and both activation learners, over 20 seeds. It logs the
// table and asserts nothing about which predictor wins.
func TestActrSimulationPrediction(t *testing.T) {
	const seeds = 20
	var aF, aN, aG, pF, pN, pG []float64
	for s := int64(1); s <= seeds; s++ {
		facts := predictionFacts(s)
		logs := runStream(s, "naive", facts)
		for _, r := range evalPredictors(facts, logs) {
			aF = append(aF, r.aucF)
			aN = append(aN, r.aucN)
			aG = append(aG, r.aucG)
			pF = append(pF, r.p10F)
			pN = append(pN, r.p10N)
			pG = append(pG, r.p10G)
		}
	}
	row := func(name string, a, p []float64) {
		am, ase := meanSE(a)
		pm, pse := meanSE(p)
		t.Logf("%-28s AUC %.3f +/- %.3f   P@10 %.3f +/- %.3f", name, am, ase, pm, pse)
	}
	t.Logf("next-use prediction: %d seeds x probes at day 210..330 (every 30), window %d days, label = randomised-regime use", seeds, simWindow)
	row("fixed 90-day half-life", aF, pF)
	row("ACT-R, naive learner", aN, pN)
	row("ACT-R, randomised-only", aG, pG)
	if len(aF) == 0 {
		t.Fatal("no probes evaluated")
	}
}

// TestActrSimulationRichGetRicher starts two equally useful facts, X and Y,
// with unequal exposure (X has 10 earlier uses, Y none), in a pool of 20 facts
// of equal usefulness 0.3. The naive learner should keep favouring X. The
// randomised-only learner should close the gap. It asserts only that ordering.
func TestActrSimulationRichGetRicher(t *testing.T) {
	const seeds = 20
	gapNaive, gapGuard := []float64{}, []float64{}
	shareNaive, shareGuard := []float64{}, []float64{}
	for s := int64(1); s <= seeds; s++ {
		facts := make([]simFact, 20)
		for i := range facts {
			facts[i] = simFact{create: 0, base: 0.3}
		}
		// X = fact 0. Unequal start: ten uses in the 30 days before day 0,
		// recorded in both logs. The randomised-only learner is given the same
		// head start, so the test measures what happens afterwards.
		var prior []int
		for k := 0; k < 10; k++ {
			prior = append(prior, -30+k*3)
		}
		for _, learner := range []string{"naive", "guard"} {
			logs := make([]simLog, len(facts))
			logs[0].all = append([]int(nil), prior...)
			logs[0].rand = append([]int(nil), prior...)
			rng := rand.New(rand.NewSource(s))
			var xNonRandom, nonRandom int
			for day := 0; day < simDays; day++ {
				randomised := rng.Float64() < simEps
				var shown []int
				if randomised {
					shown = append(shown, rng.Perm(len(facts))[:simK]...)
				} else {
					type sc struct {
						i int
						s float64
					}
					scored := make([]sc, len(facts))
					for i := range facts {
						src := logs[i].all
						if learner == "guard" {
							src = logs[i].rand
						}
						scored[i] = sc{i, activation(facts[i], src, day) + 1e-9*rng.Float64()}
					}
					sort.Slice(scored, func(a, b int) bool { return scored[a].s > scored[b].s })
					for _, c := range scored[:simK] {
						shown = append(shown, c.i)
					}
					if day >= simDays-90 {
						for _, i := range shown {
							nonRandom++
							if i == 0 {
								xNonRandom++
							}
						}
					}
				}
				for _, i := range shown {
					if rng.Float64() < facts[i].useful(day) {
						logs[i].all = append(logs[i].all, day)
						if randomised {
							logs[i].rand = append(logs[i].rand, day)
						}
					}
				}
			}
			bX := bOf(facts[0], logsFor(learner, logs[0]), simDays)
			bY := bOf(facts[1], logsFor(learner, logs[1]), simDays)
			share := float64(xNonRandom) / math.Max(1, float64(nonRandom))
			if learner == "naive" {
				gapNaive = append(gapNaive, bX-bY)
				shareNaive = append(shareNaive, share)
			} else {
				gapGuard = append(gapGuard, bX-bY)
				shareGuard = append(shareGuard, share)
			}
		}
	}
	gn, gnse := meanSE(gapNaive)
	gg, ggse := meanSE(gapGuard)
	sn, _ := meanSE(shareNaive)
	sg, _ := meanSE(shareGuard)
	t.Logf("rich-get-richer: 20 facts, usefulness 0.3 each, X starts with 10 prior uses, %d seeds, 365 days", seeds)
	t.Logf("naive learner:       final B(X)-B(Y) = %+.3f +/- %.3f, X share of last-90-day ranked exposure = %.3f (uniform = 0.050)", gn, gnse, sn)
	t.Logf("randomised-only:     final B(X)-B(Y) = %+.3f +/- %.3f, X share of last-90-day ranked exposure = %.3f", gg, ggse, sg)
	if !(math.Abs(gg) < math.Abs(gn)) {
		t.Fatalf("guard did not reduce the gap: naive %.3f, guard %.3f", gn, gg)
	}
}

func logsFor(learner string, l simLog) []int {
	if learner == "guard" {
		return l.rand
	}
	return l.all
}

func bOf(f simFact, days []int, now int) float64 {
	b, _ := actrBase(traceFrom(f.create, days), actrDay(float64(now)))
	return b
}
