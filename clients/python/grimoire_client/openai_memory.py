"""Give any OpenAI-compatible chat client a memory bank.

>>> from openai import OpenAI
>>> from grimoire_client import Grimoire, with_memory
>>> llm = with_memory(OpenAI(), Grimoire().bank("user-42"), session_id="chat-7")
>>> llm.chat.completions.create(model="gpt-x", messages=[{"role": "user", "content": "hi"}])

Before each ``chat.completions.create`` the wrapper recalls with the last user
message and adds a "Relevant memories" system block; after it, the exchange
(user turn + assistant reply) is retained into the bank. Everything else on the
client is passed through untouched.

Memory never breaks the chat call: a recall that fails is logged and the call
goes ahead without it; a retain that fails is logged (and, in background mode,
kept in :meth:`MemoryClient.pending_errors`).

Per-call overrides are keyword arguments prefixed ``grimoire_`` — for example
``grimoire_inject=False`` or ``grimoire_session_id="other"`` — and are removed
before the request reaches the model.
"""

from __future__ import annotations

import asyncio
import dataclasses
import inspect
import logging
import threading
from collections.abc import AsyncIterator, Iterator, Mapping
from datetime import UTC, datetime
from typing import Any

from . import GrimoireError
from .banks import AsyncBank, Bank

__all__ = ["MemoryClient", "MemorySettings", "format_memories", "with_memory"]

log = logging.getLogger("grimoire_client.memory")

HEADING = "# Relevant memories"
PREFIX = "grimoire_"


@dataclasses.dataclass
class MemorySettings:
    """What the wrapper does around each call. Every field can be overridden
    per call with a ``grimoire_<field>`` keyword argument."""

    inject: bool = True
    store: bool = True
    budget: str = "mid"
    max_tokens: int = 4096
    types: list[str] | None = None
    recall_tags: list[str] | None = None
    recall_tags_match: str | None = None
    #: Cap on how many recalled facts are listed (None: all that fit max_tokens).
    max_memories: int | None = None
    #: Tags put on what is retained.
    tags: list[str] | None = None
    #: The retained document id; a session accumulates in one document.
    session_id: str | None = None
    #: A fixed recall query instead of the last user message.
    query: str | None = None
    #: Retain on a daemon thread instead of before create() returns.
    background: bool = False


def _get(obj: Any, key: str, default: Any = None) -> Any:
    if isinstance(obj, Mapping):
        return obj.get(key, default)
    return getattr(obj, key, default)


def _text(content: Any) -> str:
    """Text of a message's content: a string, or the text parts of a list."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = [_get(p, "text", "") for p in content if _get(p, "type") == "text"]
        return " ".join(p for p in parts if p)
    return ""


def last_user_text(messages: list[Any]) -> str:
    for message in reversed(messages):
        if _get(message, "role") == "user":
            return _text(_get(message, "content")).strip()
    return ""


def _when(fact: Mapping[str, Any]) -> str:
    start, end = fact.get("occurred_start"), fact.get("occurred_end")
    if start and end and start != end:
        return f"(occurred: {start} to {end}) "
    if start:
        return f"(occurred: {start}) "
    if fact.get("mentioned_at"):
        return f"(mentioned: {fact['mentioned_at']}) "
    return ""


def format_memories(results: list[Mapping[str, Any]], now: datetime | None = None,
                    limit: int | None = None) -> str:
    """The injected block, or "" when there is nothing to inject."""
    rows = [r for r in results if (r.get("text") or "").strip()]
    if limit is not None:
        rows = rows[:limit]
    if not rows:
        return ""
    now = now or datetime.now(UTC)
    lines = [
        HEADING,
        f"Current date/time: {now.strftime('%Y-%m-%d %H:%M:%S')} UTC",
        "The following information from memory may be relevant:",
        "",
    ]
    for i, fact in enumerate(rows, 1):
        kind = (fact.get("type") or "world").upper()
        note = " (written by a person)" if fact.get("authority") == "human" else ""
        disputed = " (disputed by a person's correction)" if fact.get("disputed_by") else ""
        lines.append(f"{i}. [{kind}] {_when(fact)}{fact['text'].strip()}{note}{disputed}")
    return "\n".join(lines)


def inject(messages: list[Any], block: str) -> list[Any]:
    """A copy of ``messages`` with ``block`` added to the system prompt."""
    out = list(messages)
    for i, message in enumerate(out):
        if _get(message, "role") == "system" and isinstance(message, Mapping):
            existing = _text(message.get("content"))
            out[i] = {**message, "content": (existing + "\n\n" + block) if existing else block}
            return out
    return [{"role": "system", "content": block}, *out]


def reply_text(response: Any) -> str:
    choices = _get(response, "choices") or []
    if not choices:
        return ""
    return _text(_get(_get(choices[0], "message"), "content")) or ""


def exchange_text(user: str, assistant: str) -> str:
    if user.startswith(HEADING):
        return ""
    if not user.strip() or not assistant.strip():
        return ""
    return f"USER: {user}\n\nASSISTANT: {assistant}"


class _Core:
    """Settings, recall and retain shared by the sync and async wrappers."""

    def __init__(self, bank: Bank, settings: MemorySettings) -> None:
        self.bank = bank
        self.settings = settings
        self._errors: list[BaseException] = []
        self._lock = threading.Lock()
        self._threads: list[threading.Thread] = []

    def split(self, kwargs: dict[str, Any]) -> tuple[MemorySettings, dict[str, Any]]:
        overrides = {k[len(PREFIX):]: kwargs.pop(k) for k in list(kwargs) if k.startswith(PREFIX)}
        known = {f.name for f in dataclasses.fields(MemorySettings)}
        unknown = set(overrides) - known
        if unknown:
            raise TypeError(f"unknown memory option(s): {', '.join(PREFIX + u for u in sorted(unknown))}")
        return dataclasses.replace(self.settings, **overrides), kwargs

    def recall_block(self, settings: MemorySettings, messages: list[Any]) -> str:
        query = settings.query or last_user_text(messages)
        if not settings.inject or not query:
            return ""
        try:
            out = self.bank.recall(
                query[:2000], budget=settings.budget, max_tokens=settings.max_tokens,
                types=settings.types, tags=settings.recall_tags,
                tags_match=settings.recall_tags_match if settings.recall_tags else None,
            )
        except (GrimoireError, OSError, ValueError) as exc:
            log.warning("grimoire recall failed; continuing without memories: %s", exc)
            return ""
        return format_memories((out or {}).get("results") or [], limit=settings.max_memories)

    def retain(self, settings: MemorySettings, model: str, user: str, assistant: str) -> None:
        if not settings.store:
            return
        content = exchange_text(user, assistant)
        if not content:
            return

        def run() -> None:
            kwargs: dict[str, Any] = {
                "timestamp": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
                "context": f"conversation:openai:{model or 'unknown'}",
                "metadata": {"source": "openai-wrapper", "model": model or ""},
                "tags": settings.tags,
            }
            if settings.session_id:
                kwargs["document_id"] = settings.session_id
                try:
                    self.bank.retain(content, update_mode="append", **kwargs)
                    return
                except GrimoireError as exc:
                    if exc.status != 400:
                        raise
            self.bank.retain(content, **kwargs)

        if not settings.background:
            try:
                run()
            except (GrimoireError, OSError, ValueError) as exc:
                log.warning("grimoire retain failed: %s", exc)
            return

        def background() -> None:
            try:
                run()
            except BaseException as exc:  # reported, never raised
                log.warning("grimoire background retain failed: %s", exc)
                with self._lock:
                    self._errors.append(exc)

        thread = threading.Thread(target=background, name="grimoire-retain", daemon=True)
        thread.start()
        with self._lock:
            self._threads = [t for t in self._threads if t.is_alive()] + [thread]

    def pending_errors(self) -> list[BaseException]:
        with self._lock:
            errors, self._errors = self._errors, []
        return errors

    def flush(self, timeout: float | None = None) -> None:
        with self._lock:
            threads = list(self._threads)
        for thread in threads:
            thread.join(timeout)


class _Proxy:
    """Pass every attribute through except the one path we intercept."""

    def __init__(self, target: Any, overrides: Mapping[str, Any]) -> None:
        self._target = target
        self._overrides = overrides

    def __getattr__(self, name: str) -> Any:
        if name in self._overrides:
            return self._overrides[name]
        return getattr(self._target, name)


class MemoryClient:
    """An OpenAI-compatible client whose ``chat.completions.create`` recalls
    before the call and retains after it. Works for sync and async clients."""

    def __init__(self, client: Any, bank: Bank | AsyncBank, settings: MemorySettings) -> None:
        if isinstance(bank, AsyncBank):
            bank = bank.sync
        self._client = client
        self._core = _Core(bank, settings)
        create = client.chat.completions.create
        self.is_async = inspect.iscoroutinefunction(create)
        completions = _Proxy(client.chat.completions,
                             {"create": self._acreate if self.is_async else self._create})
        self.chat = _Proxy(client.chat, {"completions": completions})

    def __getattr__(self, name: str) -> Any:
        return getattr(self._client, name)

    @property
    def settings(self) -> MemorySettings:
        return self._core.settings

    def pending_errors(self) -> list[BaseException]:
        """Errors from background retains since the last call, oldest first."""
        return self._core.pending_errors()

    def flush(self, timeout: float | None = None) -> None:
        """Wait for background retains to finish."""
        self._core.flush(timeout)

    def _prepare(self, kwargs: dict[str, Any]) -> tuple[MemorySettings, dict[str, Any], str]:
        settings, kwargs = self._core.split(dict(kwargs))
        messages = list(kwargs.get("messages") or [])
        user = last_user_text(messages)
        return settings, kwargs, user

    def _create(self, *args: Any, **kwargs: Any) -> Any:
        settings, kwargs, user = self._prepare(kwargs)
        block = self._core.recall_block(settings, kwargs.get("messages") or [])
        if block:
            kwargs["messages"] = inject(list(kwargs["messages"]), block)
        response = self._client.chat.completions.create(*args, **kwargs)
        model = kwargs.get("model", "")
        if kwargs.get("stream"):
            return self._stream(response, settings, model, user)
        self._core.retain(settings, model, user, reply_text(response))
        return response

    async def _acreate(self, *args: Any, **kwargs: Any) -> Any:
        settings, kwargs, user = self._prepare(kwargs)
        block = await asyncio.to_thread(self._core.recall_block, settings,
                                        kwargs.get("messages") or [])
        if block:
            kwargs["messages"] = inject(list(kwargs["messages"]), block)
        response = await self._client.chat.completions.create(*args, **kwargs)
        model = kwargs.get("model", "")
        if kwargs.get("stream"):
            return self._astream(response, settings, model, user)
        await asyncio.to_thread(self._core.retain, settings, model, user, reply_text(response))
        return response

    def _stream(self, stream: Any, settings: MemorySettings, model: str, user: str) -> Iterator[Any]:
        parts: list[str] = []
        try:
            for chunk in stream:
                parts.append(_delta(chunk))
                yield chunk
        finally:
            self._core.retain(settings, model, user, "".join(parts))

    async def _astream(self, stream: Any, settings: MemorySettings, model: str,
                       user: str) -> AsyncIterator[Any]:
        parts: list[str] = []
        try:
            async for chunk in stream:
                parts.append(_delta(chunk))
                yield chunk
        finally:
            await asyncio.to_thread(self._core.retain, settings, model, user, "".join(parts))


def _delta(chunk: Any) -> str:
    choices = _get(chunk, "choices") or []
    if not choices:
        return ""
    return _get(_get(choices[0], "delta"), "content") or ""


def with_memory(client: Any, bank: Bank | AsyncBank, **settings: Any) -> MemoryClient:
    """Wrap ``client`` (``OpenAI()``, ``AsyncOpenAI()``, or anything with the
    same ``chat.completions.create``) so it recalls from and retains into
    ``bank``. Keyword arguments are :class:`MemorySettings` fields."""
    return MemoryClient(client, bank, MemorySettings(**settings))
