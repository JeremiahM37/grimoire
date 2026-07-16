"""Daily notes + capture inbox."""
import time

from fastapi import APIRouter
from pydantic import BaseModel

from .. import config, db, index, vault

router = APIRouter(prefix="/api")


def _daily_rel(date: str | None = None) -> str:
    d = date or time.strftime("%Y-%m-%d")
    return f"{config.DAILY_DIR}/{d}.md"


@router.get("/daily")
def daily(date: str | None = None):
    """Today's (or a given date's) note — created from a template if absent."""
    d = date or time.strftime("%Y-%m-%d")
    rel = _daily_rel(d)
    if not vault.safe_path(rel).exists():
        vault.write(rel, f"# {d}\n\n", {"title": d, "tags": ["daily"]})
        index.upsert(rel)
    row = db.one("SELECT * FROM notes WHERE path=?", (rel,))
    return {"path": rel, "title": row["title"], "body": row["body"]}


class CaptureIn(BaseModel):
    text: str
    title: str | None = None
    url: str | None = None
    source: str = "capture"


@router.post("/capture", status_code=201)
def capture(c: CaptureIn):
    """Inbound note from outside (browser clip, CLI, share). Lands in the inbox,
    and a link is appended to today's daily note so nothing gets lost."""
    stamp = time.strftime("%Y%m%d-%H%M%S")
    title = c.title or f"capture {stamp}"
    rel = f"{config.INBOX_DIR}/{stamp}-{vault.slugify(title)}.md"
    body = c.text
    if c.url:
        body = f"> source: {c.url}\n\n{body}"
    vault.write(rel, body, {"title": title, "tags": ["capture"],
                            "source": c.source, **({"url": c.url} if c.url else {})})
    index.upsert(rel)
    # thread into today's daily note
    drel = _daily_rel()
    day = daily()  # ensure it exists
    note = vault.read(drel)
    stem = rel.rsplit("/", 1)[-1][:-3]
    vault.write(drel, note["body"].rstrip() + f"\n- [[{stem}|{title}]]\n",
                note["frontmatter"])
    index.upsert(drel)
    return {"path": rel, "title": title}
