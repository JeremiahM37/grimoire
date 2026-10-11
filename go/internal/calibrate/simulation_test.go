package calibrate

import (
	"math/rand/v2"
	"testing"
)

// The simulation answers one question: how many correction events does it
// take before the ranking of two agents stops changing? Each agent produces
// correction events as Bernoulli draws at a fixed rate of facts that stand
// (p), so the true ordering is known. The ranking is "right" at n events when
// the agent with the higher true p has the higher lower bound.
//
// It is deterministic (fixed PCG seed) and offline. It models the learner, not
// the world: real correction signals are sparse and not i.i.d. across facts, so
// these counts are a lower bound on what real traffic needs, not a promise.

const (
	simTrials = 2000
	simMaxN   = 300
)

type simPair struct {
	name     string
	pHi, pLo float64
}

// simAgreement returns, for each n in 0..simMaxN, the fraction of trials in
// which the higher-p agent's lower bound is strictly above the lower-p agent's,
// and the fraction of trials whose sign at n differs from its sign at simMaxN.
func simAgreement(pHi, pLo float64, seed uint64) (agree, flipped []float64) {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	agree = make([]float64, simMaxN+1)
	flipped = make([]float64, simMaxN+1)
	signs := make([][]bool, simTrials)
	for t := 0; t < simTrials; t++ {
		var hi, lo Counts
		row := make([]bool, simMaxN+1)
		for n := 0; n <= simMaxN; n++ {
			if n > 0 {
				if rng.Float64() < pHi {
					hi.Add(Confirmed)
				} else {
					hi.Add(Challenged)
				}
				if rng.Float64() < pLo {
					lo.Add(Confirmed)
				} else {
					lo.Add(Challenged)
				}
			}
			ok := Reliability(hi) > Reliability(lo)
			row[n] = ok
			if ok {
				agree[n]++
			}
		}
		signs[t] = row
	}
	for n := 0; n <= simMaxN; n++ {
		agree[n] /= simTrials
	}
	for t := 0; t < simTrials; t++ {
		final := signs[t][simMaxN]
		for n := 0; n <= simMaxN; n++ {
			if signs[t][n] != final {
				flipped[n]++
			}
		}
	}
	for n := range flipped {
		flipped[n] /= simTrials
	}
	return agree, flipped
}

// firstStable is the smallest n from which agreement stays at or above level
// for every larger n, or -1 if it never does within simMaxN.
func firstStable(agree []float64, level float64) int {
	for n := len(agree) - 1; n >= 0; n-- {
		if agree[n] < level {
			if n == len(agree)-1 {
				return -1
			}
			return n + 1
		}
	}
	return 0
}

func TestSimulationEventsNeededForStableRanking(t *testing.T) {
	pairs := []simPair{
		{name: "0.90 vs 0.60", pHi: 0.90, pLo: 0.60},
		{name: "0.90 vs 0.80", pHi: 0.90, pLo: 0.80},
		{name: "0.80 vs 0.50", pHi: 0.80, pLo: 0.50},
	}
	for i, p := range pairs {
		agree, flipped := simAgreement(p.pHi, p.pLo, uint64(1000+i))
		stable := firstStable(agree, 0.95)
		t.Logf("pair %s (trials=%d, max n=%d)", p.name, simTrials, simMaxN)
		for _, n := range []int{1, 2, 5, 10, 20, 50, 100, 200, 300} {
			t.Logf("  n=%3d events each: agree=%.3f  sign-differs-from-final=%.3f",
				n, agree[n], flipped[n])
		}
		t.Logf("  first n with agreement >=0.95 from there on: %d", stable)
		if stable < 0 {
			t.Errorf("pair %s never stabilised at 95%% agreement within %d events", p.name, simMaxN)
		}
	}
}

// The gap that matters in practice is the one an operator would act on:
// a clearly worse agent against a good one. That ordering is learned quickly
// and should be stable well before fifty events each.
func TestSimulationClearGapIsStableEarly(t *testing.T) {
	agree, _ := simAgreement(0.90, 0.60, 42)
	if agree[50] < 0.90 {
		t.Fatalf("agreement at 50 events each = %.3f, want >= 0.90", agree[50])
	}
}

// A near-tie is not learned quickly, and the test records that rather than
// hiding it. Agreement on a 0.80 vs 0.50 gap is required to reach 95% within
// the horizon, but not at 10 events.
func TestSimulationNearTieIsNotQuick(t *testing.T) {
	agree, _ := simAgreement(0.90, 0.80, 7)
	if agree[10] >= 0.95 {
		t.Fatalf("0.90 vs 0.80 already at %.3f agreement after 10 events; the test expects a slow near-tie", agree[10])
	}
}
