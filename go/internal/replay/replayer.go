package replay

import (
	"runtime"
	"sort"
	"sync"
	"time"
)

// UsefulWeight is the outcome weight from which a memory that fired counts as
// having been useful, and so as something a change must not lose.
const UsefulWeight = 0.5

// Situation is a remembered moment: what the agent was asked or about to do,
// the stage it happened at, and which memories fired for it and how that
// went. Nothing here is richer than what the injection and outcome logs hold,
// plus the request text the hook sent (bounded, see Store).
type Situation struct {
	ID     string
	Text   string
	Stage  string
	MinRel float64
	Limit  int
	// Budget is the response byte budget (0 = the stage's default).
	Budget int
	Source string // "live" or "seed"
	N      int    // times seen
	First  int64
	Last   int64
	// Expect maps a memory target to its outcome weight: 1 cited, 0.7
	// followed, 0 fired and was ignored or contradicted. Weights from
	// UsefulWeight up are what a change must keep firing.
	Expect map[string]float64
	// Avoid names memories that must NOT fire here (seed near-misses).
	Avoid []string
	Vec   []float32
}

// Useful returns the targets that fired usefully.
func (s Situation) Useful() []string {
	var out []string
	for t, w := range s.Expect {
		if w >= UsefulWeight {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// Move is one memory's fate in one situation.
type Move struct {
	Target string  `json:"target"`
	Was    int     `json:"was,omitempty"`    // 1-based rank among fired memories before; 0 = not firing
	Now    int     `json:"now,omitempty"`    // after
	Before float64 `json:"before"`           // relevance before
	After  float64 `json:"after"`            // relevance after
	Weight float64 `json:"weight,omitempty"` // outcome weight, for lost
	Why    string  `json:"why,omitempty"`    // lost: removed, below_min_rel, displaced
}

// SituationDiff is what changed for one situation.
type SituationDiff struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Stage  string `json:"stage"`
	Source string `json:"source"`
	Lost   []Move `json:"lost,omitempty"`
	Gained []Move `json:"gained,omitempty"`
	Moved  []Move `json:"moved,omitempty"`
	// AtRisk lists useful memories that still fire but whose relevance fell
	// by RiskDrop or more and now sits within RiskBand of the floor: the next
	// request worded a little differently will miss.
	AtRisk []Move `json:"at_risk,omitempty"`
	// FalseFire marks gained memories that fire where nothing useful fired
	// before, or that a seed case says must not fire.
	FalseFire []string `json:"false_fire,omitempty"`
}

// Report is the result of one replay.
type Report struct {
	Situations int `json:"situations"`
	// Checked is the situations with at least one useful memory that fires
	// on the current store: the ones that can show a loss.
	Checked   int `json:"checked"`
	Unchanged int `json:"unchanged"`

	LostSituations int             `json:"lost_situations"`
	LostMemories   int             `json:"lost_memories"`
	Gained         int             `json:"gained"`
	NewFalseFires  int             `json:"new_false_fires"`
	Moved          int             `json:"moved"`
	AtRisk         int             `json:"at_risk"`
	UsefulBefore   float64         `json:"useful_before"` // weight of useful recalls that fired before
	UsefulKept     float64         `json:"useful_kept"`
	Score          float64         `json:"score"` // kept / before, 1 when nothing was at stake
	Diffs          []SituationDiff `json:"diffs,omitempty"`
	DiffsTruncated bool            `json:"diffs_truncated,omitempty"`
	Millis         int64           `json:"ms"`
}

// A useful memory that still fires is "at risk" when its relevance fell by
// RiskDrop or more and now sits within RiskBand above min_rel.
const (
	RiskDrop = 0.08
	RiskBand = 0.10
)

// MaxDiffs bounds the per-situation detail in a report.
const MaxDiffs = 200

// Replayer scores a corpus against a base snapshot once and compares
// proposed snapshots against it by rescoring only the memories they touch.
type Replayer struct {
	mu   sync.Mutex
	base *Snapshot
	sits []*Prepared
	rank [][]Scored // per situation, strongest first, against base
	fire [][]Scored
}

// NewReplayer scores every situation against base (in parallel).
func NewReplayer(base *Snapshot, sits []Situation) *Replayer {
	r := &Replayer{base: base, sits: make([]*Prepared, 0, len(sits))}
	for _, s := range sits {
		if len(s.Vec) == 0 {
			continue // not embedded: nothing to score
		}
		r.sits = append(r.sits, Prepare(s))
	}
	r.rank = make([][]Scored, len(r.sits))
	r.fire = make([][]Scored, len(r.sits))
	parallel(len(r.sits), func(i int) {
		r.rank[i] = base.Score(r.sits[i], nil)
		r.fire[i] = base.Fire(r.sits[i], r.rank[i])
	})
	return r
}

func parallel(n int, f func(i int)) {
	workers := runtime.GOMAXPROCS(0)
	if workers > n {
		workers = n
	}
	if workers < 1 {
		return
	}
	var wg sync.WaitGroup
	next := make(chan int, n)
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f(i)
			}
		}()
	}
	wg.Wait()
}

// Base is the snapshot the replayer compares against.
func (r *Replayer) Base() *Snapshot { return r.base }

// Situations is how many situations are scored.
func (r *Replayer) Situations() int { return len(r.sits) }

// scoresAfter derives a situation's ranking in the changed snapshot from its
// ranking in the base: everything the change did not touch keeps its score.
func scoresAfter(base []Scored, after *Snapshot, p *Prepared, touched map[string]bool) []Scored {
	out := make([]Scored, 0, len(base)+len(touched))
	for _, sc := range base {
		if !touched[sc.Target] {
			out = append(out, sc)
		}
	}
	out = append(out, after.Score(p, touched)...)
	sortScored(out)
	return out
}

// Diff compares the base with the base after change.
func (r *Replayer) Diff(c Change) (*Report, *Snapshot) {
	start := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	after := r.base.Overlay(c)
	touched := c.Touched()
	n := len(r.sits)
	diffs := make([]*SituationDiff, n)
	type tally struct {
		before, kept     float64
		checked, lostSit bool
	}
	tallies := make([]tally, n)
	parallel(n, func(i int) {
		p := r.sits[i]
		rankB := scoresAfter(r.rank[i], after, p, touched)
		fireB := after.Fire(p, rankB)
		d, t := compare(r.base, after, p, r.rank[i], r.fire[i], rankB, fireB, c)
		diffs[i] = d
		tallies[i] = tally{t.before, t.kept, t.checked, t.lost}
	})
	rep := &Report{Situations: n}
	for i, d := range diffs {
		t := tallies[i]
		rep.UsefulBefore += t.before
		rep.UsefulKept += t.kept
		if t.checked {
			rep.Checked++
		}
		if d == nil {
			rep.Unchanged++
			continue
		}
		if len(d.Lost) > 0 {
			rep.LostSituations++
			rep.LostMemories += len(d.Lost)
		}
		rep.Gained += len(d.Gained)
		rep.NewFalseFires += len(d.FalseFire)
		rep.Moved += len(d.Moved)
		rep.AtRisk += len(d.AtRisk)
		rep.Diffs = append(rep.Diffs, *d)
	}
	rep.Score = 1
	if rep.UsefulBefore > 0 {
		rep.Score = rep.UsefulKept / rep.UsefulBefore
	}
	// Worst first: situations that lost something, most valuable loss first.
	sort.SliceStable(rep.Diffs, func(i, j int) bool {
		li, lj := lossWeight(rep.Diffs[i]), lossWeight(rep.Diffs[j])
		if li != lj {
			return li > lj
		}
		return len(rep.Diffs[i].FalseFire) > len(rep.Diffs[j].FalseFire)
	})
	if len(rep.Diffs) > MaxDiffs {
		rep.Diffs, rep.DiffsTruncated = rep.Diffs[:MaxDiffs], true
	}
	rep.Millis = time.Since(start).Milliseconds()
	return rep, after
}

func lossWeight(d SituationDiff) float64 {
	var w float64
	for _, m := range d.Lost {
		w += m.Weight
	}
	return w
}

// Commit makes after (returned by Diff) the new base, rescoring only what the
// change touched. Gating a series of changes in order uses this.
func (r *Replayer) Commit(c Change, after *Snapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	touched := c.Touched()
	parallel(len(r.sits), func(i int) {
		r.rank[i] = scoresAfter(r.rank[i], after, r.sits[i], touched)
		r.fire[i] = after.Fire(r.sits[i], r.rank[i])
	})
	r.base = after
}

type verdict struct {
	before, kept  float64
	checked, lost bool
}

func rankOf(fired []Scored, t string) int {
	for i, f := range fired {
		if f.Target == t {
			return i + 1
		}
	}
	return 0
}

func scoreOf(ranked []Scored, t string) float64 {
	for _, s := range ranked {
		if s.Target == t {
			return s.Score
		}
	}
	return 0
}

// compare is one situation's diff. Identity is followed through the change's
// Remap, so a merge of A into B counts as A kept when B fires.
func compare(before, after *Snapshot, p *Prepared, rankA []Scored, fireA []Scored, rankB, fireB []Scored, c Change) (*SituationDiff, verdict) {
	var v verdict
	d := &SituationDiff{ID: p.ID, Text: p.Text, Stage: p.Stage, Source: p.Source}
	mapped := func(t string) string { return c.resolve(t) }
	firedB := map[string]bool{}
	for _, f := range fireB {
		firedB[f.Target] = true
	}
	afterScore := map[string]float64{}
	for _, f := range fireB {
		afterScore[f.Target] = f.Score
	}
	firedA := map[string]bool{} // by identity in the new store
	for _, f := range fireA {
		firedA[mapped(f.Target)] = true
	}
	for _, f := range fireA {
		w := p.Expect[f.Target]
		if w < UsefulWeight {
			continue
		}
		v.checked = true
		v.before += w
		nt := mapped(f.Target)
		if firedB[nt] {
			v.kept += w
			if a := afterScore[nt]; f.Score-a >= RiskDrop && a < p.MinRel+RiskBand {
				d.AtRisk = append(d.AtRisk, Move{Target: f.Target, Was: rankOf(fireA, f.Target), Now: rankOf(fireB, nt), Before: f.Score, After: a, Weight: w})
			}
			continue
		}
		v.lost = true
		m := Move{Target: f.Target, Was: rankOf(fireA, f.Target), Before: f.Score, Weight: w}
		switch {
		case !after.Has(nt):
			m.Why = "removed"
		default:
			m.After = scoreOf(rankB, nt)
			if m.After < p.MinRel {
				m.Why = "below_min_rel"
			} else {
				m.Why = "displaced"
			}
		}
		if m.After == 0 && after.Has(nt) {
			m.After = scoreOf(rankB, nt)
		}
		d.Lost = append(d.Lost, m)
	}
	nothingUseful := len(p.Useful()) == 0
	avoid := map[string]bool{}
	for _, a := range p.Avoid {
		avoid[a] = true
	}
	for _, f := range fireB {
		if firedA[f.Target] {
			// Still fires: note a rank change of a useful memory.
			if rb, ra := rankOf(fireB, f.Target), rankOf(fireAByNew(fireA, c), f.Target); ra != rb {
				if w := usefulWeightFor(p, f.Target, c); w >= UsefulWeight {
					d.Moved = append(d.Moved, Move{Target: f.Target, Was: ra, Now: rb, Before: scoreOf(rankA, f.Target), After: f.Score, Weight: w})
				}
			}
			continue
		}
		g := Move{Target: f.Target, Now: rankOf(fireB, f.Target), After: f.Score}
		d.Gained = append(d.Gained, g)
		if nothingUseful || avoid[f.Target] {
			d.FalseFire = append(d.FalseFire, f.Target)
		}
	}
	if len(d.Lost) == 0 && len(d.Gained) == 0 && len(d.Moved) == 0 && len(d.AtRisk) == 0 {
		return nil, v
	}
	return d, v
}

// fireAByNew renames the previously firing memories to their identity in the
// new store, keeping order, so ranks compare like for like.
func fireAByNew(fireA []Scored, c Change) []Scored {
	out := make([]Scored, len(fireA))
	for i, f := range fireA {
		out[i] = Scored{Target: c.resolve(f.Target), Score: f.Score}
	}
	return out
}

func usefulWeightFor(p *Prepared, newTarget string, c Change) float64 {
	best := p.Expect[newTarget]
	for old, w := range p.Expect {
		if c.resolve(old) == newTarget && w > best {
			best = w
		}
	}
	return best
}

// FireTargets returns, per situation id, the memories that fire on the base
// snapshot. It exists for evaluation and tests.
func (r *Replayer) FireTargets() map[string][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string][]string, len(r.sits))
	for i, p := range r.sits {
		for _, f := range r.fire[i] {
			out[p.ID] = append(out[p.ID], f.Target)
		}
		if _, ok := out[p.ID]; !ok {
			out[p.ID] = nil
		}
	}
	return out
}

// Cosine is the cosine similarity of two vectors (unit length not required).
func Cosine(a, b []float32) float64 {
	na, nb := Unit(a), Unit(b)
	return dot(na, nb)
}
