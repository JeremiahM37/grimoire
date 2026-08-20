package index

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// countingEmbedder reports how many texts it was asked to embed, which is the
// cost a sync exists to avoid paying twice.
type countingEmbedder struct {
	calls int
	sig   string
}

func (c *countingEmbedder) Embed(texts []string) [][]float32 {
	c.calls += len(texts)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, 8)
		for j, r := range t {
			v[j%8] += float32(r%13) / 10
		}
		out[i] = v
	}
	return out
}

func (c *countingEmbedder) Dim() int { return 8 }

func (c *countingEmbedder) Signature() string {
	if c.sig == "" {
		return "counting:v1"
	}
	return c.sig
}

func syncIndex(t *testing.T) (*Index, *countingEmbedder) {
	t.Helper()
	ix := testIndex(t)
	emb := &countingEmbedder{}
	ix.Emb = emb
	return ix, emb
}

func TestSyncSkipsUnchangedNotes(t *testing.T) {
	ix, emb := syncIndex(t)
	for _, name := range []string{"a.md", "b.md", "c.md"} {
		write(t, ix, name, "# "+name+"\n\nsome body text about deploys and latency")
	}

	first, err := ix.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if !first.FullRebuild || first.Added != 3 {
		t.Fatalf("first sync = %+v, want a full rebuild of 3 notes", first)
	}
	embedded := emb.calls
	if embedded == 0 {
		t.Fatal("nothing was embedded on the first sync")
	}

	// Second sync: nothing changed on disk, so nothing may be read or embedded.
	second, err := ix.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if second.FullRebuild {
		t.Fatal("an unchanged vault triggered a full rebuild")
	}
	if second.Unchanged != 3 || second.Added+second.Updated+second.Removed != 0 {
		t.Fatalf("second sync = %+v, want 3 unchanged and nothing else", second)
	}
	if emb.calls != embedded {
		t.Fatalf("re-embedded %d chunks for an unchanged vault", emb.calls-embedded)
	}
}

func TestSyncPicksUpAddsEditsAndDeletes(t *testing.T) {
	ix, _ := syncIndex(t)
	write(t, ix, "keep.md", "# Keep\n\nunchanged text")
	write(t, ix, "edit.md", "# Edit\n\nbefore")
	write(t, ix, "gone.md", "# Gone\n\ndoomed")
	if _, err := ix.Sync(); err != nil {
		t.Fatal(err)
	}

	// mtime resolution on some filesystems is coarse; make the edit unambiguous
	time.Sleep(10 * time.Millisecond)
	write(t, ix, "edit.md", "# Edit\n\nafter, with entirely different words")
	write(t, ix, "new.md", "# New\n\na note that did not exist")
	if err := os.Remove(filepath.Join(ix.Vault.Root, "gone.md")); err != nil {
		t.Fatal(err)
	}

	got, err := ix.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if got.Added != 1 || got.Updated != 1 || got.Removed != 1 || got.Unchanged != 1 {
		t.Fatalf("sync = %+v, want 1 added / 1 updated / 1 removed / 1 unchanged", got)
	}

	n, err := ix.DB.Count("SELECT count(*) FROM notes WHERE path=?", "gone.md")
	if err != nil || n != 0 {
		t.Fatalf("deleted note still indexed (%d, %v)", n, err)
	}
	var body string
	if err := ix.DB.QueryRow("SELECT body FROM notes WHERE path=?", "edit.md").Scan(&body); err != nil {
		t.Fatal(err)
	}
	if want := "after"; body == "" || !contains(body, want) {
		t.Fatalf("edited note body = %q, want it to contain %q", body, want)
	}
	// the deleted note's chunks must go too, or search answers from a file
	// that no longer exists
	orphans, err := ix.DB.Count("SELECT count(*) FROM vectors WHERE note=?", "gone.md")
	if err != nil || orphans != 0 {
		t.Fatalf("%d orphan chunks for a deleted note (%v)", orphans, err)
	}
}

// A file touched but not edited is the common case for sync clients. It must
// cost a read and nothing more — no re-embedding — and must not keep costing
// that read on every subsequent start.
func TestSyncDoesNotReembedATouchedFile(t *testing.T) {
	ix, emb := syncIndex(t)
	write(t, ix, "note.md", "# Note\n\nbody that will not change")
	if _, err := ix.Sync(); err != nil {
		t.Fatal(err)
	}
	before := emb.calls

	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(ix.Vault.Root, "note.md"), later, later); err != nil {
		t.Fatal(err)
	}
	got, err := ix.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if got.Unchanged != 1 || got.Updated != 0 {
		t.Fatalf("touched file: sync = %+v, want it treated as unchanged", got)
	}
	if emb.calls != before {
		t.Fatalf("re-embedded a file whose content is identical (%d new calls)", emb.calls-before)
	}
	// and the new mtime must have been recorded, or every future start pays
	// the read again
	third, err := ix.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if third.Unchanged != 1 {
		t.Fatalf("third sync = %+v", third)
	}
}

// Embeddings from different models are not comparable, so changing the model
// must abandon the incremental path entirely rather than leave a corpus half
// embedded by each.
func TestSyncFullyRebuildsWhenTheEmbedderChanges(t *testing.T) {
	ix, emb := syncIndex(t)
	write(t, ix, "a.md", "# A\n\ntext")
	if _, err := ix.Sync(); err != nil {
		t.Fatal(err)
	}

	emb.sig = "counting:v2"
	got, err := ix.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if !got.FullRebuild {
		t.Fatalf("sync = %+v, want a full rebuild after the embedder changed", got)
	}
	// and the signature is recorded, so the NEXT start is incremental again
	again, err := ix.Sync()
	if err != nil {
		t.Fatal(err)
	}
	if again.FullRebuild {
		t.Fatal("the new signature was not recorded — every start would rebuild")
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
