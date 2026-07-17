"""Local full-text search (FTS5, with tag:/is:pinned/path: operators) + graph."""
import json

from fastapi import APIRouter

from .. import db, index

router = APIRouter(prefix="/api")


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
