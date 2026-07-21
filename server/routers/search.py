"""Local full-text search (FTS5, with tag:/is:pinned/path: operators) + graph
+ tag rename."""
import json
import re

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import db, index, queries, vault
from ..vault import VaultError

router = APIRouter(prefix="/api")


class QueryIn(BaseModel):
    block: str          # the raw text inside a ```query fence


@router.post("/query")
def run_query(q: QueryIn):
    """Execute a live-query block for the PWA's preview renderer. The
    authenticated app may see private notes; /read and export never call this —
    they run queries server-side with include_private=False."""
    return queries.run(q.block, include_private=True)


def _fts_escape(q: str, op: str = " ") -> str:
    # wrap each term as a quoted prefix so user input can't break FTS syntax
    terms = [t for t in q.replace('"', " ").split() if t]
    return op.join(f'"{t}"*' for t in terms) if terms else '""'


def _is_pinned(path: str) -> bool:
    r = db.one("SELECT frontmatter_json FROM notes WHERE path=?", (path,))
    try:
        return bool(json.loads(r["frontmatter_json"] or "{}").get("pinned")) if r else False
    except Exception:
        return False


@router.get("/search")
def search(q: str = "", tag: str | None = None, limit: int = 50, full: bool = False):
    # operators: tag:X  is:pinned  path:X  — the rest is full-text
    op_tag, want_pinned, path_like, terms = tag, False, None, []
    for tok in q.split():
        low = tok.lower()
        if low.startswith("tag:"):
            op_tag = tok[4:]
        elif low in ("is:pinned", "is:pin"):
            want_pinned = True
        elif low.startswith("path:"):
            path_like = tok[5:].lower()
        else:
            terms.append(tok)
    text = " ".join(terms).strip()

    if text:
        sql = ("SELECT f.path, f.title, snippet(fts, 2, '[', ']', ' … ', 12) AS snippet, "
               "bm25(fts) AS score FROM fts f WHERE fts MATCH ? ORDER BY score LIMIT 500")
        try:
            rows = db.query(sql, (_fts_escape(text),))
            if not rows and len(text.split()) > 1:
                # natural-language queries rarely match EVERY term — fall back
                # to any-term so a question still surfaces its best notes
                rows = db.query(sql, (_fts_escape(text, op=" OR "),))
        except Exception:
            return []
    elif op_tag or want_pinned or path_like:
        rows = db.query("SELECT path, title, '' AS snippet FROM notes ORDER BY updated DESC LIMIT 500")
    else:
        return []

    out = []
    for r in rows:
        if op_tag and not db.one("SELECT 1 FROM tags WHERE note=? AND tag=?", (r["path"], op_tag)):
            continue
        if path_like and path_like not in r["path"].lower():
            continue
        if want_pinned and not _is_pinned(r["path"]):
            continue
        hit = {"path": r["path"], "title": r["title"], "snippet": r["snippet"]}
        if full:
            # agents opt in to bodies to avoid a search→read round-trip per
            # hit; encrypted notes stay sealed (their body is ciphertext, so
            # return nothing rather than noise). Long notes come back as the
            # query-relevant excerpt, not the whole body — read the note for
            # the rest.
            row = db.one("SELECT body FROM notes WHERE path=?", (r["path"],))
            from .. import secrets as _secrets
            body = (row or {}).get("body") or ""
            if _secrets.is_encrypted(body):
                body = ""
            elif len(body) > 2400:
                body = _excerpt(body, terms)
                hit["excerpted"] = True
            hit["body"] = body
        out.append(hit)
        if len(out) >= limit:
            break
    return out




def _excerpt(body: str, terms: list[str], budget: int = 2400) -> str:
    """The most query-relevant ~budget chars of a long body: score its chunks
    by how many query terms they contain, keep the best ones in document
    order. With no scoring signal, fall back to the head of the note."""
    from .. import ai
    chunks = ai.chunk_text(body)
    toks = [t.lower() for t in terms]
    scored = sorted(range(len(chunks)),
                    key=lambda i: -sum(1 for t in toks if t in chunks[i].lower()))
    keep, used = set(), 0
    for i in scored:
        if used >= budget:
            break
        keep.add(i)
        used += len(chunks[i])
    return "\n[…]\n".join(chunks[i] for i in sorted(keep))



@router.get("/tags")
def tags():
    return db.query("SELECT tag, COUNT(*) c FROM tags GROUP BY tag ORDER BY c DESC")


@router.get("/facts")
def facts(key: str = "", note: str = "", include_private: bool = False):
    """Structured `key:: value` facts projected from note bodies — a
    deterministic lookup layer over the same markdown. Filter by key and/or
    note. Private notes' facts are excluded unless include_private."""
    conds, params = [], []
    if key:
        conds.append("key=?")
        params.append(key.strip().lower())
    if note:
        conds.append("note=?")
        params.append(note.strip())
    if not include_private:
        conds.append("private=0")
    where = (" WHERE " + " AND ".join(conds)) if conds else ""
    return db.query(f"SELECT note, key, value FROM facts{where} ORDER BY key, note",
                    tuple(params))


class FactIn(BaseModel):
    note: str
    key: str
    value: str


@router.post("/facts", status_code=201)
def set_fact(f: FactIn):
    """Write a `key:: value` fact into a note (markdown stays the source of
    truth): update the existing line for that key, else append it."""
    rel, key, val = f.note.strip(), f.key.strip(), f.value.strip()
    if not rel or not key or not val:
        raise HTTPException(400, "note, key and value are required")
    try:
        n = vault.read(rel)
    except VaultError:
        raise HTTPException(404, "no such note") from None
    if n.get("encrypted"):
        raise HTTPException(400, "cannot set a fact on an encrypted note")
    pat = re.compile(rf"^(\s*(?:[-*]\s+)?){re.escape(key)}\s*::\s+.*$",
                     re.IGNORECASE | re.MULTILINE)
    body = n["body"]
    if pat.search(body):
        body = pat.sub(lambda m: f"{m.group(1)}{key}:: {val}", body, count=1)
    else:
        body = body.rstrip() + f"\n\n{key}:: {val}\n"
    vault.write(rel, body, n["frontmatter"])
    index.upsert(rel)
    return {"note": rel, "key": key.lower(), "value": val}


class TagRename(BaseModel):
    old: str
    new: str


@router.post("/tags/rename")
def rename_tag(r: TagRename):
    """Rename #old → #new across every note (body occurrences + frontmatter tags).
    Encrypted notes: only their frontmatter tags change (ciphertext body untouched)."""
    old = r.old.strip().lstrip("#")
    new = r.new.strip().lstrip("#")
    if not old or not new:
        raise HTTPException(400, "old and new tag names required")
    # match '#old' as a whole tag (not '#oldsuffix' or 'word#old')
    pat = re.compile(r"(?<![\w#/])#" + re.escape(old) + r"(?![\w/-])")
    affected = 0
    for row in db.query("SELECT DISTINCT note FROM tags WHERE tag=?", (old,)):
        rel = row["note"]
        try:
            note = vault.read(rel)
        except VaultError:
            continue
        fm = dict(note["frontmatter"])
        body = note["body"] if note.get("encrypted") else pat.sub("#" + new, note["body"])
        fmtags = fm.get("tags")
        if isinstance(fmtags, list):
            fm["tags"] = [new if str(t) == old else t for t in fmtags]
        elif isinstance(fmtags, str) and fmtags == old:
            fm["tags"] = new
        vault.write(rel, body, fm)
        index.upsert(rel)
        affected += 1
    return {"renamed": old, "to": new, "notes": affected}


@router.get("/graph")
def graph():
    nodes = [{"id": n["path"], "title": n["title"]}
             for n in db.query("SELECT path, title FROM notes")]
    edges = [{"src": e["src"], "dst": e["dst"]}
             for e in db.query("SELECT src, dst FROM links WHERE resolved=1")]
    unresolved = db.query(
        "SELECT DISTINCT target FROM links WHERE resolved=0 ORDER BY target LIMIT 200")
    return {"nodes": nodes, "edges": edges,
            "unresolved": [u["target"] for u in unresolved]}
