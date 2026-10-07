package bank

import (
	"math"
	"runtime"
	"sort"
	"sync"
)

// Links between facts.
//
// Three kinds connect facts, besides the entities they share:
//
//	temporal — facts of the same type close in time, weight max(0.3, 1 − Δhours/24)
//	semantic — facts of the same type whose meanings are close, weight = cosine ≥ 0.7
//	causal   — "this happened because of that", as the extractor reported it, weight 1
//
// Only causal links are stored, in the fact trailer and in bank_links. The
// other two are pure functions of the facts and are computed from the
// in-memory bank when recall needs them. Materialising them would mean
// rewriting up to forty rows per fact on every retain and an all-pairs pass
// on every rebuild — O(n²) for semantic links, which at twenty thousand facts
// is minutes — to store numbers that are cheaper to recompute for the few
// facts a query actually touches.

const (
	temporalLinksPerUnit = 20
	temporalWindowHours  = 24.0
	temporalMinWeight    = 0.3
	semanticLinksPerUnit = 20
	semanticLinkMin      = 0.7
)

// temporalWeight is the strength of a time link between two instants.
func temporalWeight(aMS, bMS int64) float64 {
	h := math.Abs(float64(aMS-bMS)) / 3.6e6
	return math.Max(temporalMinWeight, 1-h/temporalWindowHours)
}

// temporalNeighbors are the facts of the same type nearest in time, nearest
// first, at most temporalLinksPerUnit of them.
func (c *bankCache) temporalNeighbors(pos int32) []neighbor {
	u := &c.units[pos]
	t := u.eventMS()
	p := c.timePos[pos]
	if t == 0 || p < 0 {
		return nil
	}
	list := c.byTime[u.Type]
	lo, hi := int(p)-1, int(p)+1
	out := make([]neighbor, 0, temporalLinksPerUnit)
	for len(out) < temporalLinksPerUnit && (lo >= 0 || hi < len(list)) {
		var pick int
		switch {
		case lo < 0:
			pick, hi = hi, hi+1
		case hi >= len(list):
			pick, lo = lo, lo-1
		default:
			dl := t - c.units[list[lo]].eventMS()
			dh := c.units[list[hi]].eventMS() - t
			if dl <= dh {
				pick, lo = lo, lo-1
			} else {
				pick, hi = hi, hi+1
			}
		}
		q := list[pick]
		out = append(out, neighbor{pos: q, weight: temporalWeight(t, c.units[q].eventMS())})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].weight > out[b].weight })
	return out
}

// semanticNeighbors returns, for each seed, its nearest facts of the same type
// with cosine ≥ semanticLinkMin, strongest first and never itself. Results are
// memoised for the life of the cache — the cache is rebuilt on every write, so
// a memo can never be stale — and the seeds still missing are computed in one
// parallel pass over the vector matrix, each row read once for all of them.
func (c *bankCache) semanticNeighbors(seeds []int32) map[int32][]neighbor {
	out := make(map[int32][]neighbor, len(seeds))
	var todo []int32
	c.semMu.Lock()
	for _, s := range seeds {
		if n, ok := c.semMemo[s]; ok {
			out[s] = n
		} else if c.dim > 0 {
			todo = append(todo, s)
		}
	}
	c.semMu.Unlock()
	if len(todo) == 0 {
		return out
	}
	n := len(c.units)
	workers := min(runtime.GOMAXPROCS(0), max(1, n/2000))
	per := (n + workers - 1) / workers
	partial := make([][][]neighbor, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo, hi := w*per, min((w+1)*per, n)
		if lo >= hi {
			continue
		}
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			res := make([][]neighbor, len(todo))
			svecs := make([][]float32, len(todo))
			for k, s := range todo {
				svecs[k] = c.vec(s)
			}
			for i := lo; i < hi; i++ {
				row := c.vecs[i*c.dim : (i+1)*c.dim]
				for k, s := range todo {
					if int32(i) == s || c.units[i].Type != c.units[s].Type {
						continue
					}
					sim := float64(dot(row, svecs[k]))
					if sim >= semanticLinkMin {
						res[k] = append(res[k], neighbor{pos: int32(i), weight: math.Min(sim, 1)})
					}
				}
			}
			partial[w] = res
		}(w, lo, hi)
	}
	wg.Wait()
	c.semMu.Lock()
	defer c.semMu.Unlock()
	for k, s := range todo {
		var all []neighbor
		for _, p := range partial {
			if p != nil {
				all = append(all, p[k]...)
			}
		}
		sort.SliceStable(all, func(a, b int) bool {
			if all[a].weight != all[b].weight {
				return all[a].weight > all[b].weight
			}
			return all[a].pos < all[b].pos
		})
		if len(all) > semanticLinksPerUnit {
			all = all[:semanticLinksPerUnit]
		}
		c.semMemo[s] = all
		out[s] = all
	}
	return out
}
