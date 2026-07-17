"""CRDT sync endpoints — exchange and merge per-note replicated body documents."""
from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import crdtstore, db, index, vault
from ..vault import _serialize
from .sync import _conflict_name, _write_raw

router = APIRouter(prefix="/api")


def _norm(path: str) -> str:
    path = path.strip("/")
    return path if path.endswith(".md") else path + ".md"


@router.get("/crdt/doc/{path:path}")
def get_doc(path: str):
    """The note's body CRDT doc + its frontmatter (for deterministic fm merge)."""
    rel = _norm(path)
    if not db.one("SELECT 1 FROM notes WHERE path=?", (rel,)):
        raise HTTPException(404, "no such note")
    note = vault.read(rel)
    if not crdtstore.mergeable(rel, note["body"]):
        raise HTTPException(409, "note is not CRDT-mergeable (encrypted or too large)")
    return {"path": rel, "doc": crdtstore.body_doc_json(rel, note["body"]),
            "fm": note["frontmatter"]}


class MergeIn(BaseModel):
    path: str
    doc: str                 # peer's body CRDT doc
    fm: dict = {}            # peer's frontmatter


@router.post("/crdt/merge")
def merge(m: MergeIn):
    """Merge a peer's body doc into ours; materialize + write the converged note."""
    rel = _norm(m.path)
    exists = vault.safe_path(rel).exists()
    note = vault.read(rel) if exists else {"body": "", "frontmatter": {}, "raw": ""}
    if exists and not crdtstore.mergeable(rel, note["body"]):
        raise HTTPException(409, "note is not CRDT-mergeable")
    merged_body, win_fm, clean = crdtstore.merge_body(
        rel, note["body"], note["frontmatter"], m.doc, m.fm)
    new_raw = _serialize(win_fm, merged_body)
    changed = new_raw != note["raw"]
    conflict = False
    if not clean and changed and note["raw"].strip():
        # independent histories — preserve our version before the peer's replaces it
        cp = _conflict_name(rel)
        _write_raw(cp, note["raw"])
        index.upsert(cp)
        conflict = True
    if changed:
        _write_raw(rel, new_raw)
        index.upsert(rel)
    return {"path": rel, "changed": changed, "conflict": conflict}
