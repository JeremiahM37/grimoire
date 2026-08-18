package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// legacySchema is the index shape as shipped before fts_chunks was removed.
// Migrations have to be proven against the schema that exists in the field,
// not only against a freshly created one — an existing vault's index is the
// only version of this file that matters.
const legacySchema = `
CREATE TABLE IF NOT EXISTS notes(
  path TEXT PRIMARY KEY, title TEXT, body TEXT, frontmatter_json TEXT DEFAULT '{}',
  private INTEGER DEFAULT 0, mtime REAL, hash TEXT, created TEXT, updated TEXT);
CREATE TABLE IF NOT EXISTS vectors(
  note TEXT NOT NULL, chunk_idx INTEGER, chunk TEXT, embedding BLOB,
  private INTEGER DEFAULT 0);
CREATE VIRTUAL TABLE IF NOT EXISTS fts_chunks USING fts5(
  note UNINDEXED, chunk_idx UNINDEXED, chunk, private UNINDEXED,
  tokenize='porter unicode61');
`

func tableNames(t *testing.T, conn *sql.DB) map[string]bool {
	t.Helper()
	rows, err := conn.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMigrationDropsFTSChunksFromAnExistingIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")

	// Build an index in the old shape, with rows in it.
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(
		`INSERT INTO notes(path,title,body) VALUES('a.md','A','hello');
		 INSERT INTO vectors(note,chunk_idx,chunk,embedding) VALUES('a.md',0,'hello',x'00');
		 INSERT INTO fts_chunks(note,chunk_idx,chunk,private) VALUES('a.md',0,'hello',0);`); err != nil {
		t.Fatal(err)
	}
	before := tableNames(t, conn)
	if !before["fts_chunks"] {
		t.Fatal("fixture did not create fts_chunks; the test would prove nothing")
	}
	conn.Close()

	// Opening with the current code must migrate it, and must not disturb
	// anything else in the file.
	d, err := Open(path)
	if err != nil {
		t.Fatalf("opening a pre-migration index failed: %v", err)
	}
	defer d.Close()

	after := tableNames(t, d.Conn())
	if after["fts_chunks"] {
		t.Error("fts_chunks survived the migration")
	}
	for name := range after {
		if len(name) > 11 && name[:11] == "fts_chunks_" {
			t.Errorf("fts5 shadow table %s was left behind", name)
		}
	}
	n, err := d.Count("SELECT count(*) FROM notes")
	if err != nil || n != 1 {
		t.Errorf("notes = %d (err %v), want 1 — migration must not touch data", n, err)
	}
	v, err := d.Count("SELECT count(*) FROM vectors")
	if err != nil || v != 1 {
		t.Errorf("vectors = %d (err %v), want 1", v, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

// Opening twice must be a no-op the second time: migrations run on every
// start, so one that is not idempotent breaks every restart after the first.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	for i := 0; i < 3; i++ {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := d.Exec("INSERT INTO notes(path,title) VALUES(?,?)",
			filepath.Join("n", string(rune('a'+i))+".md"), "T"); err != nil {
			t.Fatalf("write after open %d: %v", i, err)
		}
		d.Close()
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	n, err := d.Count("SELECT count(*) FROM notes")
	if err != nil || n != 3 {
		t.Errorf("notes = %d (err %v), want 3 across reopens", n, err)
	}
}
