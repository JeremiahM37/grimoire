"""LlamaIndex adapters: a retriever and a memory block, both duck-typed.

    from grimoire_client import Grimoire
    from grimoire_client.llamaindex import GrimoireMemoryBlock, GrimoireRetriever

    client = Grimoire("http://localhost:9111")
    retriever = GrimoireRetriever(client, agent="research-bot")
    nodes = retriever.retrieve("how is the kestrel cluster deployed?")

    memory = GrimoireMemoryBlock(client, agent="research-bot", session="run-12")
    block_text = memory.get(chat_messages)   # recalled facts for the latest user turn
    memory.put(chat_messages)                # record the new turns, once each

``llama_index.core`` is optional. With it present, the nodes are its
``TextNode`` and ``NodeWithScore``; without it they are small stand-ins with the
same attribute names. The memory block mirrors the ``get``/``put`` contract that
LlamaIndex's agent memory blocks use, with ``aget``/``aput`` for the async
path, and is passed to an agent as a block of memory.

Messages are read duck-typed: anything with ``role`` and ``content`` (or
``blocks``), or a plain dict, works, including the enum roles LlamaIndex uses.
"""

from __future__ import annotations

import asyncio
import hashlib
from collections.abc import Iterable
from dataclasses import dataclass, field
from typing import Any

from . import Grimoire, Memory
from ._common import agent_write, format_memories
from .fencing import text_of

try:  # pragma: no cover - exercised only where llama_index.core is installed
    from llama_index.core.schema import NodeWithScore as _LINodeWithScore
    from llama_index.core.schema import TextNode as _LITextNode
except ImportError:
    _LINodeWithScore = None
    _LITextNode = None

__all__ = ["GrimoireMemoryBlock", "GrimoireRetriever", "NodeWithScore", "TextNode"]

DEFAULT_AGENT = "llamaindex"


@dataclass
class TextNode:
    """Stand-in for ``llama_index.core.schema.TextNode`` when it is absent."""

    text: str = ""
    id_: str = ""
    metadata: dict[str, Any] = field(default_factory=dict)

    def get_content(self, metadata_mode: Any = None) -> str:
        return self.text


@dataclass
class NodeWithScore:
    """Stand-in for ``llama_index.core.schema.NodeWithScore`` when it is absent."""

    node: Any
    score: float | None = None

    def get_content(self, metadata_mode: Any = None) -> str:
        return self.node.get_content()


_NODE = _LITextNode or TextNode
_SCORED = _LINodeWithScore or NodeWithScore


def _query_text(query: Any) -> str:
    """The string to search for, from a str or a LlamaIndex ``QueryBundle``."""
    if isinstance(query, str):
        return query
    return str(getattr(query, "query_str", query))


def _role_of(message: Any) -> str:
    """``user``, ``assistant`` or the raw role, whatever the message type."""
    if isinstance(message, dict):
        role = message.get("role", "")
    else:
        role = getattr(message, "role", "")
    role = getattr(role, "value", role)  # LlamaIndex MessageRole is an enum
    role = str(role or "").lower()
    if role in ("user", "human"):
        return "user"
    if role in ("assistant", "ai", "chatbot", "model"):
        return "assistant"
    return role


def _content_of(message: Any) -> str:
    if isinstance(message, dict):
        content = message.get("content")
        blocks = message.get("blocks")
    else:
        content = getattr(message, "content", None)
        blocks = getattr(message, "blocks", None)
    if isinstance(content, str):
        return content
    if blocks:
        parts = []
        for block in blocks:
            text = block.get("text") if isinstance(block, dict) else getattr(block, "text", None)
            if isinstance(text, str) and text:
                parts.append(text)
        return " ".join(parts)
    return ""


class GrimoireRetriever:
    """Retrieve facts as LlamaIndex nodes, with the server's score."""

    def __init__(self, client: Grimoire, *, agent: str = "", limit: int = 5,
                 session: str = "", category: str = "") -> None:
        self.client = client
        self.agent = agent
        self.limit = limit
        self.session = session
        self.category = category

    def retrieve(self, query_bundle_or_str: Any) -> list[Any]:
        query = _query_text(query_bundle_or_str)
        if not query.strip():
            return []
        memories = self.client.search(
            query, limit=self.limit, agent=self.agent, session=self.session,
            category=self.category,
        )
        return [self._node(memory) for memory in memories]

    async def aretrieve(self, query_bundle_or_str: Any) -> list[Any]:
        return await asyncio.to_thread(self.retrieve, query_bundle_or_str)

    @staticmethod
    def _node(memory: Memory) -> Any:
        metadata = {
            "id": memory.id,
            "path": memory.path,
            "trust": memory.trust,
            "authority": memory.authority,
            "origin": memory.origin,
            "freshness": dict(memory.freshness),
            "agent": memory.agent,
            "session": memory.session,
            "stamp": memory.stamp,
            "source": "grimoire",
        }
        node = _NODE(text=text_of(memory), id_=memory.id, metadata=metadata)
        return _SCORED(node=node, score=memory.score)


class GrimoireMemoryBlock:
    """A memory block: recall for ``get``, retain for ``put``.

    ``get`` recalls against the latest user message and returns the formatted
    facts ("" when there is nothing to say), so an agent can splice it into its
    context. ``put`` records user and assistant turns as agent-authored facts in
    ``session``. System and tool messages are never recorded.

    A turn is recorded once per block instance: ``put`` is called with the whole
    history each time, and a content hash of each turn already written stops it
    being stored again. The hash set is in memory only, so a new process can
    re-record a turn once. Set ``retain=False`` for a read-only block.
    """

    name = "grimoire"

    def __init__(self, client: Grimoire, *, agent: str = DEFAULT_AGENT, session: str = "",
                 limit: int = 5, retain: bool = True) -> None:
        self.client = client
        self.agent = agent
        self.session = session
        self.limit = limit
        self.retain = retain
        self._written: set[str] = set()

    def get(self, messages: Iterable[Any] | None = None) -> str:
        query = ""
        for message in reversed(list(messages or [])):
            if _role_of(message) == "user":
                query = _content_of(message).strip()
                break
        if not query:
            return ""
        memories = self.client.search(query, limit=self.limit, session=self.session)
        return format_memories(memories)

    def put(self, messages: Iterable[Any] | None = None) -> None:
        if not self.retain:
            return
        for message in messages or []:
            role = _role_of(message)
            if role not in ("user", "assistant"):
                continue
            text = _content_of(message).strip()
            if not text:
                continue
            digest = hashlib.sha256(f"{role}\n{text}".encode()).hexdigest()
            if digest in self._written:
                continue
            self._written.add(digest)
            self.client.remember(
                f"{role}: {text}",
                session=self.session,
                infer=False,
                **agent_write(self.agent),
            )

    async def aget(self, messages: Iterable[Any] | None = None) -> str:
        return await asyncio.to_thread(self.get, list(messages or []))

    async def aput(self, messages: Iterable[Any] | None = None) -> None:
        await asyncio.to_thread(self.put, list(messages or []))

