"""Canvases — visual boards stored as `.canvas` files in the vault, using the
open JSON Canvas format (https://jsoncanvas.org): `{nodes: [...], edges: [...]}`
with `text` and `file` node types, so boards interoperate with other
JSON Canvas apps.

Validation is structural, not semantic: unknown extra keys are preserved
verbatim (spec-compatible round-tripping), but the document must be an object
with node/edge lists of objects, ids must be strings, and the file is capped —
a canvas is metadata, not a data store.
"""
import json

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import vault
from ..vault import VaultError

router = APIRouter(prefix="/api")

MAX_CANVAS_BYTES = 1024 * 1024        # 1 MB is an enormous canvas


def _canvas_path(rel: str):
    rel = rel if rel.endswith(".canvas") else rel + ".canvas"
    p = vault.safe_raw_path(rel)
    return rel, p


def _validate(doc: dict) -> dict:
    if not isinstance(doc, dict):
        raise HTTPException(400, "canvas must be a JSON object")
    nodes, edges = doc.get("nodes", []), doc.get("edges", [])
    if not isinstance(nodes, list) or not isinstance(edges, list):
        raise HTTPException(400, "nodes and edges must be lists")
    ids = set()
    for n in nodes:
        if not isinstance(n, dict) or not isinstance(n.get("id"), str):
            raise HTTPException(400, "every node needs a string id")
        ids.add(n["id"])
    for e in edges:
        if not isinstance(e, dict) or not isinstance(e.get("id"), str):
            raise HTTPException(400, "every edge needs a string id")
        if e.get("fromNode") not in ids or e.get("toNode") not in ids:
            raise HTTPException(400, "edge endpoints must reference existing nodes")
    return {"nodes": nodes, "edges": edges,
            **{k: v for k, v in doc.items() if k not in ("nodes", "edges")}}


@router.get("/canvas")
def list_canvases():
    root = vault.vault_root()
    out = []
    for p in sorted(root.rglob("*.canvas")):
        rel = p.relative_to(root).as_posix()
        if ".grimoire" in p.parts:
            continue
        out.append({"path": rel, "name": p.stem, "mtime": p.stat().st_mtime})
    return out


class CanvasIn(BaseModel):
    name: str


@router.post("/canvas", status_code=201)
def create_canvas(c: CanvasIn):
    slug = vault.slugify(c.name) or "canvas"
    try:
        rel, p = _canvas_path(f"canvases/{slug}")
    except VaultError as e:
        raise HTTPException(400, str(e)) from None
    if p.exists():
        raise HTTPException(409, "canvas already exists")
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(json.dumps({"nodes": [], "edges": []}), encoding="utf-8")
    return {"path": rel, "name": p.stem}


@router.get("/canvas/{path:path}")
def get_canvas(path: str):
    try:
        rel, p = _canvas_path(path)
    except VaultError as e:
        raise HTTPException(400, str(e)) from None
    if not p.is_file():
        raise HTTPException(404, "no such canvas")
    try:
        return {"path": rel, **_validate(json.loads(p.read_text(encoding="utf-8")))}
    except json.JSONDecodeError:
        raise HTTPException(422, "canvas file is not valid JSON") from None


class CanvasUpdate(BaseModel):
    nodes: list
    edges: list


@router.put("/canvas/{path:path}")
def put_canvas(path: str, c: CanvasUpdate):
    try:
        rel, p = _canvas_path(path)
    except VaultError as e:
        raise HTTPException(400, str(e)) from None
    if not p.is_file():
        raise HTTPException(404, "no such canvas")
    doc = _validate({"nodes": c.nodes, "edges": c.edges})
    blob = json.dumps(doc, indent=1)
    if len(blob) > MAX_CANVAS_BYTES:
        raise HTTPException(413, "canvas too large")
    p.write_text(blob, encoding="utf-8")
    return {"path": rel, "nodes": len(c.nodes), "edges": len(c.edges)}


@router.delete("/canvas/{path:path}", status_code=204)
def delete_canvas(path: str):
    try:
        _, p = _canvas_path(path)
    except VaultError as e:
        raise HTTPException(400, str(e)) from None
    if not p.is_file():
        raise HTTPException(404, "no such canvas")
    p.unlink()
