package index

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// bound is the scan limit these tests put a small corpus over. The corpora
// hold a few hundred facts, so the window alone could never miss an old fact:
// the tests must be over the bound, not merely near it.
const bound = 100

// oracle is the exact answer to a recall that returns everything it may:
// the facts keep admits, ordered the way the window orders them (newest first,
// id descending on ties), cut to the bound. Recall with the bound binding must
// return exactly this set, because the window is the newest bound facts that
// pass the SQL predicates and accept.
func oracle(entries []memory.Entry, keep func(memory.Entry) bool) []string {
	var kept []memory.Entry
	for _, e := range entries {
		if keep(e) {
			kept = append(kept, e)
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		if kept[i].Stamp != kept[j].Stamp {
			return kept[i].Stamp > kept[j].Stamp
		}
		return kept[i].ID > kept[j].ID
	})
	if len(kept) > bound {
		kept = kept[:bound]
	}
	out := make([]string, len(kept))
	for i, e := range kept {
		out[i] = e.ID
	}
	sort.Strings(out)
	return out
}

func sortedIDs(hits []MemoryHit) []string {
	out := ids(hits)
	sort.Strings(out)
	return out
}

// An old fact that is the only answer to a question, in a store far over the
// bound. The window holds months 10 to 12; the gold fact is from January, so
// only the lexical arm can reach it.
func TestOverBoundFindsOldGoldThroughFTS(t *testing.T) {
	ix := testIndex(t)
	scanCorpus(t, ix, 50, 10)
	memNote(t, ix, "memory/gold.md",
		entry("gold", "2026-01-03 09:00", "quokka zeitgeist ledger is kept offline"))

	hits, err := ix.MemoryEntries(MemoryQuery{
		Query: "where is the quokka zeitgeist ledger", Limit: 5, ScanLimit: bound})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ID != "gold" {
		t.Fatalf("old gold fact not recalled over the bound: %v", ids(hits))
	}
}

// The semantic arm reaches an old fact the window cannot, when the caller
// supplies a vector and no words at all.
func TestOverBoundFindsOldGoldThroughTheVectorArm(t *testing.T) {
	ix := testIndex(t)
	scanCorpus(t, ix, 50, 10)
	const text = "ferret holds the spare keys in the safe"
	memNote(t, ix, "memory/gold.md", entry("gold", "2026-01-04 09:00", text))

	vec := firstVec(ix.Emb.Embed([]string{text}))
	hits, err := ix.MemoryEntries(MemoryQuery{QueryVector: vec, Limit: 5, ScanLimit: bound})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		found = found || h.ID == "gold"
	}
	if !found {
		t.Fatalf("vector arm did not recall the old fact: %v", ids(hits))
	}
}

// As-of at a mid-corpus instant must return what was believed then: the exact
// set, not the newest window of whatever survives the time filter.
func TestOverBoundAsOfMidCorpusMatchesOracle(t *testing.T) {
	ix := testIndex(t)
	entries := scanCorpus(t, ix, 50, 10)
	asOf := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)

	keep := func(e memory.Entry) bool { return e.BelievedAt(asOf) }
	want := oracle(entries, keep)
	if len(want) != bound {
		t.Fatalf("corpus should fill the bound at this instant: %d believed", len(want))
	}
	hits, err := ix.MemoryEntries(MemoryQuery{AsOf: asOf, Limit: 1000, ScanLimit: bound})
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedIDs(hits); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("as_of set differs from the oracle\n got %v\nwant %v", got, want)
	}

	// With text, the same instant must keep only believed facts.
	hits, err = ix.MemoryEntries(MemoryQuery{Query: "Dana prefers dark mode",
		AsOf: asOf, Limit: 1000, ScanLimit: bound})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("as_of with text returned nothing")
	}
	for _, h := range hits {
		if !h.BelievedAt(asOf) {
			t.Errorf("returned %s, which was not believed at %s", h.ID, asOf)
		}
	}
}

// valid_at, and valid_at combined with as_of, are the bitemporal questions. As
// with as_of, the answer must be the exact oracle set at the bound.
func TestOverBoundValidityMatchesOracle(t *testing.T) {
	ix := testIndex(t)
	entries := scanCorpus(t, ix, 50, 10)
	asOf := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	validAt := time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC)
	since := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		q    MemoryQuery
		keep func(memory.Entry) bool
	}{
		// Without as_of, superseded facts and facts expired at now (asOf here)
		// are excluded by the default filters.
		{"valid_at", MemoryQuery{ValidAt: validAt},
			func(e memory.Entry) bool {
				return !e.Superseded() && !e.ExpiredAt(asOf) && e.ValidAt(validAt)
			}},
		{"valid_at as_of", MemoryQuery{ValidAt: validAt, AsOf: asOf},
			func(e memory.Entry) bool { return e.BelievedAt(asOf) && e.ValidAtAsOf(validAt, asOf) }},
		{"valid_during as_of", MemoryQuery{ValidSince: since, ValidUntil: until, AsOf: asOf},
			func(e memory.Entry) bool {
				return e.BelievedAt(asOf) && e.ValidDuringAsOf(since, until, asOf)
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := c.q
			q.Limit = 1000
			q.ScanLimit = bound
			q.Now = asOf
			hits, err := ix.MemoryEntries(q)
			if err != nil {
				t.Fatal(err)
			}
			want := oracle(entries, c.keep)
			if got := sortedIDs(hits); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("set differs from the oracle\n got %d %v\nwant %d %v",
					len(got), got, len(want), want)
			}
		})
	}
}

// Visibility is decided the same way over the bound as under it. A note the
// caller may not read, and a private note, must not appear even when they hold
// the strongest match, and must not displace a visible match.
func TestOverBoundRecallRespectsVisibility(t *testing.T) {
	ix := testIndex(t)
	scanCorpus(t, ix, 50, 10)
	memNote(t, ix, "memory/shown.md", entry("shown", "2026-01-05 09:00",
		"narwhal budget reviewed by alice"))
	memNoteFM(t, ix, "memory/acl.md", func(f *markdown.Frontmatter) { f.Set("readers", "bob") },
		entry("acl", "2026-12-20 09:00", "narwhal narwhal narwhal vault key rotation"))
	memNoteFM(t, ix, "memory/priv.md", func(f *markdown.Frontmatter) { f.Set("private", true) },
		entry("priv", "2026-12-21 09:00", "narwhal narwhal backup password"))

	alice, err := ix.MemoryEntries(MemoryQuery{Query: "narwhal", Limit: 10, ScanLimit: bound,
		Filter: Filter{User: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	got := ids(alice)
	if len(got) == 0 || got[0] != "shown" {
		t.Fatalf("alice should see her own fact first: %v", got)
	}
	for _, id := range got {
		if id == "acl" || id == "priv" {
			t.Errorf("alice saw %s, which she may not read: %v", id, got)
		}
	}

	bob, err := ix.MemoryEntries(MemoryQuery{Query: "narwhal", Limit: 10, ScanLimit: bound,
		Filter: Filter{User: "bob", IncludePrivate: true}})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, id := range ids(bob) {
		seen[id] = true
	}
	if !seen["acl"] {
		t.Errorf("bob, on the reader list, did not see acl: %v", ids(bob))
	}
	if !seen["shown"] {
		// Alice's note has no reader list, so it is in the commons and bob
		// may read it too. Its absence would be the bug.
		t.Errorf("bob lost a commons fact: %v", ids(bob))
	}
}

func TestScanLimitFromEnvironment(t *testing.T) {
	t.Setenv("GRIMOIRE_SCAN_LIMIT", "7")
	if got := scanLimitFromEnv(); got != 7 {
		t.Errorf("GRIMOIRE_SCAN_LIMIT=7 gave %d", got)
	}
	for _, bad := range []string{"", "x", "-3", "0"} {
		t.Setenv("GRIMOIRE_SCAN_LIMIT", bad)
		if got := scanLimitFromEnv(); got != 20000 {
			t.Errorf("GRIMOIRE_SCAN_LIMIT=%q gave %d, want the default", bad, got)
		}
	}
}
