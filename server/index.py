"""Reconciler: vault files → SQLite index. Files are truth; the index is a cache.

`reindex()` rebuilds everything; `upsert(rel)` / `remove(rel)` handle single-note
changes (from the API or the watcher). Backlinks fall out of the links table.
"""
import collections
import functools
import json
import math
import re

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
    db.execute("DELETE FROM vectors WHERE note=?", (rel,))
    db.execute("DELETE FROM facts WHERE note=?", (rel,))   # queried without a JOIN
    remove_crdt(rel)
    _bump_rev()               # the retrieval corpus (notes⋈vectors) changed
    _resolve_all()


def ensure_embed_signature() -> bool:
    """Re-embed the vault when the embedding backend changed (Ollama added or
    removed, local model installed…) — cosine over mixed-backend vectors is
    meaningless. Returns True when a re-embed ran."""
    sig = ai.embed_signature()
    row = db.one("SELECT value FROM meta WHERE key='embed_sig'")
    changed = bool(row) and row["value"] != sig
    if changed:
        reindex()
    if not row or changed:
        db.execute("INSERT INTO meta(key,value) VALUES('embed_sig',?) "
                   "ON CONFLICT(key) DO UPDATE SET value=excluded.value", (sig,))
    return changed


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
    _bump_rev()               # this note's vectors/rows change on every write
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
    db.execute("DELETE FROM facts WHERE note=?", (rel,))
    if not encrypted:      # never mine facts out of ciphertext
        facts = extract_facts(note["body"])
        if facts:
            priv = int(note["private"])
            db.executemany("INSERT INTO facts(note,key,value,private) VALUES(?,?,?,?)",
                           [(rel, k, v, priv) for k, v in facts])


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


_WORD_RE = re.compile(r"[a-z0-9]+")

# Dataview-style inline field: `key:: value` (optionally a list item). `::`
# must directly follow the key (no space) — otherwise ordinary prose with a
# stray " :: " reads as a fact. Keys are single tokens (letters, digits, -, _,
# /). Fenced code is skipped so `foo:: bar` in a snippet isn't mistaken for one.
_FACT_RE = re.compile(r"^\s*(?:[-*]\s+)?([A-Za-z][\w/-]{0,48})::\s+(\S.*?)\s*$")


def extract_facts(body: str) -> list[tuple[str, str]]:
    """[(key, value)] structured facts declared in a note body as `key:: value`.
    Keys are lowercased/trimmed; values kept verbatim. A projection of the
    markdown — agents can look these up deterministically instead of hoping RAG
    surfaces the right sentence."""
    out, in_fence = [], False
    for line in body.splitlines():
        if line.lstrip().startswith("```"):
            in_fence = not in_fence
            continue
        if in_fence:
            continue
        m = _FACT_RE.match(line)
        if m:
            out.append((m.group(1).strip().lower(), m.group(2).strip()))
    return out


@functools.lru_cache(maxsize=65536)
def _chunk_counts(chunk: str):
    """Term counts for one chunk, cached — chunks are immutable strings, so
    repeated queries never re-tokenize the vault."""
    return collections.Counter(_WORD_RE.findall(chunk.lower()))


def _df(counts, terms):
    """Document frequency of `terms` over the chunk list."""
    df = {}
    for tc in counts:
        for t in terms:
            if t in tc:
                df[t] = df.get(t, 0) + 1
    return df


def _bm25(terms, tc, df, n_chunks, avglen, k1=1.2, b=0.75):
    """Okapi BM25 over one chunk: IDF-weighted with term-frequency saturation
    and length normalization, so a term mentioned thrice beats once-mentioned
    but thirty mentions don't drown everything else."""
    norm = k1 * (1 - b + b * tc.total() / avglen)
    score = 0.0
    for t in terms:
        tf = tc.get(t, 0)
        if tf and df.get(t):
            score += math.log(n_chunks / df[t]) * tf * (k1 + 1) / (tf + norm)
    return score


try:
    import numpy as _np
except Exception:                          # numpy optional — zero-dep still works
    _np = None

_rev = 0
_vec_cache: dict = {}


def _bump_rev() -> None:
    """Monotonic index-mutation counter — invalidates the retrieval corpus
    cache. A counter (not a table checksum) because SQLite can reuse freed
    rowids, so an edit-in-place could otherwise look unchanged."""
    global _rev
    _rev += 1


def _corpus(include_private: bool):
    """Aligned (rows, matrix) for the vector store, cached until the index
    mutates. With numpy the matrix lets cosine be one matmul instead of a
    Python loop over every chunk — the difference between ~1s and ~20ms on a
    50k-chunk vault. Without numpy, matrix is None and callers loop."""
    key = (include_private, _rev)
    hit = _vec_cache.get("data")
    if hit and hit["key"] == key:
        return hit["rows"], hit["mat"]
    sql = "SELECT v.note, v.chunk, v.chunk_idx AS ci, v.embedding, n.title " \
          "FROM vectors v JOIN notes n ON n.path=v.note"
    if not include_private:
        sql += " WHERE v.private=0"
    rows = db.query(sql)
    mat = None
    if _np is not None and rows:
        mat = _np.stack([_np.frombuffer(r["embedding"], dtype="<f4") for r in rows])
    _vec_cache["data"] = {"key": key, "rows": rows, "mat": mat}
    return rows, mat


def retrieve(query: str, k: int = 6, include_private: bool = False) -> list[dict]:
    """Top-k note chunks, hybrid-ranked: embedding cosine similarity fused
    (reciprocal-rank) with an IDF-weighted lexical overlap score. The lexical
    leg makes rare, discriminative query words (names, places, error codes)
    count for more than filler — which pure cosine, especially the offline
    hashing embedder, does not. Private notes are excluded unless
    include_private=True."""
    qv = ai.embed([query])[0]
    qtoks = set(_WORD_RE.findall(query.lower()))
    rows, mat = _corpus(include_private)
    counts = [_chunk_counts(r["chunk"]) for r in rows]
    n_chunks = max(len(rows), 1)
    avglen = (sum(c.total() for c in counts) / n_chunks) or 1.0
    df = _df(counts, qtoks)
    if mat is not None:                    # vectorized cosine (one matmul)
        cosines = mat @ _np.asarray(qv, dtype="<f4")
    else:
        cosines = [ai.cosine(qv, ai.unpack(r["embedding"])) for r in rows]
    scored = []
    for r, tc, cos in zip(rows, counts, cosines, strict=False):
        cos = float(cos)
        lex = _bm25(qtoks, tc, df, n_chunks, avglen)
        if cos > 0 or lex > 0:
            scored.append({"path": r["note"], "title": r["title"], "ci": r["ci"],
                           "chunk": r["chunk"], "cos": cos, "lex": lex, "tc": tc})

    for key in ("cos", "lex"):
        for rank, st in enumerate(sorted(scored, key=lambda x: -x[key])):
            st["rrf"] = st.get("rrf", 0.0) + 1.0 / (60 + rank)
    for st in scored:
        st["score"] = round(st.pop("rrf", 0.0), 4)
        del st["cos"], st["lex"], st["tc"]
    scored.sort(key=lambda x: -x["score"])
    # de-dupe so one note doesn't dominate: keep best chunk per note first, then fill
    seen, primary, extra = set(), [], []
    for s in scored:
        (primary if s["path"] not in seen else extra).append(s)
        seen.add(s["path"])
    ranked = primary + extra
    # small-to-big: rank on small chunks, but return the top hits with their
    # neighbouring chunks merged in — answers often straddle a chunk boundary
    out, covered = [], set()
    for s in ranked:
        if len(out) >= k:
            break
        if (s["path"], s["ci"]) in covered:
            continue          # already inside an earlier hit's neighbourhood
        if len(out) < 3:
            near = db.query("SELECT chunk_idx, chunk FROM vectors WHERE note=? "
                            "AND chunk_idx BETWEEN ? AND ? ORDER BY chunk_idx",
                            (s["path"], s["ci"] - 1, s["ci"] + 1))
            s["chunk"] = "\n".join(r["chunk"] for r in near)
            covered.update((s["path"], r["chunk_idx"]) for r in near)
        else:
            covered.add((s["path"], s["ci"]))
        del s["ci"]
        out.append(s)
    return out


def backlinks(rel: str) -> list[dict]:
    """Notes that link TO this one (resolved), with the source title."""
    return db.query(
        "SELECT DISTINCT l.src AS path, n.title, l.alias FROM links l "
        "JOIN notes n ON n.path=l.src WHERE l.dst=? ORDER BY n.title", (rel,))
