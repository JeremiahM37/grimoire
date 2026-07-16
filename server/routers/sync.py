"""Delta sync — local-first, conflict copies, never silent data loss.

Protocol:
  1. client GETs /sync/manifest        → {path: {hash, mtime}} for the server
  2. client diffs against its own local manifest
  3. client POSTs /sync/pull {paths}   → contents to bring down
  4. client POSTs /sync/push {changes} → contents to push up; if the server's
     current hash differs from the client's base_hash (concurrent edit), the
     incoming version is written to a CONFLICT COPY and the server copy is kept —
     data is never overwritten blindly.
"""
import time

from fastapi import APIRouter
from pydantic import BaseModel

from .. import db, index, vault

router = APIRouter(prefix="/api")


@router.get("/sync/manifest")
def manifest():
    return {n["path"]: {"hash": n["hash"], "mtime": n["mtime"]}
            for n in db.query("SELECT path, hash, mtime FROM notes")}


class PullIn(BaseModel):
    paths: list[str]


@router.post("/sync/pull")
def pull(p: PullIn):
    out = {}
    for rel in p.paths:
        try:
            out[rel] = vault.read(rel)["raw"]
        except Exception:
            out[rel] = None   # deleted on server
    return {"contents": out}


class Change(BaseModel):
    path: str
    content: str | None = None      # None = delete
    base_hash: str | None = None     # the hash the client last saw (for conflict detect)


class PushIn(BaseModel):
    changes: list[Change]
    device: str = "unknown"


@router.post("/sync/push")
def push(p: PushIn):
    results = []
    for ch in p.changes:
        results.append(_apply(ch))
    return {"results": results}


def _apply(ch: Change) -> dict:
    current = db.one("SELECT hash FROM notes WHERE path=?", (ch.path,))
    cur_hash = current["hash"] if current else None

    if ch.content is None:                          # delete request
        if cur_hash and ch.base_hash and cur_hash != ch.base_hash:
            return {"path": ch.path, "status": "conflict-keep",
                    "detail": "changed on server; not deleting"}
        vault.delete(ch.path); index.remove(ch.path)
        return {"path": ch.path, "status": "deleted"}

    # conflict: server has a different version than the client's base
    if cur_hash and ch.base_hash and cur_hash != ch.base_hash:
        conflict_rel = _conflict_name(ch.path)
        _write_raw(conflict_rel, ch.content)
        index.upsert(conflict_rel)
        return {"path": ch.path, "status": "conflict", "conflict_copy": conflict_rel}

    _write_raw(ch.path, ch.content)
    index.upsert(ch.path)
    return {"path": ch.path, "status": "ok" if cur_hash else "created"}


def _write_raw(rel: str, raw: str) -> None:
    """Write the full note text verbatim (frontmatter + body preserved)."""
    from .. import markdown
    p = vault.safe_path(rel)
    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.with_suffix(p.suffix + ".tmp")
    tmp.write_text(raw, encoding="utf-8")
    import os
    os.replace(tmp, p)
    _ = markdown  # (raw is stored as-is; index re-parses)


def _conflict_name(rel: str) -> str:
    base = rel[:-3] if rel.endswith(".md") else rel
    stamp = time.strftime("%Y%m%d-%H%M%S")
    return f"{base} (conflict {stamp}).md"
