"""Notes: CRUD (files ⇄ index), backlinks, rename, unlinked mentions."""
import json
import re

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import db, index, secrets, trash, vault
from ..vault import VaultError

router = APIRouter(prefix="/api")


def _view(row: dict) -> dict:
    row = dict(row)
    row["frontmatter"] = json.loads(row.pop("frontmatter_json", "{}") or "{}")
    row["private"] = bool(row.get("private"))
    row["tags"] = [t["tag"] for t in
                   db.query("SELECT tag FROM tags WHERE note=?", (row["path"],))]
    # transparently present encrypted notes: decrypt when the vault is unlocked,
    # otherwise blank the body and flag it locked (never leak ciphertext to the UI)
    body = row.get("body", "")
    row["encrypted"] = secrets.is_encrypted(body)
    if row["encrypted"]:
        if secrets.is_unlocked():
            try:
                row["body"] = secrets.unseal_text(body)
                row["locked"] = False
            except Exception:
                row["body"] = ""; row["locked"] = True
        else:
            row["body"] = ""; row["locked"] = True
    else:
        row["locked"] = False
    return row


@router.get("/notes")
def list_notes(tag: str | None = None, limit: int = 500):
    if tag:
        rows = db.query(
            "SELECT n.* FROM notes n JOIN tags t ON t.note=n.path WHERE t.tag=? "
            "ORDER BY n.updated DESC LIMIT ?", (tag, limit))
    else:
        rows = db.query("SELECT * FROM notes ORDER BY updated DESC LIMIT ?", (limit,))
    items = [{"path": r["path"], "title": r["title"], "updated": r["updated"],
              "private": bool(r["private"]), "pinned": _pinned(r["frontmatter_json"])}
             for r in rows]
    # pinned notes float to the top (stable within each group)
    items.sort(key=lambda x: not x["pinned"])
    return items


def _pinned(frontmatter_json: str) -> bool:
    try:
        return bool(json.loads(frontmatter_json or "{}").get("pinned"))
    except Exception:
        return False


@router.post("/notes/{path:path}/pin")
def toggle_pin(path: str):
    """Toggle a note's pinned flag (frontmatter). Returns the new state."""
    rel = _norm(path)
    try:
        note = vault.read(rel)
    except VaultError:
        raise HTTPException(404, "no such note")
    fm = dict(note["frontmatter"])
    pinned = not fm.get("pinned")
    if pinned:
        fm["pinned"] = True
    else:
        fm.pop("pinned", None)
    # write the body back verbatim — for an encrypted note this is the ciphertext,
    # so pinning needs no unlock (only frontmatter changes)
    vault.write(rel, note["body"], fm)
    index.upsert(rel)
    return {"path": rel, "pinned": pinned}


class NoteIn(BaseModel):
    path: str | None = None
    title: str | None = None
    body: str = ""
    frontmatter: dict = {}
    tags: list[str] = []


@router.post("/notes", status_code=201)
def create_note(n: NoteIn):
    if not n.path and not n.title:
        raise HTTPException(400, "path or title required")
    try:
        if n.path:
            # an explicit path collision is an error the caller should see
            rel = n.path if n.path.endswith(".md") else n.path + ".md"
            if vault.safe_path(rel).exists():
                raise HTTPException(409, "note already exists")
        else:
            # a title-derived slug collision auto-suffixes (Meeting → meeting-2)
            rel = _unique_path(f"{vault.slugify(n.title)}.md")
        fm = dict(n.frontmatter)
        if n.title:
            fm.setdefault("title", n.title)
        if n.tags:
            fm["tags"] = n.tags
        vault.write(rel, n.body, fm)
    except VaultError as e:
        raise HTTPException(400, str(e))
    note = index.upsert(rel)
    return _view(db.one("SELECT * FROM notes WHERE path=?", (note["path"],)))


def _names_for(rel: str, title: str) -> list[str]:
    row = db.one("SELECT frontmatter_json FROM notes WHERE path=?", (rel,))
    names = [title]
    if row:
        names += index._aliases(row["frontmatter_json"])
    return [n for n in dict.fromkeys(names) if len(n) >= 3]


def _mention_re(name: str) -> re.Pattern:
    # a whole-word match NOT already inside a [[wiki-link]]
    return re.compile(r"(?<![\w\[])" + re.escape(name) + r"(?![\w\]])", re.I)


# NOTE: these GET routes must precede get_note — its greedy /notes/{path:path}
# would otherwise swallow them.
@router.get("/notes/random")
def random_note():
    row = db.one("SELECT path FROM notes ORDER BY RANDOM() LIMIT 1")
    if not row:
        raise HTTPException(404, "no notes yet")
    return {"path": row["path"]}


@router.get("/notes/{path:path}/unlinked")
def unlinked_mentions(path: str):
    """Notes that mention this note's title/aliases as plain text but don't link it."""
    rel = _norm(path)
    target = db.one("SELECT title FROM notes WHERE path=?", (rel,))
    if not target:
        raise HTTPException(404, "no such note")
    names = _names_for(rel, target["title"])
    if not names:
        return []
    linked = {l["src"] for l in db.query("SELECT src FROM links WHERE dst=?", (rel,))}
    pats = [(n, _mention_re(n)) for n in names]
    out = []
    for r in db.query("SELECT path, title, body FROM notes WHERE path != ?", (rel,)):
        if r["path"] in linked or secrets.is_encrypted(r["body"] or ""):
            continue
        for name, pat in pats:
            m = pat.search(r["body"] or "")
            if m:
                start = r["body"].rfind("\n", 0, m.start()) + 1
                end = r["body"].find("\n", m.end())
                snippet = r["body"][start:end if end != -1 else None].strip()
                out.append({"path": r["path"], "title": r["title"], "name": name,
                            "context": snippet[:160]})
                break
    return out


@router.get("/notes/{path:path}")
def get_note(path: str):
    row = db.one("SELECT * FROM notes WHERE path=?", (_norm(path),))
    if not row:
        raise HTTPException(404, "no such note")
    out = _view(row)
    out["backlinks"] = index.backlinks(row["path"])
    out["links"] = db.query(
        "SELECT target, dst, alias, resolved FROM links WHERE src=?", (row["path"],))
    return out


class NoteUpdate(BaseModel):
    body: str
    frontmatter: dict | None = None


@router.put("/notes/{path:path}")
def update_note(path: str, u: NoteUpdate):
    rel = _norm(path)
    try:
        existing = vault.read(rel) if vault.safe_path(rel).exists() else None
        fm = u.frontmatter if u.frontmatter is not None else (existing["frontmatter"] if existing else {})
        body = u.body
        if existing and existing.get("encrypted"):
            # the editor holds plaintext; re-seal it (requires an unlocked vault)
            if not secrets.is_unlocked():
                raise HTTPException(423, "vault locked — unlock to edit this encrypted note")
            body = secrets.seal_text(u.body)
            fm = {**fm, "encrypted": True, "private": True}
        vault.write(rel, body, fm)
    except VaultError as e:
        raise HTTPException(400, str(e))
    index.upsert(rel)
    return _view(db.one("SELECT * FROM notes WHERE path=?", (rel,)))


@router.post("/notes/{path:path}/encrypt")
def encrypt_note(path: str):
    """Seal a note's body at rest with the vault key. Requires an unlocked vault."""
    rel = _norm(path)
    if not secrets.is_unlocked():
        raise HTTPException(423, "unlock the secret vault first")
    try:
        note = vault.read(rel)
    except VaultError:
        raise HTTPException(404, "no such note")
    if not note.get("encrypted"):
        fm = {**note["frontmatter"], "encrypted": True, "private": True}
        vault.write(rel, secrets.seal_text(note["body"]), fm)
        index.upsert(rel)
    return _view(db.one("SELECT * FROM notes WHERE path=?", (rel,)))


@router.post("/notes/{path:path}/decrypt")
def decrypt_note(path: str):
    """Remove at-rest encryption, restoring plain markdown. Requires unlock."""
    rel = _norm(path)
    if not secrets.is_unlocked():
        raise HTTPException(423, "unlock the secret vault first")
    try:
        note = vault.read(rel)
    except VaultError:
        raise HTTPException(404, "no such note")
    if note.get("encrypted"):
        try:
            plain = secrets.unseal_text(note["body"])
        except ValueError:
            raise HTTPException(400, "cannot decrypt — wrong vault key")
        fm = {k: v for k, v in note["frontmatter"].items() if k != "encrypted"}
        vault.write(rel, plain, fm)
        index.upsert(rel)
    return _view(db.one("SELECT * FROM notes WHERE path=?", (rel,)))


@router.delete("/notes/{path:path}")
def delete_note(path: str):
    """Soft-delete: move the note to trash (recoverable) and drop it from the
    index. Returns the trash id so the UI can offer Undo."""
    rel = _norm(path)
    row = db.one("SELECT title FROM notes WHERE path=?", (rel,))
    title = row["title"] if row else rel
    try:
        tid = trash.trash(rel, title)
    except VaultError as e:
        raise HTTPException(404, str(e))
    index.remove(rel)
    return {"trashed": tid, "path": rel}


class LinkMentionIn(BaseModel):
    source: str          # the note that mentions this one
    name: str            # the mentioned string to wrap


@router.post("/notes/{path:path}/link")
def link_mention(path: str, m: LinkMentionIn):
    """Wrap the first unlinked mention of `name` in `source` with a wiki-link to
    this note (so an unlinked mention becomes a real link)."""
    rel = _norm(path)
    target = db.one("SELECT title FROM notes WHERE path=?", (rel,))
    if not target:
        raise HTTPException(404, "no such note")
    src = _norm(m.source)
    try:
        note = vault.read(src)
    except VaultError:
        raise HTTPException(404, "no such source note")
    if note.get("encrypted"):
        raise HTTPException(400, "cannot edit an encrypted note")
    link = f"[[{m.name}]]" if m.name.lower() == target["title"].lower() \
        else f"[[{target['title']}|{m.name}]]"
    new_body, n = _mention_re(m.name).subn(link, note["body"], count=1)
    if not n:
        raise HTTPException(404, "mention not found")
    vault.write(src, new_body, note["frontmatter"])
    index.upsert(src)
    return {"linked": src, "count": n}


@router.post("/notes/{path:path}/duplicate", status_code=201)
def duplicate_note(path: str):
    """Copy a note (title + ' (copy)', fresh timestamps). Encrypted notes copy
    their ciphertext verbatim — the duplicate stays sealed."""
    rel = _norm(path)
    try:
        note = vault.read(rel)
    except VaultError:
        raise HTTPException(404, "no such note")
    title = (note["frontmatter"].get("title") or note["title"]) + " (copy)"
    new_rel = _unique_path(f"{vault.slugify(title)}.md")
    fm = {k: v for k, v in note["frontmatter"].items() if k not in ("created", "updated")}
    fm["title"] = title
    vault.write(new_rel, note["body"], fm)
    index.upsert(new_rel)
    return _view(db.one("SELECT * FROM notes WHERE path=?", (new_rel,)))


@router.get("/trash")
def list_trash():
    return trash.list_trash()


@router.post("/trash/{tid}/restore")
def restore_trashed(tid: str):
    try:
        rel = trash.restore(tid)
    except VaultError as e:
        raise HTTPException(404, str(e))
    index.upsert(rel)
    return _view(db.one("SELECT * FROM notes WHERE path=?", (rel,)))


@router.delete("/trash/{tid}", status_code=204)
def purge_trashed(tid: str):
    trash.purge(tid)


class RenameIn(BaseModel):
    to: str


@router.post("/notes/{path:path}/rename")
def rename_note(path: str, r: RenameIn):
    try:
        new_rel = vault.rename(_norm(path), r.to)
    except VaultError as e:
        raise HTTPException(400, str(e))
    index.remove(_norm(path))
    index.upsert(new_rel)
    return {"path": new_rel}


def _unique_path(rel: str) -> str:
    """Return `rel`, or the next free `stem-N.md` if it already exists."""
    if not vault.safe_path(rel).exists():
        return rel
    stem = rel[:-3]
    i = 2
    while vault.safe_path(f"{stem}-{i}.md").exists():
        i += 1
    return f"{stem}-{i}.md"


def _norm(path: str) -> str:
    path = path.strip("/")
    return path if path.endswith(".md") else path + ".md"
