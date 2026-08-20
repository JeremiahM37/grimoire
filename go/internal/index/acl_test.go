package index

import (
	"fmt"
	"strings"
	"testing"
)

// fixedSpaces assigns notes to spaces by path prefix, the way the auth package
// does, without the index depending on it.
type fixedSpaces map[string]string // prefix -> space

func (f fixedSpaces) SpaceOf(path string) string {
	best, bestLen := CommonsSpace, -1
	for prefix, space := range f {
		if strings.HasPrefix(path, prefix) && len(prefix) > bestLen {
			best, bestLen = space, len(prefix)
		}
	}
	return best
}

func aclIndex(t *testing.T) *Index {
	t.Helper()
	ix := testIndex(t)
	ix.Spaces = fixedSpaces{"alice/": "space-alice", "bob/": "space-bob"}
	for _, n := range []struct{ path, body string }{
		{"alice/secret-project.md", "# Kestrel\n\nthe kestrel migration plan, alice only"},
		{"alice/notes.md", "# Alice notes\n\nkestrel kestrel kestrel and more kestrel"},
		{"bob/secret-project.md", "# Bob's kestrel\n\nbob's own kestrel work"},
		{"shared.md", "# Shared\n\na kestrel mention in the commons"},
	} {
		write(t, ix, n.path, n.body)
		if _, err := ix.Upsert(n.path); err != nil {
			t.Fatal(err)
		}
	}
	return ix
}

func paths(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Path
	}
	return out
}

func TestRetrievalReturnsOnlyReadableSpaces(t *testing.T) {
	ix := aclIndex(t)

	all, err := ix.Retrieve("kestrel", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) < 4 {
		t.Fatalf("unfiltered retrieval got %v, want every note", paths(all))
	}

	alice, err := ix.RetrieveFor("kestrel", 10, Filter{
		IncludePrivate: true,
		Spaces:         map[string]bool{"space-alice": true, CommonsSpace: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths(alice) {
		if strings.HasPrefix(p, "bob/") {
			t.Fatalf("alice retrieved %q", p)
		}
	}
	if len(alice) != 3 {
		t.Fatalf("alice got %v, want her two notes and the commons one", paths(alice))
	}

	// An empty allow-list is not the same as no filter: it must return nothing.
	none, err := ix.RetrieveFor("kestrel", 10, Filter{IncludePrivate: true, Spaces: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("an anonymous caller retrieved %v", paths(none))
	}
}

// The reason filtering happens inside ranking: BM25 scores against corpus
// statistics, so a filter applied to the OUTPUT would still let invisible
// notes move the scores of visible ones. Here bob's space holds a flood of
// documents containing the query term; alice's scores must not notice.
func TestInvisibleNotesDoNotInfluenceScores(t *testing.T) {
	ix := aclIndex(t)
	aliceOnly := Filter{IncludePrivate: true,
		Spaces: map[string]bool{"space-alice": true, CommonsSpace: true}}

	before, err := ix.RetrieveFor("kestrel", 5, aliceOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("no hits to compare")
	}

	for i := 0; i < 40; i++ {
		p := fmt.Sprintf("bob/flood%02d.md", i)
		write(t, ix, p, "# Flood\n\nkestrel kestrel kestrel kestrel kestrel")
		if _, err := ix.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}

	after, err := ix.RetrieveFor("kestrel", 5, aliceOnly)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("hit count changed: %v -> %v", paths(before), paths(after))
	}
	for i := range before {
		if before[i].Path != after[i].Path {
			t.Fatalf("order changed after writes in an unreadable space:\n %v\n %v",
				paths(before), paths(after))
		}
		if before[i].Score != after[i].Score {
			t.Fatalf("%s scored %.6f then %.6f — an unreadable space moved a visible score",
				before[i].Path, before[i].Score, after[i].Score)
		}
	}
}

// The corpus-fits decision has to be made against the corpus the caller would
// actually receive, or a member sees "everything fits" and gets a truncated
// view of someone else's vault.
func TestCorpusStatsAndWholeCorpusRespectSpaces(t *testing.T) {
	ix := aclIndex(t)
	f := Filter{IncludePrivate: true, Spaces: map[string]bool{"space-alice": true}}

	chunks, notes, chars, err := ix.CorpusStatsFor(f)
	if err != nil {
		t.Fatal(err)
	}
	if notes != 2 || chunks < 2 || chars == 0 {
		t.Fatalf("stats for alice = %d chunks / %d notes / %d chars", chunks, notes, chars)
	}

	whole, err := ix.WholeCorpusFor(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) == 0 {
		t.Fatal("whole corpus is empty for a space with notes")
	}
	for _, h := range whole {
		if !strings.HasPrefix(h.Path, "alice/") {
			t.Fatalf("whole corpus leaked %q", h.Path)
		}
	}
	empty, err := ix.WholeCorpusFor(Filter{IncludePrivate: true, Spaces: map[string]bool{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Fatalf("an anonymous caller got %d chunks of the whole corpus", len(empty))
	}
}
