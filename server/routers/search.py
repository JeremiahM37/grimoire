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


def _fts_escape(q: str) -> str:
    # wrap each term as a quoted prefix so user input can't break FTS syntax
    terms = [t for t in q.replace('"', " ").split() if t]
    return " ".join(f'"{t}"*' for t in terms) if terms else '""'


def _is_pinned(path: str) -> bool:
    r = db.one("SELECT frontmatter_json FROM notes WHERE path=?", (path,))
    try:
        return bool(json.loads(r["frontmatter_json"] or "{}").get("pinned")) if r else False
    except Exception:
        return False


@router.get("/search")
def search(q: str = "", tag: str | None = None, limit: int = 50):
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
        try:
            rows = db.query(
                "SELECT f.path, f.title, snippet(fts, 2, '[', ']', ' … ', 12) AS snippet, "
                "bm25(fts) AS score FROM fts f WHERE fts MATCH ? ORDER BY score LIMIT 500",
                (_fts_escape(text),))
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
        out.append({"path": r["path"], "title": r["title"], "snippet": r["snippet"]})
        if len(out) >= limit:
            break
    return out


@router.get("/tags")
def tags():
    return db.query("SELECT tag, COUNT(*) c FROM tags GROUP BY tag ORDER BY c DESC")


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
