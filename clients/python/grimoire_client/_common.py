"""Shared by the framework adapters. Private: import from the adapter modules.

Two rules every adapter write follows, collected here so they cannot drift:

* the write names the agent (``agent=``), so recall can say who recorded it
  and an agent's own facts can be found again;
* the write is marked agent-authored (``category=AGENT_AUTHORED``). The server
  already treats a write without ``by=human`` as an agent assertion, so this is
  the explicit marker a reader of the note sees, not a change in authority.

Recall results go through :mod:`grimoire_client.fencing` so untrusted text is
fenced the same way on every path into a prompt.
"""

from __future__ import annotations

from collections.abc import Iterable
from typing import Any

from . import GrimoireError
from .fencing import PREAMBLE, fence, is_fenced, text_of

__all__ = [
    "AGENT_AUTHORED",
    "agent_write",
    "format_memories",
    "recall_facts",
    "remember_fact",
    "search_notes_text",
]

AGENT_AUTHORED = "agent-authored"


def agent_write(agent: str, **extra: Any) -> dict[str, Any]:
    """Keyword arguments for one adapter write: the agent name and the marker."""
    kwargs: dict[str, Any] = {"agent": agent, "category": AGENT_AUTHORED}
    kwargs.update({k: v for k, v in extra.items() if v})
    return kwargs


def format_memories(memories: Iterable[Any]) -> str:
    """A numbered list of recalled facts, with the fence preamble when needed."""
    lines = []
    for i, memory in enumerate(memories, 1):
        path = memory.path if hasattr(memory, "path") else memory.get("path", "")
        lines.append(f"{i}. {text_of(memory, n=i)} [{path}]")
    body = "\n".join(lines)
    if body and is_fenced(body):
        return f"{PREAMBLE}\n\n{body}"
    return body


def remember_fact(client: Any, agent: str, text: str, topic: str = "") -> str:
    """Record one fact; the result as text, because a tool answers in text.

    Errors come back as text rather than raising, so a server that is down
    degrades one step of an agent's run instead of failing the run.
    """
    try:
        result = client.add(text, topic=topic, **agent_write(agent))
    except GrimoireError as exc:
        return f"Not saved: {exc.message}"
    return f"{result.op}: {result.why}" if result.why else result.op


def recall_facts(client: Any, query: str, limit: int = 5) -> str:
    """Currently believed facts about ``query``, formatted for a model."""
    try:
        memories = client.search(query, limit=limit)
    except GrimoireError as exc:
        return f"Memory unavailable: {exc.message}"
    return format_memories(memories) or "No matching memories."


def search_notes_text(client: Any, query: str, limit: int = 5) -> str:
    """Full-text note hits as ``- path: excerpt`` lines."""
    try:
        hits = client.search_notes(query, limit=limit)
    except GrimoireError as exc:
        return f"Notes unavailable: {exc.message}"
    lines = []
    for hit in hits:
        excerpt = str(hit.get("snippet") or hit.get("excerpt") or "")[:200]
        if hit.get("trust") == "untrusted":
            excerpt = fence(excerpt, origin=hit.get("origin", ""))
        lines.append(f"- {hit.get('path', '')}: {excerpt}")
    body = "\n".join(lines)
    if body and is_fenced(body):
        return f"{PREAMBLE}\n\n{body}"
    return body or "No matching notes."
