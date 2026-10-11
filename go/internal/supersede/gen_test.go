package supersede

import (
	"strings"
	"testing"
)

func TestGenerateIsDeterministicAndCounted(t *testing.T) {
	a := Generate(7, 10)
	b := Generate(7, 10)
	if len(a) != len(Categories)*3*10 {
		t.Fatalf("got %d pairs, want %d", len(a), len(Categories)*3*10)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("pair %d differs between runs with the same seed", i)
		}
	}
}

func TestTruthOnlyForTrueUpdates(t *testing.T) {
	for _, p := range Generate(3, 20) {
		want := p.Category == CatTrueUpdate || p.Category == CatParaUpdate
		if p.Supersede != want {
			t.Fatalf("%s labelled supersede=%v", p.Category, p.Supersede)
		}
	}
}

// No statement appears in two splits, so a template or a subject cannot leak
// from training into the held-out test split.
func TestSplitsShareNoStatement(t *testing.T) {
	seen := map[string]string{}
	for _, p := range Generate(11, 40) {
		for _, s := range []string{p.Old, p.New} {
			if prev, ok := seen[s]; ok && prev != p.Split {
				t.Fatalf("statement %q appears in %s and %s", s, prev, p.Split)
			}
			seen[s] = p.Split
		}
	}
}

// Subjects are disjoint across splits as well.
func TestSubjectPoolsAreDisjoint(t *testing.T) {
	owner := map[string]string{}
	for split, pl := range pools {
		for _, s := range pl.subjects {
			if o, ok := owner[s]; ok && o != split {
				t.Fatalf("subject %q in %s and %s", s, o, split)
			}
			owner[s] = split
		}
	}
}

func TestPairsAreASCIIAndNonEmpty(t *testing.T) {
	for _, p := range Generate(5, 10) {
		if p.Old == "" || p.New == "" {
			t.Fatalf("empty statement in %+v", p)
		}
		for _, r := range p.Old + p.New {
			if r > 127 {
				t.Fatalf("non-ASCII rune %q in %q", r, p.Old+p.New)
			}
		}
		if strings.Contains(p.Old, "{") || strings.Contains(p.New, "{") {
			t.Fatalf("unfilled template in %+v", p)
		}
	}
}
