package justify

import (
	"fmt"
	"math/rand"
	"testing"
)

// buildDAG records a layered derivation graph: layer 0 is base premises, and
// each later item cites 1-3 justifications of 1-3 earlier items. It returns the
// store and the item ids, base first.
func buildDAG(tb testing.TB, nBase, nDerived int, seed int64) (*Store, []string) {
	tb.Helper()
	s, _ := openStore(tb)
	rng := rand.New(rand.NewSource(seed))
	names := make([]string, 0, nBase+nDerived)
	for i := 0; i < nBase; i++ {
		names = append(names, fmt.Sprintf("b%d", i))
	}
	for i := 0; i < nDerived; i++ {
		id := fmt.Sprintf("d%d", i)
		var js [][]Premise
		for k := 1 + rng.Intn(3); k > 0; k-- {
			seen := map[string]bool{}
			var j []Premise
			for n := 1 + rng.Intn(3); len(j) < n; {
				p := names[rng.Intn(len(names))]
				if !seen[p] {
					seen[p] = true
					j = append(j, Premise{ID: p, Hash: "h0"})
				}
			}
			js = append(js, j)
		}
		if err := s.Derive(id, "observation", "h0", js...); err != nil {
			tb.Fatal(err)
		}
		names = append(names, id)
	}
	return s, names
}

// BenchmarkInvalidate forgets one random base premise of a 2000-item DAG per
// iteration and reports how many items the store invalidates. The comparison
// it supports is with a from-scratch recompute, which visits every item.
func BenchmarkInvalidate(b *testing.B) {
	s, names := buildDAG(b, 200, 2000, 1)
	items, derived, justs, edges, err := s.Size()
	if err != nil {
		b.Fatal(err)
	}
	b.Logf("graph: %d items (%d derived), %d justifications, %d premise edges", items, derived, justs, edges)
	rng := rand.New(rand.NewSource(2))
	var touched int
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		x := names[rng.Intn(200)]
		lost, err := s.Plan(x)
		if err != nil {
			b.Fatal(err)
		}
		touched += len(lost)
	}
	b.ReportMetric(float64(touched)/float64(b.N), "items/forget")
}
