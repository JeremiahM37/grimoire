"""Notes: CRUD (files ⇄ index), backlinks, rename."""
import json

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
    return [{"path": r["path"], "title": r["title"], "updated": r["updated"],
             "private": bool(r["private"])} for r in rows]


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
