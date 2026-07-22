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

from .. import ai, db, history, index, vault
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


class ConsolidateIn(BaseModel):
    path: str = ""          # a specific memory note…
    topic: str = ""         # …or its topic; empty for both = every memory note


@router.post("/memory/consolidate")
def consolidate(c: ConsolidateIn):
    """Compact agent memory so recall stays sharp as it grows: merge redundant
    entries, supersede stale ones. Each rewrite is snapshotted first, so the
    human reviews and rolls back like any note — memory stays auditable."""
    if c.path.strip():
        rels = [c.path.strip()]
    elif c.topic.strip():
        rels = [_memory_rel(c.topic)]
    else:
        rels = [r["path"] for r in db.query(
            "SELECT path FROM notes WHERE path LIKE ? ORDER BY updated DESC",
            (f"{MEMORY_DIR}/%",))]
    out = []
    for rel in rels:
        try:
            note = vault.read(rel)
        except VaultError:
            continue
        if note.get("encrypted"):
            continue
        before = note["body"]
        after = ai.consolidate_memory(before)
        if after and after.strip() != before.strip():
            history.snapshot(rel, before)          # rollback-able
            vault.write(rel, after, note["frontmatter"])
            index.upsert(rel)
            out.append({"path": rel,
                        "before_entries": before.count("- **"),
                        "after_entries": after.count("- **")})
    return {"consolidated": out, "notes_changed": len(out)}


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
        if not rows:
            # exact terms missed — fall back to semantic retrieval over the
            # memory namespace so paraphrased recalls still land
            hits = index.retrieve(q, k=limit * 3)
            paths = []
            for h in hits:
                if h["path"].startswith(f"{MEMORY_DIR}/") and h["path"] not in paths:
                    paths.append(h["path"])
            rows = [db.one("SELECT path, title, body, updated FROM notes WHERE path=?",
                           (pth,)) for pth in paths[:limit]]
    else:
        rows = db.query(
            "SELECT path, title, body, updated FROM notes WHERE path LIKE ? "
            "ORDER BY updated DESC LIMIT ?", (f"{MEMORY_DIR}/%", limit))
    return [{"path": r["path"], "title": r["title"], "updated": r["updated"],
             "body": r["body"]} for r in rows]

@router.get("/briefing")
def briefing(memories: int = 5):
    """The "read this first" pack for an agent joining a session: pinned notes,
    onboarding-tagged notes, and the most recent agent memories — one call.

    Exists because retrieval only surfaces what an agent thinks to ask for;
    standing context (conventions, environment rules, active decisions) must
    arrive unprompted or it gets skipped. (Benchmark finding: an agent missed a
    documented env rule because it lived in an onboarding memory it never
    queried.) Private notes stay excluded — this can feed unauthenticated
    automation."""
    memories = max(1, min(memories, 20))

    def rows_to_notes(rows):
        return [{"path": r["path"], "title": r["title"], "body": r["body"]}
                for r in rows]

    pinned = db.query(
        "SELECT path, title, body FROM notes WHERE private=0 "
        "AND frontmatter_json LIKE '%\"pinned\": true%' ORDER BY updated DESC LIMIT 10")
    onboarding = db.query(
        "SELECT n.path, n.title, n.body FROM notes n JOIN tags t ON t.note=n.path "
        "WHERE t.tag='onboarding' AND n.private=0 ORDER BY n.updated DESC LIMIT 10")
    recent = db.query(
        "SELECT path, title, body FROM notes WHERE path LIKE ? AND private=0 "
        "ORDER BY updated DESC LIMIT ?", (f"{MEMORY_DIR}/%", memories))
    seen: set[str] = set()
    out = {"pinned": [], "onboarding": [], "recent_memories": []}
    for key, rows in (("pinned", pinned), ("onboarding", onboarding),
                      ("recent_memories", recent)):
        for n in rows_to_notes(rows):
            if n["path"] not in seen:
                seen.add(n["path"])
                out[key].append(n)
    return out

