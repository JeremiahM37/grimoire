package supersede

import (
	"math"
	"math/rand"
	"sort"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Verdict is what the write-time rules do with one pair.
type Verdict struct {
	Op        memory.Op
	Contested bool // the rules deferred a supersession (cascade flag on)
}

// Supersedes reports whether the verdict strikes the old statement through.
func (v Verdict) Supersedes() bool { return v.Op == memory.OpUpdate || v.Op == memory.OpDelete }

// RulesVerdict runs the rule path (memory.DecideAs, no model) on one pair with
// the cascade flag either off (rules today) or on (rules + contested fallback).
func RulesVerdict(p Pair, cascade bool) Verdict {
	var restore func()
	if cascade {
		restore = exp.Set("cascade")
	} else {
		restore = exp.Set()
	}
	defer restore()
	d := memory.DecideAs(p.New, "", false, []memory.Entry{{ID: "old", Text: p.Old}})
	return Verdict{Op: d.Op, Contested: d.Contested != ""}
}

// Row is one category's numbers on one split for one arm.
type Row struct {
	Category  string
	N         int     // pairs in the category
	NSup      int     // of which truth = supersede
	NKeep     int     // of which truth = keep both
	FalseSup  int     // keep-truth pairs that were superseded (false supersession)
	MissedUpd int     // supersede-truth pairs that were not superseded (missed update)
	FalseRate float64 // FalseSup / NKeep
	MissRate  float64 // MissedUpd / NSup
	FalseCI   [2]float64
	MissCI    [2]float64
}

// Tally counts a verdicting function over pairs, grouped by category.
func Tally(pairs []Pair, verdict func(Pair) bool) []Row {
	byCat := map[string]*Row{}
	for _, p := range pairs {
		r := byCat[p.Category]
		if r == nil {
			r = &Row{Category: p.Category}
			byCat[p.Category] = r
		}
		r.N++
		sup := verdict(p)
		if p.Supersede {
			r.NSup++
			if !sup {
				r.MissedUpd++
			}
		} else {
			r.NKeep++
			if sup {
				r.FalseSup++
			}
		}
	}
	var out []Row
	for _, c := range Categories {
		r := byCat[c]
		if r == nil {
			continue
		}
		if r.NKeep > 0 {
			r.FalseRate = float64(r.FalseSup) / float64(r.NKeep)
		}
		if r.NSup > 0 {
			r.MissRate = float64(r.MissedUpd) / float64(r.NSup)
		}
		out = append(out, *r)
	}
	return out
}

// Bootstrap95 is a percentile bootstrap interval for a proportion of k events
// out of n trials, with a fixed seed so the report is reproducible.
func Bootstrap95(k, n int, seed int64) [2]float64 {
	if n == 0 {
		return [2]float64{0, 0}
	}
	rng := rand.New(rand.NewSource(seed))
	const B = 2000
	vals := make([]float64, B)
	for b := 0; b < B; b++ {
		hits := 0
		for i := 0; i < n; i++ {
			// Bernoulli(k/n) resample of the n outcomes.
			if rng.Float64() < float64(k)/float64(n) {
				hits++
			}
		}
		vals[b] = float64(hits) / float64(n)
	}
	sort.Float64s(vals)
	return [2]float64{vals[int(0.025*B)], vals[int(math.Min(0.975*B, B-1))]}
}
