"""Soft-delete. Deleting a note moves it to `.grimoire/trash/` (internal, unsynced,
never indexed) with a manifest recording its original path — so an accidental
delete is recoverable. Restore returns it to the vault (auto-suffixed if the
original path is taken again)."""
import json
import shutil
import time

from . import config, vault


def _dir():
    d = config.grimoire_dir() / "trash"
    d.mkdir(parents=True, exist_ok=True)
    return d


def _manifest() -> dict:
    p = _dir() / "manifest.json"
    if not p.exists():
        return {}
    try:
        return json.loads(p.read_text(encoding="utf-8"))
    except Exception:
        return {}


def _save_manifest(m: dict) -> None:
    (_dir() / "manifest.json").write_text(json.dumps(m, indent=2), encoding="utf-8")


def _new_id() -> str:
    # monotonic-ish id without Math.random/Date ambiguity: timestamp + counter
    base = time.strftime("%Y%m%d-%H%M%S")
    m = _manifest()
    i, tid = 0, base
    while tid in m:
        i += 1
        tid = f"{base}-{i}"
    return tid


def trash(rel: str, title: str) -> str:
    """Move a note file into the trash. Returns the trash id."""
    src = vault.safe_path(rel)
    if not src.exists():
        raise vault.VaultError(f"no such note: {rel}")
    tid = _new_id()
    shutil.move(str(src), str(_dir() / f"{tid}.md"))
    m = _manifest()
    m[tid] = {"original": rel, "title": title, "deleted_at": time.strftime("%Y-%m-%dT%H:%M:%S")}
    _save_manifest(m)
    return tid


def list_trash() -> list[dict]:
    return [{"id": k, **v} for k, v in sorted(_manifest().items(), reverse=True)]


def restore(tid: str) -> str:
    """Move a trashed note back into the vault. Returns its (possibly new) path."""
    m = _manifest()
    if tid not in m:
        raise vault.VaultError("no such trashed note")
    entry = m[tid]
    dest_rel = _unique(entry["original"])
    dest = vault.safe_path(dest_rel)
    dest.parent.mkdir(parents=True, exist_ok=True)
    shutil.move(str(_dir() / f"{tid}.md"), str(dest))
    del m[tid]
    _save_manifest(m)
    return dest_rel


def purge(tid: str) -> None:
    """Permanently delete one trashed note."""
    m = _manifest()
    if tid in m:
        f = _dir() / f"{tid}.md"
        if f.exists():
            f.unlink()
        del m[tid]
        _save_manifest(m)


def _unique(rel: str) -> str:
    if not vault.safe_path(rel).exists():
        return rel
    stem = rel[:-3] if rel.endswith(".md") else rel
    i = 2
    while vault.safe_path(f"{stem}-{i}.md").exists():
        i += 1
    return f"{stem}-{i}.md"
