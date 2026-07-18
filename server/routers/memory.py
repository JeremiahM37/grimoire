"""Agent memory — the substrate's agent-writable namespace.

Agents persist what they learn under `memory/` in the vault, as plain markdown
notes with provenance frontmatter (`memory: true`, `agent`, `task`). Because
memories are ordinary notes, everything the human console offers applies to
them: read, edit, diff, roll back (version history), link, search. That
human-auditable loop — *your agent's memory is a note you can open* — is the
point of the design.

Write model: a memory targets a topic. The first memory on a topic creates
`memory/<topic>.md`; later memories on the same topic append attributed,
timestamped bullets. Topicless memories accrete on a per-day note. Appends
snapshot the previous version first (history covers agent writes too).
"""
import re
import time

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

from .. import db, history, index, vault
from ..vault import VaultError

router = APIRouter(prefix="/api")

MEMORY_DIR = "memory"
_AGENT_RE = re.compile(r"^[\w][\w .:/-]{0,60}$")


class MemoryIn(BaseModel):
    text: str = Field(min_length=1, max_length=20_000)
    topic: str = ""                  # groups related memories into one note
    agent: str = "agent"             # who is remembering (shown as provenance)
    task: str = ""                   # optional origin (task id, session, url…)


def _memory_rel(topic: str) -> str:
    slug = vault.slugify(topic) if topic else time.strftime("%Y-%m-%d")
    return f"{MEMORY_DIR}/{slug}.md"


@router.post("/memory", status_code=201)
def remember(m: MemoryIn):
    """Append one memory. Creates the topic note on first use."""
    agent = m.agent.strip() or "agent"
    if not _AGENT_RE.match(agent):
        raise HTTPException(400, "invalid agent name")
    rel = _memory_rel(m.topic)
    stamp = time.strftime("%Y-%m-%d %H:%M")
    attribution = f"{stamp} · {agent}" + (f" · {m.task.strip()}" if m.task.strip() else "")
    entry = f"- **{attribution}** — {m.text.strip()}\n"
    try:
        existing = vault.safe_path(rel).exists()
        if existing:
            note = vault.read(rel)
            history.snapshot(rel, note["body"])          # agent writes are rollbackable
            fm = dict(note["frontmatter"])
            fm["agent"] = agent                          # most recent writer…
            if m.task.strip():
                fm["task"] = m.task.strip()              # …and their task, together
            body = note["body"].rstrip("\n") + "\n" + entry
        else:
            title = m.topic.strip() or time.strftime("%Y-%m-%d")
            fm = {"title": f"Memory: {title}", "memory": True, "agent": agent}
            if m.task.strip():
                fm["task"] = m.task.strip()
            body = f"# Memory: {title}\n\n{entry}"
        vault.write(rel, body, fm)
    except VaultError as e:
        raise HTTPException(400, str(e)) from None
    index.upsert(rel)
    return {"path": rel, "created": not existing, "entry": entry.strip()}


@router.get("/memory")
def recall(q: str = "", limit: int = 20):
    """Recall memories. With a query: FTS over the memory namespace (quoted —
    user text can't inject MATCH syntax). Without: the most recently touched
    memory notes. Returns full bodies — memories exist to be re-read."""
    limit = max(1, min(limit, 100))
    if q.strip():
        # each term quoted individually → implicit AND, terms may be anywhere in
        # the note (a single quoted phrase was too strict for recall — it missed
        # any query whose words weren't adjacent in the memory). Quoting keeps
        # user text from ever acting as MATCH syntax.
        match = " ".join('"' + t.replace('"', '""') + '"' for t in q.split())
        rows = db.query(
            "SELECT n.path, n.title, n.body, n.updated FROM notes n "
            "WHERE n.path LIKE ? AND n.path IN (SELECT path FROM fts WHERE fts MATCH ?) "
            "ORDER BY n.updated DESC LIMIT ?",
            (f"{MEMORY_DIR}/%", match, limit))
    else:
        rows = db.query(
            "SELECT path, title, body, updated FROM notes WHERE path LIKE ? "
            "ORDER BY updated DESC LIMIT ?", (f"{MEMORY_DIR}/%", limit))
    return [{"path": r["path"], "title": r["title"], "updated": r["updated"],
             "body": r["body"]} for r in rows]
