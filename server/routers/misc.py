"""Health + reindex + link autocomplete + task aggregation + vault export/import."""
import io
import re
import zipfile

from fastapi import APIRouter, File, HTTPException, UploadFile
from fastapi.responses import Response

from .. import config, db, index, vault
from ..vault import VaultError

router = APIRouter(prefix="/api")

_TASK_RE = re.compile(r"^\s*[-*]\s+\[([ xX])\]\s+(.*)$")


@router.get("/export/vault")
def export_vault():
    """Download the whole vault as a .zip (all files except the internal .grimoire/
    dir — so the secret store and rebuildable index are never included)."""
    root = vault.vault_root()
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
        if root.exists():
            for p in sorted(root.rglob("*")):
                if p.is_file() and ".grimoire" not in p.parts:
                    z.write(p, arcname=str(p.relative_to(root)))
    return Response(buf.getvalue(), media_type="application/zip",
                    headers={"Content-Disposition": 'attachment; filename="grimoire-vault.zip"'})


@router.post("/import/vault")
async def import_vault(file: UploadFile = File(...)):
    """Extract a .zip into the vault. Each entry is confined via safe_raw_path
    (zip-slip protection), .grimoire entries are refused, and total uncompressed
    size is capped (zip-bomb protection). Existing files are overwritten."""
    data = await file.read()
    if len(data) > 200 * 1024 * 1024:
        raise HTTPException(413, "archive too large")
    imported, skipped, total = 0, 0, 0
    CAP = 500 * 1024 * 1024
    try:
        zf = zipfile.ZipFile(io.BytesIO(data))
    except zipfile.BadZipFile:
        raise HTTPException(400, "not a valid zip archive") from None
    with zf:
        for info in zf.infolist():
            if info.is_dir():
                continue
            total += info.file_size
            if total > CAP:
                raise HTTPException(413, "archive expands too large")
            name = info.filename.replace("\\", "/")
            if ".grimoire" in name.split("/"):
                skipped += 1
                continue
            try:
                p = vault.safe_raw_path(name)   # zip-slip: confine to the vault
            except VaultError:
                skipped += 1
                continue
            p.parent.mkdir(parents=True, exist_ok=True)
            with zf.open(info) as src:
                p.write_bytes(src.read())
            imported += 1
    index.reindex()
    return {"imported": imported, "skipped": skipped}


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


@router.get("/tasks")
def tasks(include_done: bool = False):
    """Every `- [ ]` / `- [x]` task across the vault (encrypted notes excluded —
    their ciphertext body has no parseable tasks). Open tasks first."""
    out = []
    for r in db.query("SELECT path, title, body FROM notes ORDER BY updated DESC"):
        for i, line in enumerate((r["body"] or "").split("\n")):
            m = _TASK_RE.match(line)
            if not m:
                continue
            done = m.group(1).lower() == "x"
            if done and not include_done:
                continue
            out.append({"path": r["path"], "title": r["title"], "line": i,
                        "text": m.group(2).strip(), "done": done})
    out.sort(key=lambda t: t["done"])   # open tasks first
    return out


@router.get("/complete")
def complete(q: str = "", limit: int = 12):
    """`[[` autocomplete: note titles/stems matching a prefix."""
    like = f"%{q.lower()}%"
    rows = db.query(
        "SELECT path, title FROM notes WHERE lower(title) LIKE ? "
        "OR lower(path) LIKE ? ORDER BY updated DESC LIMIT ?", (like, like, limit))
    return [{"path": r["path"], "title": r["title"],
             "stem": r["path"].rsplit("/", 1)[-1][:-3]} for r in rows]
