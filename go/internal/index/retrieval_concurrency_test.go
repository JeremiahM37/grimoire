package index

import (
	"fmt"
	"sync"
	"testing"
)

// The corpus cache is shared mutable state on a path that serves concurrent
// HTTP and MCP requests, so its failure modes are races and staleness rather
// than wrong arithmetic. These tests are meant to be run under -race.

// TestCacheConcurrentReads asserts that concurrent searches agree with a
// single-threaded search. A torn or double-built cache shows up as a result
// that differs between goroutines.
func TestCacheConcurrentReads(t *testing.T) {
	ix := parityCorpus(t)
	want, err := ix.Retrieve("alpha beta gamma", 8, false)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := ix.Retrieve("alpha beta gamma", 8, false)
			if err != nil {
				errs <- err
				return
			}
			if len(got) != len(want) {
				errs <- fmt.Errorf("len %d != %d", len(got), len(want))
				return
			}
			for j := range got {
				if got[j] != want[j] {
					errs <- fmt.Errorf("hit %d: %+v != %+v", j, got[j], want[j])
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestCacheInvalidatedByWrite is the correctness property the whole cache
// rests on: a note indexed after the cache was built must be visible to the
// very next search. If revision tracking ever stops covering a write path,
// searches silently serve a stale corpus — the worst failure this design can
// have, because nothing errors.
func TestCacheInvalidatedByWrite(t *testing.T) {
	ix := parityCorpus(t)
	if _, err := ix.Retrieve("alpha", 8, true); err != nil { // warm
		t.Fatal(err)
	}

	const body = "quokka quokka quokka distinctive marsupial"
	if _, err := ix.Vault.Write("newnote.md", body, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert("newnote.md"); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Retrieve("quokka", 8, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Path == "newnote.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("note indexed after the cache was built is not searchable: %+v", hits)
	}

	// ...and removing it must take it back out.
	if err := ix.Remove("newnote.md"); err != nil {
		t.Fatal(err)
	}
	hits, err = ix.Retrieve("quokka", 8, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Path == "newnote.md" {
			t.Fatalf("removed note still searchable: %+v", hits)
		}
	}
}

// TestCacheConcurrentReadWrite runs searches against a corpus being rewritten
// underneath them. It asserts no race and no error, not a particular result:
// which revision a given search observes is legitimately a scheduling matter.
func TestCacheConcurrentReadWrite(t *testing.T) {
	ix := parityCorpus(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	errs := make(chan error, 32)

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := ix.Retrieve("alpha beta gamma delta", 8, false); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("churn%02d.md", i)
		if _, err := ix.Vault.Write(name, fmt.Sprintf("alpha beta churn %d", i), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := ix.Upsert(name); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

// TestPrivateNeverLeaks is the invariant the product is sold on. It is
// asserted here at the retrieval boundary rather than trusted to the SQL that
// used to enforce it, because the filter moved into Go when the cache started
// holding every row regardless of visibility.
func TestPrivateNeverLeaks(t *testing.T) {
	ix := parityCorpus(t)
	privatePaths := map[string]bool{}
	rows, err := ix.DB.Query("SELECT DISTINCT note FROM vectors WHERE private=1")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		privatePaths[p] = true
	}
	rows.Close()
	if len(privatePaths) == 0 {
		t.Fatal("fixture has no private notes; this test would prove nothing")
	}

	for _, q := range parityQueries {
		for _, k := range []int{1, 3, 8, 20, 0} {
			hits, err := ix.Retrieve(q, k, false)
			if err != nil {
				t.Fatal(err)
			}
			for _, h := range hits {
				if privatePaths[h.Path] {
					t.Fatalf("private note %q leaked into a public search for %q", h.Path, q)
				}
			}
		}
	}
}

// TestCacheInvalidatedByEmptyingReindex covers the rebuild that writes nothing.
// Every other write path bumps the revision as a side effect of inserting
// rows, so a rebuild of an emptied vault is the one case where invalidation
// has to be deliberate.
func TestCacheInvalidatedByEmptyingReindex(t *testing.T) {
	ix := parityCorpus(t)
	if _, err := ix.Vault.Write("findme.md", "sphinx of black quartz", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert("findme.md"); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Retrieve("sphinx", 8, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("fixture note not searchable; test would prove nothing")
	}

	// The vault on disk holds only that note, so reindexing after deleting it
	// rebuilds to an empty corpus.
	if err := ix.Vault.Delete("findme.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	hits, err = ix.Retrieve("sphinx", 8, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("search answered from a stale cache after the index was emptied: %+v", hits)
	}
}

// TestOrphanVectorsAreNotRetrievable pins the exclusion the corpus scan has
// always performed. A vector row whose note row is gone is a bug in whatever
// wrote the index, but it must not become a search result: it would surface a
// deleted note's text, untitled, with no way for a caller to notice.
func TestOrphanVectorsAreNotRetrievable(t *testing.T) {
	ix := parityCorpus(t)

	// A chunk with no matching note row — the shape a partial rebuild leaves.
	vec := ix.Emb.Embed([]string{"orphaned secret text"})[0]
	if err := ix.DB.Exec(
		"INSERT INTO vectors(note,chunk_idx,chunk,embedding,private) VALUES(?,?,?,?,0)",
		"deleted/gone.md", 0, "orphaned secret text", Pack(vec)); err != nil {
		t.Fatal(err)
	}
	ix.InvalidateCache()

	hits, err := ix.Retrieve("orphaned secret text", 20, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Path == "deleted/gone.md" {
			t.Fatalf("orphan vector was returned: %+v", h)
		}
	}
}
