"""Audio memos: upload → transcribe → note with attachment. Browser capture."""
import time

from fastapi import APIRouter, File, Form, UploadFile

from .. import ai, config, index, vault

router = APIRouter(prefix="/api")

ATTACH_DIR = "attachments"


@router.post("/audio", status_code=201)
async def audio_memo(file: UploadFile = File(...), title: str | None = Form(None)):
    """Record → transcribe → note. The audio lands in the vault (so it syncs) and
    a note is created with the transcript + an audio link."""
    data = await file.read()
    stamp = time.strftime("%Y%m%d-%H%M%S")
    ext = (file.filename or "memo.webm").rsplit(".", 1)[-1][:8] or "webm"
    audio_rel = f"{ATTACH_DIR}/{stamp}.{ext}"
    apath = vault.safe_path(audio_rel.replace(f".{ext}", ".md")).parent / f"{stamp}.{ext}"
    apath.parent.mkdir(parents=True, exist_ok=True)
    apath.write_bytes(data)

    transcript = ai.transcribe(data, file.filename or "memo.webm")
    note_title = title or f"Audio memo {stamp}"
    rel = f"{config.INBOX_DIR}/{stamp}-audio.md"
    body = f"🎙 [audio](../{audio_rel})\n\n{transcript}"
    vault.write(rel, body, {"title": note_title, "tags": ["audio", "capture"]})
    index.upsert(rel)
    return {"path": rel, "audio": audio_rel, "transcript": transcript}
