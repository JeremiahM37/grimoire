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
  created TEXT, updated TEXT,
  space TEXT NOT NULL DEFAULT 'commons'
);
-- Requests for a credential grant that nobody has issued yet. An agent that
-- needs a secret it has no grant for can ask, and a person answers. Without
-- this the only workable habit is to pre-grant broadly, which is the failure
-- mode scoped, time-boxed grants exist to prevent. See internal/secrets.
CREATE TABLE IF NOT EXISTS grant_requests(
  id TEXT PRIMARY KEY, secret TEXT NOT NULL, grantee TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
  ttl INTEGER NOT NULL DEFAULT 900, state TEXT NOT NULL DEFAULT 'pending',
  created TEXT, decided TEXT, decided_by TEXT, token TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  max_uses INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_grant_requests_state ON grant_requests(state);
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
  private INTEGER DEFAULT 0,
  -- Which space this chunk belongs to, denormalized onto the row so ranking
  -- can filter on it without a join. Filtering AFTER ranking would leak: the
  -- corpus statistics BM25 scores against would still be computed over notes
  -- the caller cannot see.
  space TEXT NOT NULL DEFAULT 'commons'
);
CREATE INDEX IF NOT EXISTS idx_vectors_note ON vectors(note);
-- fts.path is UNINDEXED, so "DELETE FROM fts WHERE path=?" scans every row of
-- the FTS index. That delete runs once per note on every write and once per
-- note during a rebuild, which made both quadratic: inserting notes one at a
-- time took 400ms at 2k, 5.7s at 8k and 22.4s at 16k, against 52ms/213ms/442ms
-- with the delete removed. This map holds each path's fts rowid so the delete
-- can be a rowid lookup instead.
--
-- rid is the map's OWN rowid, handed to fts as an explicit one. It cannot be
-- read back from the fts insert: RETURNING rowid on an FTS5 virtual table
-- yields -1, so a map built that way matches nothing, every delete misses, and
-- both copies of a note stay searchable — which showed up as an encrypted
-- note's plaintext still answering a search.
CREATE TABLE IF NOT EXISTS fts_map(rid INTEGER PRIMARY KEY, path TEXT NOT NULL UNIQUE);
-- Agent memory, one row per remembered fact rather than per note. A memory
-- note accumulates dozens of unrelated facts, so ranking it as one document
-- blurs every one of them; reconciling a new fact against an old one needs to
-- address the old one; and scoping memory to an agent or a session is a
-- property of the fact, not of the file. Derived from the markdown bullets
-- like every other table here — drop it and a reindex rebuilds it.
-- Headings, list items and tasks, one row each. A note is the right unit for
-- most questions and the wrong one for the questions people actually ask of a
-- vault — every open task, every heading called Decisions — which are about a
-- LINE. Derived from the markdown like every other table here.
CREATE TABLE IF NOT EXISTS blocks(
  note TEXT NOT NULL, kind TEXT NOT NULL, text TEXT NOT NULL,
  level INTEGER NOT NULL DEFAULT 0, line INTEGER NOT NULL DEFAULT 0,
  checked INTEGER NOT NULL DEFAULT 0, parent TEXT NOT NULL DEFAULT '',
  private INTEGER NOT NULL DEFAULT 0,
  space TEXT NOT NULL DEFAULT 'commons', acl TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_blocks_note ON blocks(note);
CREATE INDEX IF NOT EXISTS idx_blocks_kind ON blocks(kind);
CREATE TABLE IF NOT EXISTS memory_entries(
  id TEXT NOT NULL, note TEXT NOT NULL, text TEXT NOT NULL,
  agent TEXT NOT NULL DEFAULT '', task TEXT NOT NULL DEFAULT '',
  session TEXT NOT NULL DEFAULT '', stamp TEXT NOT NULL DEFAULT '',
  category TEXT NOT NULL DEFAULT '', expires TEXT NOT NULL DEFAULT '',
  immutable INTEGER NOT NULL DEFAULT 0, superseded_by TEXT NOT NULL DEFAULT '',
  superseded_at TEXT NOT NULL DEFAULT '',
  helpful INTEGER NOT NULL DEFAULT 0, unhelpful INTEGER NOT NULL DEFAULT 0,
  line INTEGER NOT NULL DEFAULT 0, embedding BLOB,
  space TEXT NOT NULL DEFAULT 'commons', acl TEXT NOT NULL DEFAULT '',
  private INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(note, id)
);
CREATE INDEX IF NOT EXISTS idx_memory_entries_id ON memory_entries(id);
CREATE INDEX IF NOT EXISTS idx_memory_entries_session ON memory_entries(session);
CREATE INDEX IF NOT EXISTS idx_memory_entries_agent ON memory_entries(agent);
CREATE INDEX IF NOT EXISTS idx_memory_entries_category ON memory_entries(category);
-- Recall bounds its scan with ORDER BY stamp DESC LIMIT. Without this index
-- SQLite sorts the whole table to answer it, which cost more than the bound
-- saved: measured at 13.7ms -> 23.6ms over a thousand entries, a 70% regression
-- on the case where the bound does not even bind.
CREATE INDEX IF NOT EXISTS idx_memory_entries_stamp ON memory_entries(stamp DESC, id DESC);
-- Every model call grimoire made itself: ask, rerank, intent, summarize, embed.
-- NOT an agent's own token spend, which never passes through this process.
CREATE TABLE IF NOT EXISTS model_calls(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  at TEXT NOT NULL, provider TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
  surface TEXT NOT NULL DEFAULT '', agent TEXT NOT NULL DEFAULT '',
  input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  cost REAL NOT NULL DEFAULT 0, cost_known INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT ''
);
-- The dashboard always asks "since when", so time leads the index.
CREATE INDEX IF NOT EXISTS idx_model_calls_at ON model_calls(at DESC);
CREATE INDEX IF NOT EXISTS idx_model_calls_provider ON model_calls(provider);
CREATE TABLE IF NOT EXISTS memory_entities(
  note TEXT NOT NULL, id TEXT NOT NULL, entity TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_entities_entity ON memory_entities(entity);
CREATE INDEX IF NOT EXISTS idx_memory_entities_note ON memory_entities(note);
-- Identity and authorization. A deployment with no rows in users behaves
-- exactly as the single-user server always did; see internal/auth.
CREATE TABLE IF NOT EXISTS users(
  id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, display TEXT,
  pwhash TEXT NOT NULL, role TEXT NOT NULL, created TEXT
);
-- Session and API-key tokens are stored hashed: a stolen copy of the index
-- must not hand over live credentials for every account.
CREATE TABLE IF NOT EXISTS sessions(
  token TEXT PRIMARY KEY, user TEXT NOT NULL, expires REAL, created TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user);
CREATE TABLE IF NOT EXISTS api_keys(
  id TEXT PRIMARY KEY, hash TEXT NOT NULL UNIQUE, user TEXT NOT NULL,
  label TEXT, created TEXT, last_used TEXT
);
CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user);
-- A space is the unit of access: a subtree of the vault plus the people who
-- may see it. Paths stay the source of truth, so who can read what is visible
-- in the file tree rather than only in a database.
CREATE TABLE IF NOT EXISTS spaces(
  id TEXT PRIMARY KEY, name TEXT NOT NULL, prefix TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL, owner TEXT, created TEXT
);
-- Who a person is in a system Grimoire pulls from. A connector knows a Slack
-- user id or a Confluence account id; only this table can say which account
-- that is here, and an unmapped identity is nobody rather than everybody.
CREATE TABLE IF NOT EXISTS identities(
  source TEXT NOT NULL, external TEXT NOT NULL, user TEXT NOT NULL,
  PRIMARY KEY(source, external)
);
CREATE INDEX IF NOT EXISTS idx_identities_user ON identities(user);
CREATE TABLE IF NOT EXISTS space_members(
  space TEXT NOT NULL, user TEXT NOT NULL, role TEXT NOT NULL,
  PRIMARY KEY(space, user)
);
CREATE INDEX IF NOT EXISTS idx_space_members_user ON space_members(user);
-- Connectors: configured sources, and what each has already pulled. See
-- internal/connectors.
CREATE TABLE IF NOT EXISTS connectors(
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, name TEXT NOT NULL,
  config TEXT NOT NULL DEFAULT '{}', secret TEXT DEFAULT '',
  prefix TEXT NOT NULL, interval INTEGER DEFAULT 0, enabled INTEGER DEFAULT 1,
  cursor TEXT DEFAULT '', last_run TEXT DEFAULT '', last_ok INTEGER DEFAULT 1,
  last_error TEXT DEFAULT '', docs INTEGER DEFAULT 0, created TEXT
);
CREATE TABLE IF NOT EXISTS connector_docs(
  connector TEXT NOT NULL, external_id TEXT NOT NULL, path TEXT NOT NULL,
  hash TEXT, updated TEXT, PRIMARY KEY(connector, external_id)
);
CREATE INDEX IF NOT EXISTS idx_connector_docs_path ON connector_docs(path);
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS grants(
  token TEXT PRIMARY KEY, secret TEXT, grantee TEXT, scope TEXT,
  expires_at REAL, created TEXT,
  -- 0 means unlimited within the TTL, which is what a grant issued before
  -- these columns existed was.
  max_uses INTEGER NOT NULL DEFAULT 0, uses INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS audit(
  id INTEGER PRIMARY KEY, ts TEXT, action TEXT, secret TEXT, detail TEXT DEFAULT ''
);
-- Who opened a RESTRICTED document, allowed or denied. Only notes with a
-- reader list or outside the commons are recorded, and only single-document
-- reads — never search. See internal/readlog for why both limits are there.
CREATE TABLE IF NOT EXISTS read_audit(
  id INTEGER PRIMARY KEY, at TEXT NOT NULL, user TEXT, name TEXT,
  agent TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL, space TEXT, allowed INTEGER NOT NULL,
  route TEXT, addr TEXT
);
CREATE INDEX IF NOT EXISTS idx_read_audit_path ON read_audit(path);
CREATE INDEX IF NOT EXISTS idx_read_audit_user ON read_audit(user);
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

-- Backfill fts_map for an index built before it existed. Deleting by rowid
-- against an empty map would leave the old row behind and search would answer
-- from both copies, so this runs before any write can. MAX(rowid) per path
-- picks the newest of any duplicates a pre-map build could have left, and the
-- rest are dropped.
INSERT OR REPLACE INTO fts_map(rid, path)
  SELECT MAX(rowid), path FROM fts GROUP BY path;
DELETE FROM fts WHERE rowid NOT IN (SELECT rid FROM fts_map);
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
	// connection keeps write ordering obvious and avoids SQLITE_BUSY WITHIN
	// this process.
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enabling WAL: %w", err)
	}
	// Between processes it is not enough, and a vault regularly has two: the
	// server, and a `grimoire` CLI invocation against the same index. Without a
	// busy timeout the second writer fails instantly with SQLITE_BUSY rather
	// than waiting, and the watcher's handler logs the error and drops the
	// upsert — so a note edited at the wrong moment is silently never indexed.
	// Observed in production: seven "watcher: upsert …: database is locked"
	// lines in one second while a CLI command held the write lock.
	//
	// Five seconds is far longer than any write here takes and far shorter than
	// a person waits before assuming a hang.
	if _, err := conn.Exec("PRAGMA busy_timeout=5000"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setting busy timeout: %w", err)
	}
	if _, err := conn.Exec(Schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	if _, err := conn.Exec(migrations); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrating schema: %w", err)
	}
	// Columns added to existing tables cannot go in Schema — CREATE TABLE IF
	// NOT EXISTS skips a table that is already there — and cannot go in
	// migrations either, since ALTER TABLE ADD COLUMN fails the second time it
	// runs. So they are added conditionally, which also makes them safe to
	// re-apply after a downgrade and re-upgrade.
	for _, c := range addedColumns {
		has, err := hasColumn(conn, c.table, c.column)
		if err != nil {
			conn.Close()
			return nil, err
		}
		if has {
			continue
		}
		if _, err := conn.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s",
			c.table, c.column, c.decl)); err != nil {
			conn.Close()
			return nil, fmt.Errorf("adding %s.%s: %w", c.table, c.column, err)
		}
	}
	// Indexes over columns that may have just been added have to come after
	// them: on an index created before the column existed, Schema would run
	// first and fail on a column that is not there yet.
	if _, err := conn.Exec(lateIndexes); err != nil {
		conn.Close()
		return nil, fmt.Errorf("indexing: %w", err)
	}
	return &DB{conn: conn}, nil
}

const lateIndexes = `
CREATE INDEX IF NOT EXISTS idx_notes_space ON notes(space);
CREATE INDEX IF NOT EXISTS idx_vectors_space ON vectors(space);
CREATE INDEX IF NOT EXISTS idx_notes_untrusted ON notes(untrusted);
`

// addedColumns are columns introduced after their table shipped.
var addedColumns = []struct{ table, column, decl string }{
	{"notes", "space", "TEXT NOT NULL DEFAULT 'commons'"},
	{"vectors", "space", "TEXT NOT NULL DEFAULT 'commons'"},
	// A per-note reader list, for documents whose source knows who may read
	// them. Empty means "governed by the space", which is every note a person
	// writes and every note that existed before this column did.
	{"notes", "acl", "TEXT NOT NULL DEFAULT ''"},
	{"vectors", "acl", "TEXT NOT NULL DEFAULT ''"},
	// Where the text came from, and whether it may be read as instructions.
	// Denormalized onto vectors for the same reason space is: the trust
	// filter has to apply INSIDE ranking, and a join per candidate row would
	// undo the whole point of the corpus cache. Both default to the trusted
	// side, so every note that existed before this column keeps behaving as
	// the operator's own writing — which it was.
	{"notes", "origin", "TEXT NOT NULL DEFAULT ''"},
	// When a person last said this note is still true — the `verified:`
	// frontmatter date. Distinct from `updated`, which a typo bumps: a note
	// edited to fix a spelling is not a note somebody re-checked.
	{"notes", "verified", "TEXT NOT NULL DEFAULT ''"},
	{"notes", "untrusted", "INTEGER NOT NULL DEFAULT 0"},
	{"vectors", "origin", "TEXT NOT NULL DEFAULT ''"},
	{"vectors", "untrusted", "INTEGER NOT NULL DEFAULT 0"},
	// A fact's own origin, which is not the same as the note's: an agent can
	// write a fact it read in an untrusted document into a memory note it owns.
	{"memory_entries", "origin", "TEXT NOT NULL DEFAULT ''"},
	// Whether a PERSON asserted this fact rather than an agent. Reconciliation
	// compares candidates loaded from here, so authorship has to survive the
	// round trip or the authority rule silently stops applying. Defaulting to 0
	// keeps every fact written before this column an agent fact, which is what
	// it was.
	{"memory_entries", "human", "INTEGER NOT NULL DEFAULT 0"},
	// The fact this one contradicts but was not allowed to supersede. Stored so
	// open disagreements are a query rather than a walk of every memory note.
	{"memory_entries", "challenges", "TEXT NOT NULL DEFAULT ''"},
	// Which agent read, as distinct from which account. On a single-user
	// deployment there is no account, so without this the trail can say a
	// restricted note was read and not by what — which is most of the question.
	{"read_audit", "agent", "TEXT NOT NULL DEFAULT ''"},
	// How many times a grant may be redeemed, and how many times it has been.
	// A time window alone bounds nothing about volume: a fifteen-minute grant
	// is fifteen minutes in which an agent may make any number of calls.
	{"grants", "max_uses", "INTEGER NOT NULL DEFAULT 0"},
	{"grants", "uses", "INTEGER NOT NULL DEFAULT 0"},
	// An agent asking for one call should be able to say so; the ask is where
	// the tightest bound is known, because the agent knows what it is about to
	// do and the approver is guessing.
	{"grant_requests", "max_uses", "INTEGER NOT NULL DEFAULT 0"},
}

func hasColumn(conn *sql.DB, table, column string) (bool, error) {
	rows, err := conn.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
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
// ExecAffected runs a write and reports how many rows it changed.
//
// Exists for check-and-set: "claim this if it has not been claimed" is one
// UPDATE whose WHERE clause carries the condition, and the row count is the
// only way to learn whether the condition held. Doing it as a read then a
// write would let two callers both read "available" before either wrote.
func (d *DB) ExecAffected(query string, args ...any) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.conn.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

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
