package main

// The cascade evaluation. The production gate (internal/cascade) is run with
// cache-backed askers, so the numbers below come from the same decision code
// the write path uses. The uncertain band is chosen on the calibration split
// only, and the test split is then reported once.

import (
	"context"
	"fmt"
	"math"

	"github.com/JeremiahM37/grimoire/go/internal/cascade"
	"github.com/JeremiahM37/grimoire/go/internal/supersede"
)

// cacheAsker answers from a cache of raw probabilities keyed by the pair's
// state. A missing key is an error, which the gate treats as "no answer".
type cacheAsker map[string]float64

func (c cacheAsker) Replace(_ context.Context, old, new string) (float64, error) {
	p, ok := c[cascade.State(old, new)]
	if !ok {
		return 0, fmt.Errorf("no cached answer")
	}
	return p, nil
}

// chooseBand picks the band on the calibration split: first the band with the
// fewest Laya decisions outside it that are wrong, then the fewest escalations.
func chooseBand(calib []supersede.Pair, laya cacheAsker, lcal cascade.Platt) (float64, float64, int, int) {
	bestLo, bestHi := 0.5, 0.5
	bestErr, bestEsc := math.MaxInt, math.MaxInt
	for li := 0; li <= 19; li++ {
		lo := float64(li) * 0.05
		for hi := lo; hi <= 1.0001; hi += 0.05 {
			errOut, esc := 0, 0
			for _, p := range calib {
				if !gateApplies(p) {
					continue
				}
				raw, _ := laya.Replace(context.Background(), p.Old, p.New)
				q := lcal.Apply(raw)
				switch {
				case q >= hi:
					if !p.Supersede {
						errOut++
					}
				case q < lo:
					if p.Supersede {
						errOut++
					}
				default:
					esc++
				}
			}
			if errOut < bestErr || (errOut == bestErr && esc < bestEsc) {
				bestErr, bestEsc, bestLo, bestHi = errOut, esc, lo, hi
			}
		}
	}
	return bestLo, bestHi, bestErr, bestEsc
}

// gateApplies is true for the pairs the gate decides: contested under the
// rules with the flag on.
func gateApplies(p supersede.Pair) bool { return supersede.RulesVerdict(p, true).Contested }

// arm is one column of the report.
type arm struct {
	name   string
	decide func(p supersede.Pair) (sup bool, source string)
}

func cascadeCmd(pairs []supersede.Pair, laya, jev cacheAsker) {
	// Platt fits on calibration only.
	var lRaw, jRaw []float64
	var cy []bool
	for _, p := range pairs {
		if p.Split != supersede.Calib || !gateApplies(p) {
			continue
		}
		lr, _ := laya.Replace(context.Background(), p.Old, p.New)
		jr, _ := jev.Replace(context.Background(), p.Old, p.New)
		lRaw = append(lRaw, lr)
		jRaw = append(jRaw, jr)
		cy = append(cy, p.Supersede)
	}
	lcal := FitPlatt(lRaw, cy)
	jcal := FitPlatt(jRaw, cy)
	var calibPairs []supersede.Pair
	for _, p := range pairs {
		if p.Split == supersede.Calib {
			calibPairs = append(calibPairs, p)
		}
	}
	lo, hi, errOut, escCalib := chooseBand(calibPairs, laya, cascade.Platt{A: lcal.A, B: lcal.B})
	fmt.Printf("laya calibration (fit on calib gate pairs): a=%.3f b=%.3f\n", lcal.A, lcal.B)
	fmt.Printf("jev calibration  (fit on calib gate pairs): a=%.3f b=%.3f\n", jcal.A, jcal.B)
	fmt.Printf("uncertain band chosen on calib: lo=%.2f hi=%.2f (laya errors outside band=%d, calib escalations=%d of %d gate pairs)\n",
		lo, hi, errOut, escCalib, len(cy))

	gate := cascade.Gate{
		Laya: laya, LayaCal: cascade.Platt{A: lcal.A, B: lcal.B},
		Jev: jev, JevCal: cascade.Platt{A: jcal.A, B: jcal.B},
		Lo: lo, Hi: hi,
	}
	layaOnly := cascade.Gate{Laya: laya, LayaCal: cascade.Platt{A: lcal.A, B: lcal.B}, Lo: 0.5, Hi: 0.5}
	jevOnly := cascade.Gate{Jev: jev, JevCal: cascade.Platt{A: jcal.A, B: jcal.B}}
	arms := []arm{
		{"(a) rules today", func(p supersede.Pair) (bool, string) {
			return supersede.RulesVerdict(p, false).Supersedes(), "rules"
		}},
		{"(b) rules+contested keep", func(p supersede.Pair) (bool, string) {
			return supersede.RulesVerdict(p, true).Supersedes(), "rules"
		}},
		{"(c) +Jev only", func(p supersede.Pair) (bool, string) {
			if !gateApplies(p) {
				return supersede.RulesVerdict(p, true).Supersedes(), "rules"
			}
			v := jevOnly.Decide(context.Background(), p.Old, p.New)
			return v.Replace, v.Source
		}},
		{"(d) +Laya only (p>=0.5)", func(p supersede.Pair) (bool, string) {
			if !gateApplies(p) {
				return supersede.RulesVerdict(p, true).Supersedes(), "rules"
			}
			v := layaOnly.Decide(context.Background(), p.Old, p.New)
			if v.Source == "fallback" {
				return false, "fallback"
			}
			return v.P >= 0.5, v.Source
		}},
		{"(e) cascade", func(p supersede.Pair) (bool, string) {
			if !gateApplies(p) {
				return supersede.RulesVerdict(p, true).Supersedes(), "rules"
			}
			v := gate.Decide(context.Background(), p.Old, p.New)
			return v.Replace, v.Source
		}},
	}
	var testPairs []supersede.Pair
	for _, p := range pairs {
		if p.Split == supersede.Test {
			testPairs = append(testPairs, p)
		}
	}
	fmt.Printf("\nTEST split, n=%d per category (%d gate pairs). Rates: false-supersession on keep-truth pairs, missed-update on supersede-truth pairs; 95%% bootstrap CI in brackets.\n",
		len(testPairs)/len(supersede.Categories), countGate(testPairs))
	for _, a := range arms {
		sources := map[string]int{}
		rows := supersede.Tally(testPairs, func(p supersede.Pair) bool {
			s, src := a.decide(p)
			if gateApplies(p) {
				sources[src]++
			}
			return s
		})
		fmt.Printf("\n%s   gate sources on test: %v\n", a.name, sources)
		for _, r := range rows {
			fa := supersede.Bootstrap95(r.FalseSup, r.NKeep, 7)
			mi := supersede.Bootstrap95(r.MissedUpd, r.NSup, 9)
			fmt.Printf("  %-20s keep=%3d false=%.3f [%.3f,%.3f]  sup=%3d miss=%.3f [%.3f,%.3f]\n",
				r.Category, r.NKeep, r.FalseRate, fa[0], fa[1], r.NSup, r.MissRate, mi[0], mi[1])
		}
	}
}

func countGate(ps []supersede.Pair) int {
	n := 0
	for _, p := range ps {
		if gateApplies(p) {
			n++
		}
	}
	return n
}
