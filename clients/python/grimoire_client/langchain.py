"""LangChain adapters: a chat message history, a retriever and memory tools.

    from grimoire_client import Grimoire
    from grimoire_client.langchain import (
        GrimoireChatMessageHistory, GrimoireRetriever, make_recall_tool, make_remember_tool,
    )

    client = Grimoire("http://localhost:9111")
    history = GrimoireChatMessageHistory(client, session_id="chat-7", agent="support-bot")
    retriever = GrimoireRetriever(client, agent="support-bot", limit=5)
    tools = [make_recall_tool(client), make_remember_tool(client, agent="support-bot")]

``langchain_core`` is optional. When it is importable the messages, documents
and tools are its real classes, so they drop into a chain unchanged. When it is
not, the same attributes come back on small stand-ins, and the module still
imports. Nothing here needs the rest of LangChain.

Writes are agent-authored (see :mod:`grimoire_client._common`). A retriever
returns current beliefs only, as :meth:`Grimoire.search` does, and fences
untrusted text in ``page_content`` (see :mod:`grimoire_client.fencing`).

``GrimoireRetriever`` follows the ``BaseRetriever`` shape (``invoke``,
``ainvoke``, ``get_relevant_documents``) but is not a subclass of it. Pydantic
field declarations on that base would make the optional import fragile.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import re
import threading
from collections.abc import Iterable
from dataclasses import dataclass, field
from typing import Any

from . import Grimoire, Memory, NotFound
from ._common import agent_write, recall_facts, remember_fact
from .fencing import text_of

try:  # pragma: no cover - exercised only where langchain_core is installed
    from langchain_core.chat_history import BaseChatMessageHistory as _HistoryBase
    from langchain_core.documents import Document as _LCDocument
    from langchain_core.messages import AIMessage, HumanMessage, SystemMessage
    from langchain_core.tools import StructuredTool
except ImportError:
    _HistoryBase = object
    _LCDocument = None
    AIMessage = HumanMessage = SystemMessage = None
    StructuredTool = None

__all__ = [
    "Document",
    "GrimoireChatMessageHistory",
    "GrimoireRetriever",
    "StoredMessage",
    "make_recall_tool",
    "make_remember_tool",
]

DEFAULT_AGENT = "langchain"


@dataclass
class Document:
    """Stand-in for ``langchain_core.documents.Document`` when it is absent."""

    page_content: str
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass
class StoredMessage:
    """Stand-in for a LangChain message when ``langchain_core`` is absent.

    ``type`` is ``"human"``, ``"ai"``, ``"system"`` or another LangChain type
    name, which is what LangChain itself uses.
    """

    type: str
    content: str


_DOCUMENT = _LCDocument or Document
_MESSAGE_CLASSES = {"human": HumanMessage, "ai": AIMessage, "system": SystemMessage}
_ROLE_TO_TYPE = {"user": "human", "human": "human", "assistant": "ai", "ai": "ai",
                 "system": "system"}
_SEQ_TASK = re.compile(r"^(\d{6}):([a-z]+)$")


def _to_message(kind: str, content: str) -> Any:
    cls = _MESSAGE_CLASSES.get(kind)
    if cls is not None:
        return cls(content=content)
    return StoredMessage(type=kind, content=content)


def _kind_of(message: Any) -> str:
    if isinstance(message, dict):
        role = message.get("role") or message.get("type") or ""
    else:
        role = getattr(message, "type", None) or getattr(message, "role", None) or ""
    role = re.sub(r"[^a-z]", "", str(role).lower()) or "human"
    return _ROLE_TO_TYPE.get(role, role)


def _content_text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = [p.get("text", "") if isinstance(p, dict) else str(p) for p in content
                 if not isinstance(p, dict) or p.get("type", "text") == "text"]
        return " ".join(p for p in parts if p)
    return json.dumps(content, default=str)


class GrimoireChatMessageHistory(_HistoryBase):  # type: ignore[misc, valid-type]
    """A chat history whose messages are facts in one session.

    Every message is one fact tagged with the session id. Its ``task`` carries
    the sequence number and the message type, so :attr:`messages` comes back in
    the order it was written, whatever the server's recall order is. Messages
    are written verbatim (``infer=False``): repeated chatter such as "ok" must
    not supersede earlier turns.

    :meth:`clear` hard-deletes the session's facts, so an erased conversation
    is not recalled later. The sequence counter is held in memory, so two
    processes appending to one session can interleave; use one writer per
    session.
    """

    def __init__(self, client: Grimoire, session_id: str, *, agent: str = DEFAULT_AGENT,
                 limit: int = 500) -> None:
        if not session_id:
            raise ValueError("session_id is required: it is what scopes the history")
        self.client = client
        self.session_id = session_id
        self.agent = agent
        self.limit = limit
        self._next: int | None = None
        self._lock = threading.Lock()

    # ---- reading ------------------------------------------------------

    def _entries(self) -> list[tuple[int, str, Memory]]:
        facts = self.client.search("", session=self.session_id, limit=self.limit)
        out = []
        for fact in facts:
            match = _SEQ_TASK.match(fact.task or "")
            if match:
                out.append((int(match.group(1)), match.group(2), fact))
        out.sort(key=lambda entry: entry[0])
        return out

    @property
    def messages(self) -> list[Any]:
        return [_to_message(kind, fact.text) for _, kind, fact in self._entries()]

    # ---- writing ------------------------------------------------------

    def _take_seq(self) -> int:
        with self._lock:
            if self._next is None:
                entries = self._entries()
                self._next = entries[-1][0] + 1 if entries else 0
            seq = self._next
            self._next += 1
            return seq

    def add_message(self, message: Any) -> None:
        raw = message.get("content", "") if isinstance(message, dict) else getattr(message, "content", message)
        content = _content_text(raw)
        if not content.strip():
            return
        kind = _kind_of(message)
        self.client.remember(
            content,
            session=self.session_id,
            task=f"{self._take_seq():06d}:{kind}",
            infer=False,
            **agent_write(self.agent),
        )

    def add_messages(self, messages: Iterable[Any]) -> None:
        for message in messages:
            self.add_message(message)

    def add_user_message(self, message: str) -> None:
        self.add_message(_to_message("human", message))

    def add_ai_message(self, message: str) -> None:
        self.add_message(_to_message("ai", message))

    def clear(self) -> None:
        for _, _, fact in self._entries():
            try:
                self.client.delete(fact.path, fact.id, hard=True)
            except NotFound:
                pass
        with self._lock:
            self._next = 0


class GrimoireRetriever:
    """Retrieve facts for a query as documents (``invoke`` / ``ainvoke``).

    ``metadata`` carries ``id``, ``path``, ``score``, ``trust``, ``authority``,
    ``origin``, ``freshness`` and the write's agent and session, so a chain can
    rank or filter on them. Errors propagate: a retriever's caller decides
    whether a missing memory is fatal.
    """

    def __init__(self, client: Grimoire, *, agent: str = "", limit: int = 5,
                 session: str = "", category: str = "") -> None:
        self.client = client
        self.agent = agent
        self.limit = limit
        self.session = session
        self.category = category

    def invoke(self, input: Any, config: Any = None, **_: Any) -> list[Any]:
        query = input if isinstance(input, str) else str(getattr(input, "content", input))
        if not query.strip():
            return []
        memories = self.client.search(
            query, limit=self.limit, agent=self.agent, session=self.session,
            category=self.category,
        )
        return [self._document(memory) for memory in memories]

    async def ainvoke(self, input: Any, config: Any = None, **kwargs: Any) -> list[Any]:
        return await asyncio.to_thread(self.invoke, input, config, **kwargs)

    def get_relevant_documents(self, query: str, **kwargs: Any) -> list[Any]:
        return self.invoke(query, **kwargs)

    async def aget_relevant_documents(self, query: str, **kwargs: Any) -> list[Any]:
        return await self.ainvoke(query, **kwargs)

    @staticmethod
    def _document(memory: Memory) -> Any:
        metadata = {
            "id": memory.id,
            "path": memory.path,
            "score": memory.score,
            "trust": memory.trust,
            "authority": memory.authority,
            "origin": memory.origin,
            "freshness": dict(memory.freshness),
            "agent": memory.agent,
            "session": memory.session,
            "stamp": memory.stamp,
            "source": "grimoire",
        }
        return _DOCUMENT(page_content=text_of(memory), metadata=metadata)


def _tool(fn: Any, name: str) -> Any:
    """A plain callable with ``name``/``description``, or a StructuredTool."""
    fn.name = name
    fn.description = inspect.cleandoc(fn.__doc__ or name)
    if StructuredTool is not None:  # pragma: no cover - needs langchain_core
        return StructuredTool.from_function(fn, name=name, description=fn.description)
    return fn


def make_remember_tool(client: Grimoire, *, agent: str = DEFAULT_AGENT) -> Any:
    """A ``remember`` tool. Writes are reconciled and agent-authored."""

    def remember(text: str, topic: str = "") -> str:
        """Record one durable fact. A fact that contradicts a stored one replaces it.

        Args:
            text: The fact, as one self-contained sentence.
            topic: Optional topic to file it under.
        """
        return remember_fact(client, agent, text, topic)

    return _tool(remember, "remember")


def make_recall_tool(client: Grimoire, *, limit: int = 5) -> Any:
    """A ``recall`` tool over currently believed facts, from any agent."""

    def recall(query: str) -> str:
        """Recall what is currently remembered about a topic.

        Args:
            query: What to look up, in plain words.
        """
        return recall_facts(client, query, limit)

    return _tool(recall, "recall")
