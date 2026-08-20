package index

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
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

// Per-note reader lists: the mechanism a pulled document uses when its source
// already knows who may read it.

func aclNote(t *testing.T, ix *Index, path, body string, readers ...string) {
	t.Helper()
	fm := markdown.NewFrontmatter()
	if len(readers) > 0 {
		fm.Set("readers", strings.Join(readers, ", "))
	}
	if _, err := ix.Vault.Write(path, body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert(path); err != nil {
		t.Fatal(err)
	}
}

func TestReaderListsNarrowAccessWithinASpace(t *testing.T) {
	ix := testIndex(t)
	aclNote(t, ix, "open.md", "# Open\n\nkestrel for everyone")
	aclNote(t, ix, "restricted.md", "# Restricted\n\nkestrel for alice only", "user-alice")

	alice := Filter{IncludePrivate: true, User: "user-alice"}
	bob := Filter{IncludePrivate: true, User: "user-bob"}
	nobody := Filter{IncludePrivate: true}

	for name, c := range map[string]struct {
		filter Filter
		want   []string
	}{
		"named reader":   {alice, []string{"open.md", "restricted.md"}},
		"another member": {bob, []string{"open.md"}},
		"no account":     {nobody, []string{"open.md"}},
	} {
		hits, err := ix.RetrieveFor("kestrel", 10, c.filter)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, h := range hits {
			got[h.Path] = true
		}
		if len(got) != len(c.want) {
			t.Errorf("%s got %v, want %v", name, paths(hits), c.want)
			continue
		}
		for _, w := range c.want {
			if !got[w] {
				t.Errorf("%s is missing %s (got %v)", name, w, paths(hits))
			}
		}
	}
}

// A reader list can only narrow. A connector writing one must not be able to
// widen access beyond the space the document was pulled into.
func TestAReaderListCannotWidenAccess(t *testing.T) {
	ix := testIndex(t)
	ix.Spaces = fixedSpaces{"private-space/": "space-private"}
	aclNote(t, ix, "private-space/doc.md", "# Doc\n\nkestrel", "user-bob")

	// Bob is on the reader list but cannot read the space.
	hits, err := ix.RetrieveFor("kestrel", 10, Filter{
		IncludePrivate: true, User: "user-bob",
		Spaces: map[string]bool{CommonsSpace: true}, // not space-private
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("a reader list granted access to a space the caller cannot read: %v", paths(hits))
	}
}

// The same reasoning as spaces: statistics must be computed over what the
// caller can see, or the contents of restricted documents move visible scores.
func TestRestrictedDocumentsDoNotMoveVisibleScores(t *testing.T) {
	ix := testIndex(t)
	aclNote(t, ix, "visible.md", "# Visible\n\nkestrel migration notes")
	bob := Filter{IncludePrivate: true, User: "user-bob"}

	before, err := ix.RetrieveFor("kestrel", 5, bob)
	if err != nil || len(before) == 0 {
		t.Fatalf("no baseline: %v", err)
	}
	for i := 0; i < 30; i++ {
		aclNote(t, ix, fmt.Sprintf("secret%02d.md", i),
			"# Secret\n\nkestrel kestrel kestrel", "user-alice")
	}
	after, err := ix.RetrieveFor("kestrel", 5, bob)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("hit count changed: %v -> %v", paths(before), paths(after))
	}
	for i := range before {
		if before[i].Path != after[i].Path || before[i].Score != after[i].Score {
			t.Fatalf("%s scored %.6f then %.6f — documents the caller cannot read moved it",
				before[i].Path, before[i].Score, after[i].Score)
		}
	}
}

func TestACLEncodingCannotMatchAPartialID(t *testing.T) {
	acl := EncodeACL([]string{"abcdef", "ghijkl"})
	for _, id := range []string{"abcdef", "ghijkl"} {
		if !aclAllows(acl, id) {
			t.Errorf("%s was refused by its own list", id)
		}
	}
	for _, id := range []string{"abc", "def", "hij", "", "abcdefg"} {
		if aclAllows(acl, id) {
			t.Errorf("%q matched a list it is not on", id)
		}
	}
	if !aclAllows("", "anyone") {
		t.Error("an empty list must defer to the space, not deny")
	}
}

// The whole-corpus path is where a reader list is easiest to forget: when the
// corpus fits a budget nothing is ranked, so a filter that lives only in
// ranking checks nothing. That is the case a small vault always takes.
func TestTheWholeCorpusPathAppliesReaderLists(t *testing.T) {
	ix := testIndex(t)
	aclNote(t, ix, "open.md", "# Open\n\nkestrel for everyone")
	aclNote(t, ix, "restricted.md", "# Restricted\n\nkestrel for alice", "user-alice")

	for name, c := range map[string]struct {
		filter Filter
		want   int
	}{
		"named reader":  {Filter{IncludePrivate: true, User: "user-alice"}, 2},
		"other member":  {Filter{IncludePrivate: true, User: "user-bob"}, 1},
		"no account":    {Filter{IncludePrivate: true}, 1},
		"administrator": {Filter{IncludePrivate: true, IgnoreACLs: true}, 2},
	} {
		hits, err := ix.WholeCorpusFor(c.filter)
		if err != nil {
			t.Fatal(err)
		}
		notes := map[string]bool{}
		for _, h := range hits {
			notes[h.Path] = true
		}
		if len(notes) != c.want {
			t.Errorf("%s saw %v, want %d notes", name, paths(hits), c.want)
		}
		if c.want == 1 && notes["restricted.md"] {
			t.Errorf("%s saw the restricted note", name)
		}
	}
}
