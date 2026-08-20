package index

import (
	"fmt"
	"testing"
	"time"
)

// Relabelling spaces must be one transaction, not one per row: a per-row
// transaction is an fsync per row, and this runs over the whole vault whenever
// a space is added.
func TestRestampSpacesIsBatched(t *testing.T) {
	ix := testIndex(t)
	for i := 0; i < 400; i++ {
		p := fmt.Sprintf("team/n%03d.md", i)
		write(t, ix, p, "# N\n\nbody")
		if _, err := ix.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := ix.RestampSpaces(func(string) string { return "space-team" }); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	n, err := ix.DB.Count("SELECT count(*) FROM notes WHERE space=?", "space-team")
	if err != nil || n != 400 {
		t.Fatalf("relabelled %d notes (%v)", n, err)
	}
	v, err := ix.DB.Count("SELECT count(*) FROM vectors WHERE space=?", "space-team")
	if err != nil || v == 0 {
		t.Fatalf("chunks were not relabelled (%d, %v)", v, err)
	}
	// Generous, because CI disks vary — but a per-row transaction over 800
	// statements is seconds, not milliseconds.
	if elapsed > 3*time.Second {
		t.Fatalf("relabelling 400 notes took %v — is it still one transaction per row?", elapsed)
	}
}
