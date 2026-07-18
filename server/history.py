"""Note version history — automatic file-recovery snapshots.

Every content-changing save snapshots the note's *previous* on-disk body into
`.grimoire/history/<note-slug>/<millis>.md` before the write. A per-note ring
buffer keeps the newest `KEEP` versions; identical consecutive snapshots are
skipped. Restoring never discards work: the current body is snapshotted first,
so a restore is itself undoable.

Security: what is snapshotted is whatever is on disk — for an encrypted note
that's the ciphertext. Plaintext of encrypted notes never lands in history.
"""
from __future__ import annotations

import re
import time
from pathlib import Path

from . import config

KEEP = 25                      # newest versions kept per note
_ID_RE = re.compile(r"^\d{10,16}$")


def _dir_for(rel: str) -> Path:
    # flatten the note path into one safe directory name: journal/2026.md → journal__2026.md
    return config.grimoire_dir() / "history" / rel.replace("/", "__")


def snapshot(rel: str, body: str) -> None:
    """Store `body` as the newest version of `rel`. Skips exact duplicates of
    the most recent snapshot; prunes beyond KEEP. Never raises — history is a
    safety net, not a reason a save may fail."""
    try:
        d = _dir_for(rel)
        d.mkdir(parents=True, exist_ok=True)
        existing = sorted(d.glob("*.md"))
        if existing and existing[-1].read_text(encoding="utf-8") == body:
            return
        (d / f"{int(time.time() * 1000)}.md").write_text(body, encoding="utf-8")
        for old in existing[: max(0, len(existing) + 1 - KEEP)]:
            old.unlink(missing_ok=True)
    except Exception:   # noqa: BLE001
        pass


def list_versions(rel: str) -> list[dict]:
    """Versions of a note, newest first: [{id, ts, size}]."""
    d = _dir_for(rel)
    if not d.is_dir():
        return []
    out = []
    for p in sorted(d.glob("*.md"), reverse=True):
        if _ID_RE.match(p.stem):
            out.append({"id": p.stem, "ts": int(p.stem) / 1000,
                        "size": p.stat().st_size})
    return out


def get_version(rel: str, version_id: str) -> str | None:
    """A specific version's body, or None. `version_id` is validated strictly —
    it becomes part of a filesystem path."""
    if not _ID_RE.match(version_id):
        return None
    p = _dir_for(rel) / f"{version_id}.md"
    return p.read_text(encoding="utf-8") if p.is_file() else None
