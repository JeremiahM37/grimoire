"""Health + reindex + link autocomplete."""
from fastapi import APIRouter

from .. import config, db, index

router = APIRouter(prefix="/api")


@router.get("/health")
def health():
    notes = db.one("SELECT COUNT(*) c FROM notes")["c"]
    latest = db.one("SELECT MAX(updated) m FROM notes")["m"] or ""
    return {
        "ok": True,
        "vault": str(config.VAULT),
        "notes": notes,
        "tags": db.one("SELECT COUNT(DISTINCT tag) c FROM tags")["c"],
        "unresolved_links": db.one(
            "SELECT COUNT(*) c FROM links WHERE resolved=0")["c"],
        # cheap change signature: the open PWA polls this to notice notes
        # created/edited/deleted OUTSIDE it (device sync, MCP, external editor)
        "rev": f"{notes}:{latest}",
    }


@router.post("/reindex")
def reindex():
    return {"indexed": index.reindex()}


@router.get("/aliases")
def aliases():
    """{alias: path} map so the editor can resolve [[alias]] wiki-links."""
    return index.alias_map()


@router.get("/complete")
def complete(q: str = "", limit: int = 12):
    """`[[` autocomplete: note titles/stems matching a prefix."""
    like = f"%{q.lower()}%"
    rows = db.query(
        "SELECT path, title FROM notes WHERE lower(title) LIKE ? "
        "OR lower(path) LIKE ? ORDER BY updated DESC LIMIT ?", (like, like, limit))
    return [{"path": r["path"], "title": r["title"],
             "stem": r["path"].rsplit("/", 1)[-1][:-3]} for r in rows]
