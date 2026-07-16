"""Local full-text search (FTS5) + tag/graph endpoints."""
from fastapi import APIRouter

from .. import db, index

router = APIRouter(prefix="/api")


def _fts_escape(q: str) -> str:
    # wrap each term as a quoted prefix so user input can't break FTS syntax
    terms = [t for t in q.replace('"', " ").split() if t]
    return " ".join(f'"{t}"*' for t in terms) if terms else '""'


@router.get("/search")
def search(q: str = "", tag: str | None = None, limit: int = 50):
    if not q.strip():
        return []
    match = _fts_escape(q)
    sql = ("SELECT f.path, f.title, snippet(fts, 2, '[', ']', ' … ', 12) AS snippet, "
           "bm25(fts) AS score FROM fts f ")
    params: list = []
    if tag:
        sql += "JOIN tags t ON t.note=f.path AND t.tag=? "
        params.append(tag)
    sql += "WHERE fts MATCH ? ORDER BY score LIMIT ?"
    params += [match, limit]
    try:
        rows = db.query(sql, tuple(params))
    except Exception:
        return []
    return [{"path": r["path"], "title": r["title"], "snippet": r["snippet"]}
            for r in rows]


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
