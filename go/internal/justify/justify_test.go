package justify

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

func openStore(t testing.TB) (*Store, *db.DB) {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	// Every operation commits; the test checks logic, not durability.
	if err := d.Exec("PRAGMA synchronous=OFF"); err != nil {
		t.Fatal(err)
	}
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	return s, d
}

func P(pairs ...string) []Premise {
	var out []Premise
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Premise{ID: pairs[i], Hash: pairs[i+1]})
	}
	return out
}

func mustDerive(t *testing.T, s *Store, id, hash string, js ...[]Premise) {
	t.Helper()
	if err := s.Derive(id, "observation", hash, js...); err != nil {
		t.Fatalf("derive %s: %v", id, err)
	}
}

func mustInvalidate(t *testing.T, s *Store, ids ...string) []string {
	t.Helper()
	lost, err := s.Invalidate(ids...)
	if err != nil {
		t.Fatalf("invalidate %v: %v", ids, err)
	}
	return lost
}

func valid(t *testing.T, s *Store, id string) bool {
	t.Helper()
	n, ok, err := s.Get(id)
	if err != nil || !ok {
		t.Fatalf("get %s: ok=%v err=%v", id, ok, err)
	}
	return n.Valid
}

func TestAlternativeJustificationSurvives(t *testing.T) {
	s, _ := openStore(t)
	mustDerive(t, s, "d", "dh", P("a", "1"), P("b", "1"))
	mustDerive(t, s, "e", "eh", P("d", "dh"))

	if lost := mustInvalidate(t, s, "a"); len(lost) != 0 {
		t.Fatalf("forgetting one of two justifications lost %v, want nothing", lost)
	}
	if !valid(t, s, "d") || !valid(t, s, "e") {
		t.Fatal("d or e lost validity though a second justification of d survives")
	}
	st, err := s.Stale(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(st) != 1 || st[0].ID != "d" || !reflect.DeepEqual(st[0].Reasons, []string{"premise_gone:a"}) {
		t.Fatalf("stale after forgetting a = %+v, want d with premise_gone:a", st)
	}

	lost := mustInvalidate(t, s, "b")
	if !reflect.DeepEqual(lost, []string{"d", "e"}) {
		t.Fatalf("forgetting the last justification lost %v, want [d e]", lost)
	}
	if n, _, _ := s.Get("d"); n.Valid || n.Reason != "premise_gone:a" {
		t.Fatalf("d = %+v, want invalid with a reason", n)
	}
}

func TestEarlyCutoff(t *testing.T) {
	s, _ := openStore(t)
	mustDerive(t, s, "d", "dh", P("a", "1"))
	mustDerive(t, s, "e", "eh", P("d", "dh"))
	mustDerive(t, s, "f", "fh", P("e", "eh"))

	lost, err := s.Recompute("d", "dh")
	if err != nil || len(lost) != 0 {
		t.Fatalf("same-hash recompute: lost=%v err=%v", lost, err)
	}
	if s.Evals != 0 {
		t.Fatalf("same-hash recompute re-evaluated %d items, want 0", s.Evals)
	}
	lost, err = s.Recompute("a", "1")
	if err != nil || len(lost) != 0 || s.Evals != 0 {
		t.Fatalf("same-hash premise recompute: lost=%v evals=%d err=%v", lost, s.Evals, err)
	}

	// A changed hash does reach the dependents, and each of them is lost
	// because its justification recorded the old hash.
	lost, err = s.Recompute("d", "dh2")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lost, []string{"e", "f"}) {
		t.Fatalf("changed-hash recompute lost %v, want [e f]", lost)
	}
	// Re-deriving e on the new hash of d revives it. Its own hash did not
	// change, so f, which cites that hash, is valid again as well.
	mustDerive(t, s, "e", "eh", P("d", "dh2"))
	if !valid(t, s, "e") || !valid(t, s, "f") {
		t.Fatal("re-derivation did not revive e and, through its unchanged hash, f")
	}
}

func TestCycleRefused(t *testing.T) {
	s, _ := openStore(t)
	mustDerive(t, s, "d1", "h", P("d2", "h"))
	if err := s.Derive("d2", "observation", "h", P("d1", "h")); err != ErrCycle {
		t.Fatalf("cycle returned %v, want ErrCycle", err)
	}
	if err := s.Derive("d3", "observation", "h", P("d3", "h")); err != ErrCycle {
		t.Fatalf("self-loop returned %v, want ErrCycle", err)
	}
}

func TestPlanDoesNotWrite(t *testing.T) {
	s, _ := openStore(t)
	mustDerive(t, s, "d", "dh", P("a", "1"))
	mustDerive(t, s, "e", "eh", P("d", "dh"))
	plan, err := s.Plan("a")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan, []string{"d", "e"}) {
		t.Fatalf("plan = %v, want [d e]", plan)
	}
	if !valid(t, s, "d") || !valid(t, s, "e") {
		t.Fatal("plan changed the store")
	}
	if got := mustInvalidate(t, s, "a"); !reflect.DeepEqual(got, plan) {
		t.Fatalf("invalidate %v differs from plan %v", got, plan)
	}
}

// oracle is the from-scratch reference: the least fixpoint of the validity
// equations, computed from the recorded graph and the current world state
// with no incremental bookkeeping at all.
type oracle struct {
	base    map[string]bool // ids that are base premises
	present map[string]bool
	hash    map[string]string
	just    map[string][][]Premise // derived id -> justifications as recorded
}

func (o *oracle) valid() map[string]bool {
	v := map[string]bool{}
	for id := range o.base {
		v[id] = o.present[id]
	}
	for changed := true; changed; {
		changed = false
		for d, js := range o.just {
			if v[d] || !o.present[d] {
				continue
			}
			for _, j := range js {
				ok := true
				for _, p := range j {
					if !v[p.ID] || o.hash[p.ID] != p.Hash {
						ok = false
						break
					}
				}
				if ok {
					v[d] = true
					changed = true
					break
				}
			}
		}
	}
	for d := range o.just {
		if _, ok := v[d]; !ok {
			v[d] = false
		}
	}
	return v
}

// lost is the set of derived items valid before and not after, excluding
// the forgotten ids, as Invalidate defines it.
func (o *oracle) lost(before map[string]bool, forgot ...string) []string {
	skip := map[string]bool{}
	for _, f := range forgot {
		skip[f] = true
	}
	after := o.valid()
	var out []string
	for d := range o.just {
		if before[d] && !after[d] && !skip[d] {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func (o *oracle) stale() []string {
	v := o.valid()
	var out []string
	for d, js := range o.just {
		bad := !v[d]
		for _, j := range js {
			for _, p := range j {
				if !o.present[p.ID] || o.hash[p.ID] != p.Hash {
					bad = true
				}
			}
		}
		if bad {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

func sameSet(a, b []string) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	return reflect.DeepEqual(a, b)
}

// TestInvalidationMatchesRecompute generates random derivation graphs and
// random sequences of forgets, content changes and plans, and checks after
// every step that the incremental store agrees with the from-scratch oracle
// on validity, on the lost set, and on the stale set.
func TestInvalidationMatchesRecompute(t *testing.T) {
	s, d := openStore(t)
	const cases = 2000
	rng := rand.New(rand.NewSource(20261011))
	hashes := []string{"h0", "h1", "h2"}
	var ops, nonEmptyLost, totalLost, evals int
	start := time.Now()
	for c := 0; c < cases; c++ {
		for _, tbl := range []string{"justify_just", "justify_node"} {
			if err := d.Exec("DELETE FROM " + tbl); err != nil {
				t.Fatal(err)
			}
		}
		o := &oracle{base: map[string]bool{}, present: map[string]bool{}, hash: map[string]string{}, just: map[string][][]Premise{}}
		nb := 2 + rng.Intn(4)
		nd := 2 + rng.Intn(14)
		var names []string
		for i := 0; i < nb; i++ {
			id := fmt.Sprintf("b%d", i)
			o.base[id], o.present[id], o.hash[id] = true, true, "h0"
			names = append(names, id)
		}
		for i := 0; i < nd; i++ {
			id := fmt.Sprintf("d%d", i)
			o.present[id], o.hash[id] = true, "h0"
			var js [][]Premise
			for k := 1 + rng.Intn(3); k > 0; k-- {
				n := min(1+rng.Intn(3), len(names))
				seen := map[string]bool{}
				var j []Premise
				for len(j) < n {
					p := names[rng.Intn(len(names))]
					if seen[p] {
						continue
					}
					seen[p] = true
					j = append(j, Premise{ID: p, Hash: o.hash[p]})
				}
				js = append(js, j)
			}
			if err := s.Derive(id, "observation", "h0", js...); err != nil {
				t.Fatalf("case %d derive %s: %v", c, id, err)
			}
			o.just[id] = js
			names = append(names, id)
		}
		for step := 0; step < 1+rng.Intn(25); step++ {
			ops++
			x := names[rng.Intn(len(names))]
			before := o.valid()
			switch rng.Intn(3) {
			case 0: // forget
				o.present[x] = false
				want := o.lost(before, x)
				got, err := s.Invalidate(x)
				if err != nil {
					t.Fatal(err)
				}
				if !sameSet(got, want) {
					t.Fatalf("case %d step %d: invalidate %s = %v, from-scratch = %v", c, step, x, got, want)
				}
				totalLost += len(got)
				if len(got) > 0 {
					nonEmptyLost++
				}
			case 1: // content change
				h := hashes[rng.Intn(len(hashes))]
				old := o.hash[x]
				o.hash[x] = h
				want := o.lost(before)
				got, err := s.Recompute(x, h)
				if err != nil {
					t.Fatal(err)
				}
				if !sameSet(got, want) {
					t.Fatalf("case %d step %d: recompute %s to %s = %v, from-scratch = %v", c, step, x, h, got, want)
				}
				if h == old && (len(got) != 0 || s.Evals != 0) {
					t.Fatalf("case %d step %d: same-hash recompute of %s touched %d items", c, step, x, s.Evals)
				}
				evals += s.Evals
				totalLost += len(got)
				if len(got) > 0 {
					nonEmptyLost++
				}
			default: // plan: must report the same set and write nothing
				f := *o
				f.present = map[string]bool{}
				for k, v := range o.present {
					f.present[k] = v
				}
				f.present[x] = false
				want := f.lost(before, x)
				got, err := s.Plan(x)
				if err != nil {
					t.Fatal(err)
				}
				if !sameSet(got, want) {
					t.Fatalf("case %d step %d: plan %s = %v, from-scratch = %v", c, step, x, got, want)
				}
			}
			// The stored validity of every item must equal the oracle's.
			v := o.valid()
			for id, want := range v {
				n, ok, err := s.Get(id)
				if err != nil {
					t.Fatal(err)
				}
				if !ok {
					continue // an unreferenced base id: no row, no effect
				}
				if n.Valid != want {
					t.Fatalf("case %d step %d: %s valid=%v, from-scratch=%v", c, step, id, n.Valid, want)
				}
			}
			// The stale set must agree too.
			cur := func(id string) (string, bool) {
				if !o.present[id] {
					return "", false
				}
				return o.hash[id], true
			}
			st, err := s.Stale(cur)
			if err != nil {
				t.Fatal(err)
			}
			var gotStale []string
			for _, it := range st {
				gotStale = append(gotStale, it.ID)
			}
			sort.Strings(gotStale)
			if want := o.stale(); !sameSet(gotStale, want) {
				t.Fatalf("case %d step %d: stale = %v, from-scratch = %v", c, step, gotStale, want)
			}
		}
	}
	t.Logf("cases=%d ops=%d nonEmptyLost=%d totalLost=%d reevals=%d elapsed=%v",
		cases, ops, nonEmptyLost, totalLost, evals, time.Since(start).Round(time.Millisecond))
	if nonEmptyLost < cases/10 {
		t.Fatalf("generator too weak: only %d operations lost anything", nonEmptyLost)
	}
}
