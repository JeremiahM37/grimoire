"""Note templates. Live in the vault's `templates/` dir (synced, plain .md, but
NOT part of the note graph). Applying one expands {{date}}/{{time}}/{{datetime}}/
{{title}} and creates a normal note."""
import time

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import index, vault
from ..vault import VaultError

router = APIRouter(prefix="/api")

TEMPLATE_DIR = "templates"


def _expand(text: str, title: str) -> str:
    now = time.localtime()
    for k, v in {
        "{{date}}": time.strftime("%Y-%m-%d", now),
        "{{time}}": time.strftime("%H:%M", now),
        "{{datetime}}": time.strftime("%Y-%m-%d %H:%M", now),
        "{{title}}": title or "",
    }.items():
        text = text.replace(k, v)
    return text


@router.get("/templates")
def list_templates():
    root = vault.vault_root() / TEMPLATE_DIR
    if not root.exists():
        return []
    out = []
    for p in sorted(root.rglob("*.md")):
        rel = vault.rel_of(p)
        try:
            name = vault.read(rel).get("title") or p.stem
        except Exception:
            name = p.stem
        out.append({"path": rel, "name": name})
    return out


class TemplateIn(BaseModel):
    name: str
    body: str


@router.post("/templates", status_code=201)
def create_template(t: TemplateIn):
    slug = vault.slugify(t.name)
    if not slug:
        raise HTTPException(400, "template needs a name")
    rel = f"{TEMPLATE_DIR}/{slug}.md"
    try:
        vault.write(rel, t.body, {"title": t.name})
    except VaultError as e:
        raise HTTPException(400, str(e))
    return {"path": rel, "name": t.name}


class ApplyIn(BaseModel):
    template: str            # a templates/… path
    title: str


@router.post("/templates/apply", status_code=201)
def apply_template(a: ApplyIn):
    trel = a.template if a.template.endswith(".md") else a.template + ".md"
    if not vault.is_reserved(trel) or TEMPLATE_DIR not in trel.split("/"):
        raise HTTPException(400, "not a template path")
    try:
        tpl = vault.read(trel)
    except VaultError:
        raise HTTPException(404, "no such template")
    if not vault.safe_path(trel).exists():
        raise HTTPException(404, "no such template")
    body = _expand(tpl["body"], a.title)
    rel = _unique(f"{vault.slugify(a.title)}.md")
    try:
        vault.write(rel, body, {"title": a.title})
    except VaultError as e:
        raise HTTPException(400, str(e))
    index.upsert(rel)
    return {"path": rel, "title": a.title}


def _unique(rel: str) -> str:
    if not vault.safe_path(rel).exists():
        return rel
    stem, i = rel[:-3], 2
    while vault.safe_path(f"{stem}-{i}.md").exists():
        i += 1
    return f"{stem}-{i}.md"
