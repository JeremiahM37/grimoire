// Package db is the SQLite index — a rebuildable cache over the vault, with
// FTS5 for local search.
//
// Port of server/db.py. Uses modernc.org/sqlite (a pure-Go SQLite, FTS5
// included) rather than a cgo binding: cgo would forfeit both headline reasons
// for this port — a single static binary and effortless cross-compilation.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite" // pure-Go driver
)

// Schema is the index shape. Every table here is derived from the vault and can
// be dropped and rebuilt; files are the source of truth.
const Schema = `
CREATE TABLE IF NOT EXISTS notes(
  path TEXT PRIMARY KEY, title TEXT, body TEXT, frontmatter_json TEXT DEFAULT '{}',
  private INTEGER DEFAULT 0, mtime REAL, hash TEXT,
  created TEXT, updated TEXT
);
CREATE TABLE IF NOT EXISTS links(
  src TEXT NOT NULL, target TEXT NOT NULL, dst TEXT, alias TEXT DEFAULT '',
  resolved INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_links_src ON links(src);
CREATE INDEX IF NOT EXISTS idx_links_dst ON links(dst);
CREATE INDEX IF NOT EXISTS idx_links_target ON links(target);
CREATE TABLE IF NOT EXISTS tags(note TEXT NOT NULL, tag TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_tags_tag ON tags(tag);
CREATE INDEX IF NOT EXISTS idx_tags_note ON tags(note);
CREATE TABLE IF NOT EXISTS facts(
  note TEXT NOT NULL, key TEXT NOT NULL, value TEXT, private INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_facts_key ON facts(key);
CREATE INDEX IF NOT EXISTS idx_facts_note ON facts(note);
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(
  path UNINDEXED, title, body, tokenize='porter unicode61'
);
CREATE TABLE IF NOT EXISTS vectors(
  note TEXT NOT NULL, chunk_idx INTEGER, chunk TEXT, embedding BLOB,
  private INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_vectors_note ON vectors(note);
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS grants(
  token TEXT PRIMARY KEY, secret TEXT, grantee TEXT, scope TEXT,
  expires_at REAL, created TEXT
);
CREATE TABLE IF NOT EXISTS audit(
  id INTEGER PRIMARY KEY, ts TEXT, action TEXT, secret TEXT, detail TEXT DEFAULT ''
);
`

// migrations bring an index created by an older build up to the current shape.
// The index is a rebuildable cache, so these only ever need to remove things —
// anything missing is recreated by Schema, and anything unreadable is fixed by
// a reindex.
const migrations = `
-- fts_chunks was a per-chunk FTS5 table that nothing ever queried: the lexical
-- leg of retrieval is a hand-rolled BM25 over the vector store, kept that way
-- so its scores match the Python implementation the published benchmarks were
-- measured on. The table was written on every note write and every rebuild.
--
-- It was not merely idle. Mirroring every chunk into an FTS5 index made chunk
-- writes 5x slower end to end (20k chunks: 1.43s with, 284ms without), and
-- because FTS5 defers index merging, part of that cost was paid later by
-- whichever statement ran next rather than by the write itself.
DROP TABLE IF EXISTS fts_chunks;
`

// DB wraps the index connection.
//
// The mutex serializes writes. SQLite would serialize them anyway, but callers
// need a lock that spans a whole logical write: a full rebuild deletes every
// row then re-inserts them one note at a time, and a single-note upsert landing
// mid-sweep would collide on notes.path and abort the rebuild half-done.
type DB struct {
	conn *sql.DB
	mu   sync.RWMutex
}

// Open creates or opens the index at path, applying the schema.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One writer: the driver is fine with more, but WAL plus a single
	// connection keeps write ordering obvious and avoids SQLITE_BUSY retries.
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enabling WAL: %w", err)
	}
	if _, err := conn.Exec(Schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	if _, err := conn.Exec(migrations); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrating schema: %w", err)
	}
	return &DB{conn: conn}, nil
}

// Close releases the connection.
func (d *DB) Close() error { return d.conn.Close() }

// Conn exposes the underlying handle for queries that need it.
func (d *DB) Conn() *sql.DB { return d.conn }

// Lock takes the write lock for a whole logical write.
func (d *DB) Lock()   { d.mu.Lock() }
func (d *DB) Unlock() { d.mu.Unlock() }

// Exec runs a statement under the write lock.
func (d *DB) Exec(query string, args ...any) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.conn.Exec(query, args...)
	return err
}

// ExecLocked runs a statement assuming the caller already holds the lock.
func (d *DB) ExecLocked(query string, args ...any) error {
	_, err := d.conn.Exec(query, args...)
	return err
}

// Query runs a read.
func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.conn.Query(query, args...)
}

// QueryRow runs a single-row read.
func (d *DB) QueryRow(query string, args ...any) *sql.Row {
	return d.conn.QueryRow(query, args...)
}

// Count returns a single integer result, 0 when there is no row.
func (d *DB) Count(query string, args ...any) (int, error) {
	var n int
	err := d.conn.QueryRow(query, args...).Scan(&n)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return n, err
}
