package index

import (
	"fmt"
	"reflect"
	"testing"
)

// The whole design rests on one property: a cache that was patched in place
// must answer exactly as one rebuilt from SQLite. If it does not, results
// depend on the order writes happened to arrive in, and the golden digest
// pinning the published benchmark numbers means nothing.

func hitKeys(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = fmt.Sprintf("%s#%.6f#%s", h.Path, h.Score, h.Chunk)
	}
	return out
}

// retrieveBothWays runs a query against the live (patched) cache and against a
// cache rebuilt from scratch, and returns both answers.
func retrieveBothWays(t *testing.T, ix *Index, q string) ([]string, []string) {
	t.Helper()
	patched, err := ix.Retrieve(q, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	ix.InvalidateCache()
	rebuilt, err := ix.Retrieve(q, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	return hitKeys(patched), hitKeys(rebuilt)
}

func TestPatchedCacheRanksIdenticallyToARebuiltOne(t *testing.T) {
	ix := testIndex(t)
	for i := 0; i < 24; i++ {
		write(t, ix, fmt.Sprintf("n%02d.md", i),
			fmt.Sprintf("# Note %d\n\ndeploy rollback latency postgres vault chunk %d", i, i))
		if _, err := ix.Upsert(fmt.Sprintf("n%02d.md", i)); err != nil {
			t.Fatal(err)
		}
	}
	// warm the cache, then mutate underneath it in every way a vault can
	if _, err := ix.Retrieve("deploy latency", 5, true); err != nil {
		t.Fatal(err)
	}

	write(t, ix, "n05.md", "# Note 5\n\ncompletely rewritten about kestrels and falconry")
	if _, err := ix.Upsert("n05.md"); err != nil {
		t.Fatal(err)
	}
	write(t, ix, "aaa-new.md", "# Aaa\n\na new note that sorts FIRST in corpus order, about deploy latency")
	if _, err := ix.Upsert("aaa-new.md"); err != nil {
		t.Fatal(err)
	}
	write(t, ix, "zzz-new.md", "# Zzz\n\na new note that sorts LAST, also about deploy latency")
	if _, err := ix.Upsert("zzz-new.md"); err != nil {
		t.Fatal(err)
	}
	if err := ix.Remove("n11.md"); err != nil {
		t.Fatal(err)
	}

	for _, q := range []string{"deploy latency", "kestrels", "chunk 7", "postgres vault"} {
		patched, rebuilt := retrieveBothWays(t, ix, q)
		if !reflect.DeepEqual(patched, rebuilt) {
			t.Fatalf("query %q ranked differently after patching\n patched: %v\n rebuilt: %v",
				q, patched, rebuilt)
		}
	}
}

// Corpus statistics feed BM25 (corpus size, average length) and the
// corpus-fits decision (character total). A patch that updates rows but not
// the statistics would rank plausibly and be wrong.
func TestPatchedCacheKeepsCorpusStatisticsExact(t *testing.T) {
	ix := testIndex(t)
	for i := 0; i < 10; i++ {
		write(t, ix, fmt.Sprintf("n%d.md", i), fmt.Sprintf("# N%d\n\nsome text here number %d", i, i))
		if _, err := ix.Upsert(fmt.Sprintf("n%d.md", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.Retrieve("text", 3, true); err != nil {
		t.Fatal(err)
	}
	write(t, ix, "n3.md", "# N3\n\nmuch longer replacement text "+
		"with a great many more words in it than the original had, by design")
	if _, err := ix.Upsert("n3.md"); err != nil {
		t.Fatal(err)
	}
	if err := ix.Remove("n7.md"); err != nil {
		t.Fatal(err)
	}
	// a private note must move the private-excluded totals only
	write(t, ix, "secret.md", "---\nprivate: true\n---\n\n# Secret\n\nhidden text")
	if _, err := ix.Upsert("secret.md"); err != nil {
		t.Fatal(err)
	}

	for _, includePrivate := range []bool{false, true} {
		chunksP, notesP, charsP, err := ix.CorpusStats(includePrivate)
		if err != nil {
			t.Fatal(err)
		}
		ix.InvalidateCache()
		chunksR, notesR, charsR, err := ix.CorpusStats(includePrivate)
		if err != nil {
			t.Fatal(err)
		}
		if chunksP != chunksR || notesP != notesR || charsP != charsR {
			t.Fatalf("include_private=%v: patched stats (%d chunks, %d notes, %d chars) "+
				"!= rebuilt (%d, %d, %d)", includePrivate,
				chunksP, notesP, charsP, chunksR, notesR, charsR)
		}
	}
}

// Tombstones cannot accumulate forever: past a threshold the cache is dropped
// so the next query rebuilds it compactly.
func TestChurnEventuallyCompactsTheCache(t *testing.T) {
	ix := testIndex(t)
	for i := 0; i < 20; i++ {
		write(t, ix, fmt.Sprintf("n%02d.md", i), fmt.Sprintf("# N%d\n\nbody %d", i, i))
		if _, err := ix.Upsert(fmt.Sprintf("n%02d.md", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.Retrieve("body", 3, true); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 20; round++ {
		for i := 0; i < 20; i++ {
			p := fmt.Sprintf("n%02d.md", i)
			write(t, ix, p, fmt.Sprintf("# N%d\n\nbody %d round %d", i, i, round))
			if _, err := ix.Upsert(p); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := ix.Retrieve("body", 3, true); err != nil {
			t.Fatal(err)
		}
	}

	rows, _, _, _ := ix.CacheStats()
	if rows > 20*3 {
		t.Fatalf("cache holds %d rows for a 20-note corpus — tombstones are not being reclaimed", rows)
	}
	// and it still answers correctly after all that churn
	patched, rebuilt := retrieveBothWays(t, ix, "body 7")
	if !reflect.DeepEqual(patched, rebuilt) {
		t.Fatalf("after churn:\n patched: %v\n rebuilt: %v", patched, rebuilt)
	}
}
