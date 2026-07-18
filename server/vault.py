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
    if "/.grimoire/" in ("/" + str(target.relative_to(root)) + "/"):
        raise VaultError(".grimoire is reserved")
    return target


def safe_raw_path(rel: str) -> Path:
    """Like safe_path but preserves the real extension — for attachments/binary
    files (images, PDFs) that must NOT be coerced to .md. Same sandboxing."""
    rel = (rel or "").strip().lstrip("/")
    if not rel:
        raise VaultError("empty path")
    root = vault_root().resolve()
    target = (root / rel).resolve()
    if target != root and root not in target.parents:
        raise VaultError(f"path escapes vault: {rel!r}")
    if ".grimoire" in target.relative_to(root).parts:
        raise VaultError(".grimoire is reserved")
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
    from . import secrets  # lazy: avoid import cycle at module load
    fm, body = markdown.parse_frontmatter(text)
    stem = Path(rel).stem
    title = markdown.derive_title(fm, body, stem)
    encrypted = secrets.is_encrypted(body)
    return {
        "path": rel, "title": title, "frontmatter": fm, "body": body, "raw": text,
        # an encrypted body is opaque: no body tags/links, and always private
        "tags": _tag_union(fm, "") if encrypted else _tag_union(fm, body),
        "links": [] if encrypted else markdown.extract_links(body),
        "private": True if encrypted else bool(fm.get("private")),
        "encrypted": encrypted, "mtime": mtime, "hash": _hash(text),
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
    """Write a note. Merges/updates frontmatter (created/updated stamps).

    Round-trip fidelity: when the note already exists, its raw frontmatter block
    is PATCHED rather than regenerated — flat keys we manage are replaced, and
    anything our yamlish parser can't fully represent (nested maps, multiline
    strings, lists of objects — common in vaults that other markdown apps
    manage) passes through byte-for-byte. Editing a note through Grimoire must
    never degrade frontmatter written by another tool.
    """
    p = safe_path(rel)
    p.parent.mkdir(parents=True, exist_ok=True)
    fm = dict(frontmatter or {})
    now = time.strftime("%Y-%m-%dT%H:%M:%S")
    raw_inner = None
    if p.exists():
        existing_text = p.read_text(encoding="utf-8")
        existing_fm, _ = markdown.parse_frontmatter(existing_text)
        fm.setdefault("created", existing_fm.get("created", now))
        m = markdown.FRONTMATTER_RE.match(existing_text)
        if m:
            raw_inner = m.group(1)
    else:
        fm.setdefault("created", now)
    fm["updated"] = now
    if raw_inner is not None:
        fmblock = _patch_frontmatter(raw_inner, fm)
        text = fmblock + (body if body.startswith("\n") else "\n" + body).rstrip("\n") + "\n"
    else:
        text = _serialize(fm, body)
    _atomic_write(p, text)
    return note_from_text(rel_of(p), text, p.stat().st_mtime)


def _fm_entry(key: str, value) -> str:
    """One flat frontmatter entry in our canonical form."""
    if isinstance(value, list):
        return f"{key}: [{', '.join(str(x) for x in value)}]"
    if isinstance(value, bool):
        return f"{key}: {'true' if value else 'false'}"
    return f"{key}: {value}"


def _patch_frontmatter(raw_inner: str, fm: dict) -> str:
    """Merge managed keys into an existing raw frontmatter block.

    Block model: a top-level `key:` line owns its continuation lines (indented
    lines and list dashes) until the next top-level key. Three cases:

    * block contains structure we can't represent (an indented non-dash line:
      nested map, `|`/`>` multiline, object list) → preserved VERBATIM, and the
      caller's (necessarily degraded) value for that key is ignored;
    * flat block whose key is in `fm` → replaced with our canonical entry;
    * flat block whose key is absent from `fm` → removed (deleted in the UI).

    Keys new to `fm` are appended at the end. Unowned stray lines pass through.
    """
    import re as _re
    key_re = _re.compile(r"^([A-Za-z0-9_-]+):")
    lines = raw_inner.split("\n")
    # group into blocks: (key | None, [lines])
    blocks: list[tuple[str | None, list[str]]] = []
    for line in lines:
        m = key_re.match(line)
        if m:
            blocks.append((m.group(1), [line]))
        elif blocks and (line.startswith((" ", "\t")) or _re.match(r"^-\s", line)):
            blocks[-1][1].append(line)      # continuation of the current block
        else:
            blocks.append((None, [line]))   # stray line — preserved in place
    def is_nested(block_lines: list[str]) -> bool:
        return any(ln.strip() and ln.startswith((" ", "\t")) and not _re.match(r"^\s+-\s", ln)
                   for ln in block_lines[1:]) or block_lines[0].rstrip().endswith(("|", ">"))
    pending = dict(fm)
    out: list[str] = []
    for key, block_lines in blocks:
        if key is None:
            if block_lines != [""]:         # drop pure blanks, keep real strays
                out.extend(block_lines)
            continue
        if is_nested(block_lines):
            out.extend(block_lines)         # foreign structure: verbatim, protected
            pending.pop(key, None)
        elif key in pending:
            out.append(_fm_entry(key, pending.pop(key)))
        # else: flat key deleted by the editor — omit
    for key, value in pending.items():      # newly added keys
        out.append(_fm_entry(key, value))
    return "---\n" + "\n".join(out) + "\n---\n"


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


# dirs that hold files but are NOT part of the note graph (not indexed/searched)
RESERVED_DIRS = (".grimoire", "templates")


def is_reserved(rel: str) -> bool:
    """True for paths under a reserved dir (.grimoire internals, templates/)."""
    parts = rel.replace("\\", "/").split("/")
    return any(d in parts for d in RESERVED_DIRS)


def walk() -> list[Path]:
    """All indexable .md files in the vault (excludes reserved dirs)."""
    root = vault_root()
    if not root.exists():
        return []
    out = []
    for p in root.rglob("*.md"):
        if any(d in p.parts for d in RESERVED_DIRS):
            continue
        out.append(p)
    return out
