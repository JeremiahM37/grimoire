"""Reconciler: vault files → SQLite index. Files are truth; the index is a cache.

`reindex()` rebuilds everything; `upsert(rel)` / `remove(rel)` handle single-note
changes (from the API or the watcher). Backlinks fall out of the links table.
"""
import json

from . import ai, db, vault


def upsert(rel: str) -> dict:
    """Index a single note from its file, resolving its links. Returns the note.
    Files under reserved dirs (templates/, .grimoire/) are never indexed."""
    note = vault.read(rel)
    if vault.is_reserved(rel):
        return note
    _write_note_rows(note)
    _resolve_all()   # a new/edited note can resolve others' dangling links
    try:
        from . import crdtstore
        crdtstore.update_from_body(rel, note["body"])   # track for CRDT sync
    except Exception:
        pass
    return note


def remove_crdt(rel: str) -> None:
    try:
        from . import crdtstore
        crdtstore.delete_doc(rel)
    except Exception:
        pass


def remove(rel: str) -> None:
    for tbl in ("notes", "fts"):
        db.execute(f"DELETE FROM {tbl} WHERE path=?", (rel,))
    db.execute("DELETE FROM links WHERE src=?", (rel,))
    db.execute("DELETE FROM tags WHERE note=?", (rel,))
    remove_crdt(rel)
    _resolve_all()


def reindex() -> int:
    """Full rebuild from the vault. Returns note count."""
    for tbl in ("notes", "links", "tags", "fts"):
        db.execute(f"DELETE FROM {tbl}")
    n = 0
    for p in vault.walk():
        try:
            note = vault.note_from_text(vault.rel_of(p), p.read_text(encoding="utf-8"),
                                        p.stat().st_mtime)
        except Exception:
            continue
        _write_note_rows(note)
        n += 1
    _resolve_all()
    return n


def _write_note_rows(note: dict) -> None:
    rel = note["path"]
    db.execute("DELETE FROM notes WHERE path=?", (rel,))
    db.execute("DELETE FROM fts WHERE path=?", (rel,))
    db.execute("DELETE FROM links WHERE src=?", (rel,))
    db.execute("DELETE FROM tags WHERE note=?", (rel,))
    db.execute("DELETE FROM vectors WHERE note=?", (rel,))
    encrypted = note.get("encrypted")
    if not encrypted:
        _embed_note(note)   # NEVER embed ciphertext — encrypted notes stay out of RAG
    fm = note["frontmatter"]
    db.execute(
        "INSERT INTO notes(path,title,body,frontmatter_json,private,mtime,hash,created,updated)"
        " VALUES(?,?,?,?,?,?,?,?,?)",
        (rel, note["title"], note["body"], json.dumps(fm), int(note["private"]),
         note["mtime"], note["hash"], fm.get("created", ""), fm.get("updated", "")))
    # index only the title for encrypted notes — the ciphertext body is never searchable
    db.execute("INSERT INTO fts(path,title,body) VALUES(?,?,?)",
               (rel, note["title"], "" if encrypted else note["body"]))
    if note["links"]:
        db.executemany(
            "INSERT INTO links(src,target,alias,resolved) VALUES(?,?,?,0)",
            [(rel, l_["target"], l_["alias"]) for l_ in note["links"]])
    if note["tags"]:
        db.executemany("INSERT INTO tags(note,tag) VALUES(?,?)",
                       [(rel, t) for t in note["tags"]])


def _embed_note(note: dict) -> None:
    """Chunk + embed a note into the vector store. Private notes are stored with a
    flag so RAG can exclude them by default (and opt in per query)."""
    chunks = ai.chunk_text(f"{note['title']}\n\n{note['body']}")
    if not chunks:
        return
    vecs = ai.embed(chunks)
    priv = 1 if note["private"] else 0
    db.executemany(
        "INSERT INTO vectors(note,chunk_idx,chunk,embedding,private) VALUES(?,?,?,?,?)",
        [(note["path"], i, c, ai.pack(v), priv) for i, (c, v) in enumerate(zip(chunks, vecs, strict=False))])


def _resolve_all() -> None:
    """Resolve every link's target → a note path (by title, filename stem, or a
    frontmatter alias)."""
    notes = db.query("SELECT path, title, frontmatter_json FROM notes")
    by_title, by_stem, by_alias = {}, {}, {}
    for n in notes:
        by_title[n["title"].lower()] = n["path"]
        stem = n["path"].rsplit("/", 1)[-1][:-3].lower()   # filename without .md
        by_stem.setdefault(stem, n["path"])
        for a in _aliases(n["frontmatter_json"]):
            by_alias.setdefault(a.lower(), n["path"])
    for link in db.query("SELECT rowid, target FROM links"):
        key = link["target"].lower()
        dst = by_title.get(key) or by_stem.get(key) or by_alias.get(key)
        db.execute("UPDATE links SET dst=?, resolved=? WHERE rowid=?",
                   (dst, 1 if dst else 0, link["rowid"]))


def _aliases(frontmatter_json: str) -> list[str]:
    try:
        a = json.loads(frontmatter_json or "{}").get("aliases")
    except Exception:
        return []
    if isinstance(a, str):
        return [a]
    if isinstance(a, list):
        return [str(x) for x in a]
    return []


def alias_map() -> dict:
    """{alias_lower: path} across all notes — for link resolution in the UI."""
    out = {}
    for n in db.query("SELECT path, frontmatter_json FROM notes"):
        for a in _aliases(n["frontmatter_json"]):
            out.setdefault(a.lower(), n["path"])
    return out


def retrieve(query: str, k: int = 6, include_private: bool = False) -> list[dict]:
    """Top-k note chunks by cosine similarity to the query embedding.
    Private notes are excluded unless include_private=True."""
    qv = ai.embed([query])[0]
    sql = "SELECT v.note, v.chunk, v.embedding, n.title FROM vectors v " \
          "JOIN notes n ON n.path=v.note"
    if not include_private:
        sql += " WHERE v.private=0"
    scored = []
    for r in db.query(sql):
        score = ai.cosine(qv, ai.unpack(r["embedding"]))
        if score > 0:
            scored.append({"path": r["note"], "title": r["title"],
                           "chunk": r["chunk"], "score": round(score, 4)})
    scored.sort(key=lambda x: -x["score"])
    # de-dupe so one note doesn't dominate: keep best chunk per note first, then fill
    seen, primary, extra = set(), [], []
    for s in scored:
        (primary if s["path"] not in seen else extra).append(s)
        seen.add(s["path"])
    return (primary + extra)[:k]


def backlinks(rel: str) -> list[dict]:
    """Notes that link TO this one (resolved), with the source title."""
    return db.query(
        "SELECT DISTINCT l.src AS path, n.title, l.alias FROM links l "
        "JOIN notes n ON n.path=l.src WHERE l.dst=? ORDER BY n.title", (rel,))
