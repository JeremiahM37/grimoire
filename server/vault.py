"""Vault file operations — the filesystem side. Files are the source of truth.

Every path is sandboxed to the vault: traversal (`..`), absolute paths, and
symlink escapes are rejected. This is security-critical and covered by negative
tests.
"""
import os
import re
import time
from pathlib import Path

from . import config, markdown


class VaultError(Exception):
    pass


def vault_root() -> Path:
    return config.VAULT


def safe_path(rel: str) -> Path:
    """Resolve a vault-relative path, rejecting anything that escapes the vault."""
    rel = (rel or "").strip().lstrip("/")
    if not rel:
        raise VaultError("empty path")
    if not rel.endswith(".md"):
        rel += ".md"
    root = vault_root().resolve()
    target = (root / rel).resolve()
    if target != root and root not in target.parents:
        raise VaultError(f"path escapes vault: {rel!r}")
    if "/.mnemo/" in ("/" + str(target.relative_to(root)) + "/"):
        raise VaultError(".mnemo is reserved")
    return target


def rel_of(path: Path) -> str:
    return str(path.resolve().relative_to(vault_root().resolve()))


def slugify(title: str) -> str:
    s = re.sub(r"[^\w\s-]", "", title).strip().lower()
    s = re.sub(r"[\s_-]+", "-", s)
    return s or "untitled"


def read(rel: str) -> dict:
    p = safe_path(rel)
    if not p.exists():
        raise VaultError(f"no such note: {rel}")
    text = p.read_text(encoding="utf-8")
    return note_from_text(rel_of(p), text, p.stat().st_mtime)


def note_from_text(rel: str, text: str, mtime: float) -> dict:
    fm, body = markdown.parse_frontmatter(text)
    stem = Path(rel).stem
    title = markdown.derive_title(fm, body, stem)
    return {
        "path": rel, "title": title, "frontmatter": fm, "body": body, "raw": text,
        "tags": _tag_union(fm, body), "links": markdown.extract_links(body),
        "private": bool(fm.get("private")), "mtime": mtime,
        "hash": _hash(text),
    }


def _tag_union(fm: dict, body: str) -> list[str]:
    tags = list(markdown.extract_tags(body))
    fm_tags = fm.get("tags")
    if isinstance(fm_tags, list):
        for t in fm_tags:
            if str(t) not in tags:
                tags.append(str(t))
    elif isinstance(fm_tags, str) and fm_tags:
        if fm_tags not in tags:
            tags.append(fm_tags)
    return tags


def _hash(text: str) -> str:
    import hashlib
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:16]


def write(rel: str, body: str, frontmatter: dict | None = None) -> dict:
    """Write a note. Merges/updates frontmatter (created/updated stamps)."""
    p = safe_path(rel)
    p.parent.mkdir(parents=True, exist_ok=True)
    fm = dict(frontmatter or {})
    now = time.strftime("%Y-%m-%dT%H:%M:%S")
    if p.exists():
        existing_fm, _ = markdown.parse_frontmatter(p.read_text(encoding="utf-8"))
        fm.setdefault("created", existing_fm.get("created", now))
    else:
        fm.setdefault("created", now)
    fm["updated"] = now
    text = _serialize(fm, body)
    _atomic_write(p, text)
    return note_from_text(rel_of(p), text, p.stat().st_mtime)


def _serialize(fm: dict, body: str) -> str:
    if not fm:
        return body if body.endswith("\n") else body + "\n"
    lines = ["---"]
    for k, v in fm.items():
        if isinstance(v, list):
            lines.append(f"{k}: [{', '.join(str(x) for x in v)}]")
        elif isinstance(v, bool):
            lines.append(f"{k}: {'true' if v else 'false'}")
        else:
            lines.append(f"{k}: {v}")
    lines.append("---")
    fmblock = "\n".join(lines) + "\n"
    return fmblock + (body if body.startswith("\n") else "\n" + body).rstrip("\n") + "\n"


def _atomic_write(p: Path, text: str) -> None:
    tmp = p.with_suffix(p.suffix + ".tmp")
    tmp.write_text(text, encoding="utf-8")
    os.replace(tmp, p)


def delete(rel: str) -> None:
    p = safe_path(rel)
    if p.exists():
        p.unlink()


def rename(old_rel: str, new_rel: str) -> str:
    src, dst = safe_path(old_rel), safe_path(new_rel)
    if not src.exists():
        raise VaultError(f"no such note: {old_rel}")
    if dst.exists():
        raise VaultError(f"target exists: {new_rel}")
    dst.parent.mkdir(parents=True, exist_ok=True)
    os.replace(src, dst)
    return rel_of(dst)


def walk() -> list[Path]:
    """All .md files in the vault, excluding the reserved .mnemo dir."""
    root = vault_root()
    if not root.exists():
        return []
    out = []
    for p in root.rglob("*.md"):
        if ".mnemo" in p.parts:
            continue
        out.append(p)
    return out
