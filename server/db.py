"""SQLite index — a rebuildable cache over the vault. FTS5 for local search."""
import sqlite3
import threading

from . import config

_conn: sqlite3.Connection | None = None
_lock = threading.Lock()

SCHEMA = """
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
"""


def init(path=None) -> None:
    """Open (or re-open) the index database.

    Re-initialization CLOSES the previous connection first. Without this, the
    double-init in tests (fixture + app lifespan) leaked a live WAL connection
    per test; hundreds of leaked handles being GC-finalized from arbitrary
    threads caused intermittent interpreter segfaults and cross-test
    IntegrityErrors in full-suite runs.
    """
    global _conn
    with _lock:
        if _conn is not None:
            try:
                _conn.close()
            except Exception:   # noqa: BLE001 — a dying handle must not block re-init
                pass
        p = path or config.db_path()
        p.parent.mkdir(parents=True, exist_ok=True)
        _conn = sqlite3.connect(str(p), check_same_thread=False)
        _conn.row_factory = sqlite3.Row
        _conn.execute("PRAGMA journal_mode=WAL")
        _conn.executescript(SCHEMA)
        _conn.commit()


def close() -> None:
    global _conn
    with _lock:
        if _conn is not None:
            _conn.close()
            _conn = None


def query(sql: str, params: tuple = ()) -> list[dict]:
    with _lock:
        rows = _conn.execute(sql, params).fetchall()
    return [dict(r) for r in rows]


def one(sql: str, params: tuple = ()):
    rows = query(sql, params)
    return rows[0] if rows else None


def execute(sql: str, params: tuple = ()) -> None:
    with _lock:
        _conn.execute(sql, params)
        _conn.commit()


def executemany(sql: str, seq) -> None:
    with _lock:
        _conn.executemany(sql, seq)
        _conn.commit()
