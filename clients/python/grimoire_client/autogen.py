"""AutoGen memory adapter: a ``Memory`` backed by Grimoire.

    from autogen_agentchat.agents import AssistantAgent
    from grimoire_client import Grimoire
    from grimoire_client.autogen import GrimoireMemory

    client = Grimoire("http://localhost:9111")
    memory = GrimoireMemory(client, agent="support-bot")
    assistant = AssistantAgent("support", model_client=model, memory=[memory])

The four operations AutoGen's ``Memory`` protocol names map onto the client:

* :meth:`GrimoireMemory.add` records a fact (``remember``, reconciled and
  agent-authored);
* :meth:`GrimoireMemory.query` recalls currently believed facts (``recall``);
* :meth:`GrimoireMemory.update_context` recalls for the latest user message and
  adds the facts to the model context as one system message;
* :meth:`GrimoireMemory.clear` and :meth:`GrimoireMemory.close` do not touch
  the server (see their docstrings).

``autogen_core`` is optional. When it is importable the content, result and
system-message types are its real classes; when it is not, the same attributes
come back on small stand-ins, and the module still imports. Nothing here needs
the rest of AutoGen. The AutoGen methods are ``async``; the client is blocking
``urllib``, so each call runs in a worker thread.

``GrimoireMemory`` duck-types AutoGen's protocol rather than subclassing its
base class, for the same reason the LangChain retriever does not subclass
``BaseRetriever``: the base brings component machinery that would make the
optional import fragile.

Errors: :meth:`add` and :meth:`query` raise :class:`~grimoire_client.GrimoireError`,
because the caller asked for that write or read. :meth:`update_context` adds
nothing when the server cannot be reached, because injected recall is a
suggestion to the model, the same rule as :func:`grimoire_client.context.context_for`.
Recall passes through the client-side fence (see :mod:`grimoire_client.fencing`).
"""

from __future__ import annotations

import asyncio
import json
from dataclasses import dataclass, field
from typing import Any

from . import Grimoire, GrimoireError, Memory
from ._common import agent_write, format_memories
from .fencing import text_of

try:  # pragma: no cover - exercised only where autogen_core is installed
    from autogen_core.memory import MemoryContent as _MemoryContent
    from autogen_core.memory import MemoryQueryResult as _MemoryQueryResult
    from autogen_core.memory import UpdateContextResult as _UpdateContextResult
    from autogen_core.models import SystemMessage as _SystemMessage
except ImportError:
    _MemoryContent = _MemoryQueryResult = _UpdateContextResult = _SystemMessage = None

__all__ = ["GrimoireMemory", "MemoryContent", "MemoryQueryResult", "UpdateContextResult"]

DEFAULT_AGENT = "autogen"
TEXT_MIME = "text/plain"


@dataclass
class MemoryContent:
    """Stand-in for ``autogen_core.memory.MemoryContent`` when it is absent."""

    content: Any
    mime_type: str = TEXT_MIME
    metadata: dict[str, Any] | None = None


@dataclass
class MemoryQueryResult:
    """Stand-in for ``autogen_core.memory.MemoryQueryResult`` when it is absent."""

    results: list[MemoryContent] = field(default_factory=list)


@dataclass
class UpdateContextResult:
    """Stand-in for ``autogen_core.memory.UpdateContextResult`` when it is absent."""

    memories: MemoryQueryResult = field(default_factory=MemoryQueryResult)


@dataclass
class SystemMessage:
    """Stand-in for ``autogen_core.models.SystemMessage`` when it is absent."""

    content: str


_CONTENT = _MemoryContent or MemoryContent
_QUERY_RESULT = _MemoryQueryResult or MemoryQueryResult
_UPDATE_RESULT = _UpdateContextResult or UpdateContextResult
_SYSTEM = _SystemMessage or SystemMessage


def _text_of_content(content: Any) -> str:
    """The text of an AutoGen ``MemoryContent`` payload, whatever its type."""
    raw = getattr(content, "content", content)
    if isinstance(raw, str):
        return raw
    if isinstance(raw, bytes):
        return raw.decode("utf-8", errors="replace")
    if isinstance(raw, dict):
        return json.dumps(raw, default=str, sort_keys=True)
    return str(raw)


def _latest_user_text(messages: list[Any]) -> str:
    """The text of the most recent user message, or ``""`` when there is none."""
    for message in reversed(messages):
        if isinstance(message, dict):
            kind, content = message.get("type") or message.get("role") or "", message.get("content")
        else:
            kind = getattr(message, "type", "") or getattr(message, "role", "") or ""
            content = getattr(message, "content", "")
        # AutoGen's UserMessage has type "UserMessage"; OpenAI-style dicts say "user".
        if str(kind).lower() not in ("usermessage", "user"):
            continue
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            return " ".join(str(part) for part in content if isinstance(part, str))
        return ""
    return ""


class GrimoireMemory:
    """AutoGen memory: ``add`` records, ``query`` recalls, ``update_context`` injects.

    ``agent`` is stamped on every write so recall can say which agent recorded
    a fact. ``limit`` bounds how many facts one recall returns.
    """

    def __init__(self, client: Grimoire, *, agent: str = DEFAULT_AGENT, limit: int = 5) -> None:
        self.client = client
        self.agent = agent
        self.limit = limit

    # ---- writing ------------------------------------------------------

    def _write(self, text: str, topic: str) -> None:
        self.client.add(text, topic=topic, **agent_write(self.agent))

    async def add(self, content: Any, cancellation_token: Any = None) -> None:
        """Record one fact. ``content.metadata["topic"]`` files it under a topic."""
        text = _text_of_content(content).strip()
        if not text:
            return
        metadata = getattr(content, "metadata", None) or {}
        topic = str(metadata.get("topic", "") or "")
        await asyncio.to_thread(self._write, text, topic)

    # ---- reading ------------------------------------------------------

    def _recall(self, query: str) -> list[Memory]:
        return self.client.search(query, limit=self.limit)

    async def query(self, query: Any = "", cancellation_token: Any = None, **_: Any) -> Any:
        """Currently believed facts about ``query`` (a string or ``MemoryContent``)."""
        text = _text_of_content(query) if query else ""
        if not text.strip():
            return _QUERY_RESULT(results=[])
        memories = await asyncio.to_thread(self._recall, text)
        return _QUERY_RESULT(results=[self._content(memory) for memory in memories])

    @staticmethod
    def _content(memory: Memory) -> Any:
        metadata = {
            "id": memory.id,
            "path": memory.path,
            "score": memory.score,
            "trust": memory.trust,
            "authority": memory.authority,
            "origin": memory.origin,
            "agent": memory.agent,
            "session": memory.session,
            "stamp": memory.stamp,
            "source": "grimoire",
        }
        return _CONTENT(content=text_of(memory), mime_type=TEXT_MIME, metadata=metadata)

    async def update_context(self, model_context: Any) -> Any:
        """Recall for the latest user message and add the facts as a system message.

        Adds nothing, and reports no memories, when there is no user message,
        nothing matches, or the server cannot be reached.
        """
        query = _latest_user_text(list(await model_context.get_messages()))
        if not query.strip():
            return _UPDATE_RESULT(memories=_QUERY_RESULT(results=[]))
        try:
            memories = await asyncio.to_thread(self._recall, query)
        except GrimoireError:
            return _UPDATE_RESULT(memories=_QUERY_RESULT(results=[]))
        if not memories:
            return _UPDATE_RESULT(memories=_QUERY_RESULT(results=[]))
        block = format_memories(memories)
        await model_context.add_message(_SYSTEM(content=f"Relevant memories:\n{block}"))
        return _UPDATE_RESULT(memories=_QUERY_RESULT(results=[self._content(m) for m in memories]))

    # ---- lifecycle ----------------------------------------------------

    async def clear(self) -> None:
        """No-op. Grimoire memory is shared and durable; this does not erase it.

        AutoGen's own memories are cleared in process. Forgetting a fact on
        the server is a deliberate act: use ``client.delete`` or ``client.forget``.
        """

    async def close(self) -> None:
        """No-op. The client opens a connection per request and holds none."""
