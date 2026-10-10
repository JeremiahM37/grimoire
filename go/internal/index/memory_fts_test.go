package index

import (
	"path/filepath"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// openIndexAt opens an index over root, so a test can close it and reopen the
// same database to exercise the migration path.
func openIndexAt(t *testing.T, root string) (*Index, *db.DB) {
	t.Helper()
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	return New(database, v, embed.Hash{}), database
}

// ftsIntegrity asks FTS5 to compare its index with memory_entries row by row.
// It fails when the triggers have missed a write, which is exactly the drift
// these tests exist to catch.
func ftsIntegrity(t *testing.T, ix *Index) {
	t.Helper()
	if err := ix.DB.Exec("INSERT INTO memory_fts(memory_fts, rank) VALUES('integrity-check', 1)"); err != nil {
		t.Fatalf("memory_fts out of step with memory_entries: %v", err)
	}
}

func ftsHas(t *testing.T, ix *Index, term string) int {
	t.Helper()
	n, err := ix.DB.Count("SELECT COUNT(*) FROM memory_fts WHERE memory_fts MATCH ?", term)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMemoryFTSFollowsEveryWritePath(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/a.md",
		entry("e1", "2026-08-10 09:00", "quokka tea at the office"),
		entry("e2", "2026-08-11 09:00", "walrus coffee at home"))
	ftsIntegrity(t, ix)
	if n := ftsHas(t, ix, "quokka"); n != 1 {
		t.Fatalf("new fact not indexed: %d matches", n)
	}

	// A rewrite of the note deletes and reinserts its rows (writeMemoryRows).
	memNote(t, ix, "memory/a.md",
		entry("e2", "2026-08-11 09:00", "walrus coffee at home"),
		entry("e3", "2026-08-12 09:00", "ocelot runs the nightly job"))
	ftsIntegrity(t, ix)
	if n := ftsHas(t, ix, "quokka"); n != 0 {
		t.Errorf("a removed fact still matches: %d", n)
	}
	if n := ftsHas(t, ix, "ocelot"); n != 1 {
		t.Errorf("an added fact does not match: %d", n)
	}

	// Supersession writes SupersededBy into the old bullet, so the row is
	// rewritten and stays. Recall excludes it by the existing predicate; as_of
	// still needs it, so the FTS row is kept rather than removed.
	memNote(t, ix, "memory/a.md",
		memory.Entry{ID: "e2", Stamp: "2026-08-11 09:00", Agent: "claude",
			Text: "walrus coffee at home", SupersededBy: "e3", SupersededAt: "2026-08-12 09:00"},
		entry("e3", "2026-08-12 09:00", "ocelot runs the nightly job"))
	ftsIntegrity(t, ix)
	hits, err := ix.MemoryEntries(MemoryQuery{Query: "walrus", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.ID == "e2" {
			t.Errorf("superseded fact returned by default recall: %v", ids(hits))
		}
	}

	// Forgetting a note removes its rows and their FTS entries.
	if err := ix.Remove("memory/a.md"); err != nil {
		t.Fatal(err)
	}
	ftsIntegrity(t, ix)
	if n := ftsHas(t, ix, "ocelot"); n != 0 {
		t.Errorf("a removed note still matches: %d", n)
	}

	// A full reindex rebuilds memory from the markdown and must leave FTS in step.
	memNote(t, ix, "memory/b.md", entry("e4", "2026-08-13 09:00", "narwhal reviews the budget"))
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	ftsIntegrity(t, ix)
	if n := ftsHas(t, ix, "narwhal"); n != 1 {
		t.Errorf("reindex lost a fact from FTS: %d", n)
	}
}

// An index built before memory_fts existed must be backfilled on open, and
// the backfill must make its facts findable without any write.
func TestMemoryFTSBackfillsAnExistingIndex(t *testing.T) {
	root := t.TempDir()
	ix, database := openIndexAt(t, root)
	memNote(t, ix, "memory/old.md", entry("e1", "2026-01-02 09:00", "quokka keeps the archive"))

	// Simulate an index written by a build without the table or triggers.
	for _, stmt := range []string{
		"DROP TRIGGER memory_entries_fts_ai",
		"DROP TRIGGER memory_entries_fts_ad",
		"DROP TRIGGER memory_entries_fts_au",
		"DROP TABLE memory_fts",
	} {
		if err := database.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, database2 := openIndexAt(t, root)
	defer database2.Close()
	ftsIntegrity(t, reopened)
	if n := ftsHas(t, reopened, "quokka"); n != 1 {
		t.Fatalf("backfill did not index the existing fact: %d matches", n)
	}

	// Reopening an index that already has the table must not rebuild it or
	// disturb it: a second open is a no-op.
	if err := database2.Close(); err != nil {
		t.Fatal(err)
	}
	again, database3 := openIndexAt(t, root)
	defer database3.Close()
	ftsIntegrity(t, again)
	if n := ftsHas(t, again, "quokka"); n != 1 {
		t.Fatalf("second open changed FTS: %d matches", n)
	}
}
