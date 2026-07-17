"""Audio memos: upload → transcribe → note with attachment. Browser capture.
Generic attachments (images/files) embedded via ![[path]] / [[path]]."""
import time

from fastapi import APIRouter, File, Form, HTTPException, UploadFile
from fastapi.responses import FileResponse

from .. import ai, config, index, vault
from ..vault import VaultError

router = APIRouter(prefix="/api")

ATTACH_DIR = "attachments"
IMAGE_EXTS = {"png", "jpg", "jpeg", "gif", "webp", "svg", "avif", "bmp", "heic"}


@router.post("/attach", status_code=201)
async def attach(file: UploadFile = File(...)):
    """Store an uploaded image/file in the vault (so it syncs) and return the
    relative path the editor embeds as ![[path]] (image) or [[path]] (file)."""
    data = await file.read()
    if len(data) > 25 * 1024 * 1024:
        raise HTTPException(413, "attachment too large (25 MB max)")
    stamp = time.strftime("%Y%m%d-%H%M%S")
    name = (file.filename or "file").rsplit("/", 1)[-1].rsplit("\\", 1)[-1]
    base, _, ext = name.rpartition(".")
    ext = (ext.lower()[:8] if base else "bin")
    slug = vault.slugify(base or name)[:40] or "file"
    rel = f"{ATTACH_DIR}/{stamp}-{slug}.{ext}"
    try:
        p = vault.safe_raw_path(rel)   # sandboxed, but keeps the real extension
    except VaultError as e:
        raise HTTPException(400, str(e))
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_bytes(data)
    return {"path": rel, "is_image": ext in IMAGE_EXTS, "name": name, "bytes": len(data)}


@router.get("/file/{path:path}")
def get_file(path: str):
    """Serve a raw vault file (attachments, images) for embeds and the e-ink view."""
    try:
        p = vault.safe_raw_path(path.strip("/"))
    except VaultError:
        raise HTTPException(400, "bad path")
    if not p.exists() or not p.is_file():
        raise HTTPException(404, "no such file")
    return FileResponse(p)


@router.post("/audio", status_code=201)
async def audio_memo(file: UploadFile = File(...), title: str | None = Form(None)):
    """Record → transcribe → note. The audio lands in the vault (so it syncs) and
    a note is created with the transcript + an audio link."""
    data = await file.read()
    stamp = time.strftime("%Y%m%d-%H%M%S")
    ext = (file.filename or "memo.webm").rsplit(".", 1)[-1][:8] or "webm"
    audio_rel = f"{ATTACH_DIR}/{stamp}.{ext}"
    apath = vault.safe_raw_path(audio_rel)
    apath.parent.mkdir(parents=True, exist_ok=True)
    apath.write_bytes(data)

    transcript = ai.transcribe(data, file.filename or "memo.webm")
    note_title = title or f"Audio memo {stamp}"
    rel = f"{config.INBOX_DIR}/{stamp}-audio.md"
    body = f"🎙 [audio](../{audio_rel})\n\n{transcript}"
    vault.write(rel, body, {"title": note_title, "tags": ["audio", "capture"]})
    index.upsert(rel)
    return {"path": rel, "audio": audio_rel, "transcript": transcript}
