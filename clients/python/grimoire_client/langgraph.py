"""LangGraph store backed by Grimoire.

LangGraph's ``BaseStore`` is a namespaced key-value store for cross-thread
memory. Grimoire's memory is facts in markdown files. The two fit together with
one mapping decision, made explicit here rather than hidden:

* a **namespace** becomes a memory note — ``("memories", "alice")`` is
  ``memory/memories-alice.md``, a file you can open
* a **key** becomes the fact's ``task``, which is Grimoire's free-form
  provenance field, so ``get`` and ``delete`` can address exactly what ``put``
  wrote
* a **value** is stored as its ``text`` field when it has one, and as compact
  JSON otherwise — so a store used for plain facts stays readable, and one used
  for structured state stays lossless

By default writes are **verbatim**: a key-value store has to return what was
put, and reconciliation could supersede it. Pass ``reconcile=True`` to opt into
the reconciled behaviour instead, which is worth it for a store holding facts
rather than state — with the consequence that a superseded value stops being
returned by ``get``, because it stopped being true. Reconciliation is confined
to the namespace either way: a write here can never supersede a fact in
another namespace.
"""

from __future__ import annotations

import json
import re
from datetime import datetime, timezone
from typing import Any, Iterable

from . import Grimoire, NotFound

try:  # pragma: no cover - exercised only where langgraph is installed
    from langgraph.store.base import (
        BaseStore,
        GetOp,
        Item,
        ListNamespacesOp,
        Op,
        PutOp,
        SearchItem,
        SearchOp,
    )
except ImportError as exc:  # pragma: no cover
    raise ImportError(
        "GrimoireStore needs langgraph: pip install 'grimoire-client[langgraph]'"
    ) from exc

__all__ = ["GrimoireStore"]

NAMESPACE_SEPARATOR = "-"

# The server slugifies a topic into a filename: everything that is not a
# letter, number, underscore, space or dash is dropped, the rest is lowercased,
# and runs of space/underscore/dash collapse to one dash. The client has to
# apply the same rule, or the path it later filters on is not the path the note
# was written to — which reads as "the store lost everything you put in it".
_SLUG_STRIP = re.compile(r"[^\w\s-]", re.UNICODE)
_SLUG_DASH = re.compile(r"[\s_-]+", re.UNICODE)


def slugify(text: str) -> str:
    """Mirror of the server's note-name slug."""
    stripped = _SLUG_STRIP.sub("", text).strip().lower()
    return _SLUG_DASH.sub("-", stripped) or "untitled"


class GrimoireStore(BaseStore):
    """A ``BaseStore`` whose memories are markdown files you own.

    >>> store = GrimoireStore(Grimoire("http://localhost:9111"))
    >>> store.put(("memories", "alice"), "pref", {"text": "prefers tabs"})
    >>> store.search(("memories", "alice"), query="indentation")[0].value["text"]
    'prefers tabs'
    """

    #: Grimoire evaluates time-to-live at query time; nothing sweeps.
    supports_ttl = True

    def __init__(self, client: Grimoire, *, reconcile: bool = False) -> None:
        self.client = client
        self.reconcile = reconcile

    # BaseStore routes get/put/search/delete through batch, so implementing
    # this one method implements all of them.
    def batch(self, ops: Iterable[Op]) -> list[Any]:
        return [self._one(op) for op in ops]

    async def abatch(self, ops: Iterable[Op]) -> list[Any]:
        # The client is synchronous on purpose (stdlib urllib, no dependency
        # tree). Async callers get correct results rather than a false promise
        # of concurrency; swap in an async client if that ever costs anything.
        return self.batch(ops)

    def _one(self, op: Op) -> Any:
        if isinstance(op, PutOp):
            return self._put(op)
        if isinstance(op, GetOp):
            return self._get(op)
        if isinstance(op, SearchOp):
            return self._search(op)
        if isinstance(op, ListNamespacesOp):
            return self._list_namespaces(op)
        raise NotImplementedError(f"unsupported store operation: {type(op).__name__}")

    # ---- operations ---------------------------------------------------

    def _put(self, op: PutOp) -> None:
        topic = topic_for(op.namespace)
        if op.value is None:
            self._delete(topic, op.key)
            return
        # A put replaces what that key held, which reconciliation would not
        # necessarily do — so the old fact goes first, whatever mode we are in.
        self._delete(topic, op.key)
        self.client.remember(
            encode_value(op.value),
            topic=topic,
            task=op.key,
            expires_in=f"{int(op.ttl * 60)}s" if op.ttl else "",
            infer=self.reconcile,
            # Confined to this namespace's note. Without it a reconciled write
            # here could supersede a fact in a namespace belonging to another
            # user — which is the isolation a namespaced store is for.
            scope="topic",
        )

    def _get(self, op: GetOp) -> Item | None:
        facts = self.client.search(
            "", path=note_for(op.namespace), task=op.key, limit=1
        )
        if not facts:
            return None
        return item_for(op.namespace, facts[0])

    def _search(self, op: SearchOp) -> list[SearchItem]:
        facts = self.client.search(
            op.query or "",
            path=note_for(op.namespace_prefix),
            limit=op.limit + op.offset,
            **_filters(op.filter),
        )
        return [
            SearchItem(
                namespace=op.namespace_prefix,
                key=fact.task or fact.id,
                value=decode_value(fact.text),
                created_at=_stamp(fact.stamp),
                updated_at=_stamp(fact.stamp),
                score=fact.score,
            )
            for fact in facts[op.offset :]
        ]

    def _list_namespaces(self, op: ListNamespacesOp) -> list[tuple[str, ...]]:
        seen: dict[tuple[str, ...], None] = {}
        for fact in self.client.get_all():
            namespace = namespace_for(fact.path)
            if namespace:
                seen.setdefault(namespace, None)
        namespaces = list(seen)
        if op.max_depth is not None:
            namespaces = list(
                dict.fromkeys(tuple(ns[: op.max_depth]) for ns in namespaces)
            )
        return namespaces[op.offset : op.offset + op.limit]

    def _delete(self, topic: str, key: str) -> None:
        note = f"memory/{topic}.md"
        for fact in self.client.search("", path=note, task=key, limit=50):
            # Check the key again on this side. A server that does not know
            # the `task` filter — an older one, or a proxy that strips query
            # parameters — answers with every fact in the note, and the next
            # line deletes what it is handed. Trusting the filter cost a whole
            # namespace the first time this ran against a stale binary.
            if fact.task != key:
                continue
            try:
                # hard, not a retraction: a key-value store that keeps
                # returning a deleted key from `search` is broken, whatever
                # the audit trail says. The note's history still has it.
                self.client.delete(fact.path, fact.id, hard=True)
            except NotFound:
                pass


# ---- mapping helpers --------------------------------------------------


def topic_for(namespace: tuple[str, ...]) -> str:
    """A namespace tuple as one memory topic slug.

    Slugified, so it is already the name the note will have. Namespace parts
    that differ only in punctuation therefore collide — ``("a.b",)`` and
    ``("ab",)`` are one namespace — which is the price of the mapping being a
    filename you can open.
    """
    joined = NAMESPACE_SEPARATOR.join(part.strip() for part in namespace if part.strip())
    return slugify(joined)


def note_for(namespace: tuple[str, ...]) -> str:
    return f"memory/{topic_for(namespace)}.md"


def namespace_for(path: str) -> tuple[str, ...]:
    """The inverse of :func:`note_for`, for listing namespaces.

    Lossy in one direction: a namespace part containing the separator comes
    back split. Namespaces are conventionally short slugs
    (``("memories", user_id)``), so this is a documented edge rather than a
    problem in practice.
    """
    stem = path.removeprefix("memory/").removesuffix(".md")
    return tuple(part for part in stem.split(NAMESPACE_SEPARATOR) if part)


def encode_value(value: dict[str, Any]) -> str:
    """A value as the text of a fact.

    A dict that is just ``{"text": ...}`` — which is what a store holding
    facts looks like — is written as that text, so the note stays something a
    person reads. Anything else is JSON, so nothing is lost.
    """
    if set(value) == {"text"} and isinstance(value["text"], str):
        return value["text"]
    return json.dumps(value, separators=(",", ":"), sort_keys=True)


def decode_value(text: str) -> dict[str, Any]:
    """The inverse of :func:`encode_value`."""
    stripped = text.strip()
    if stripped.startswith("{") and stripped.endswith("}"):
        try:
            parsed = json.loads(stripped)
        except json.JSONDecodeError:
            return {"text": text}
        if isinstance(parsed, dict):
            return parsed
    return {"text": text}


def item_for(namespace: tuple[str, ...], fact) -> Item:
    return Item(
        namespace=namespace,
        key=fact.task or fact.id,
        value=decode_value(fact.text),
        created_at=_stamp(fact.stamp),
        updated_at=_stamp(fact.stamp),
    )


def _filters(filter: dict[str, Any] | None) -> dict[str, str]:
    """Translate the filters Grimoire can apply; ignore the rest.

    LangGraph filters on arbitrary value fields, which a fact-shaped store
    cannot do in general. The three that map exactly are passed through.
    """
    if not filter:
        return {}
    return {
        key: str(filter[key])
        for key in ("agent", "session", "category")
        if filter.get(key)
    }


def _stamp(stamp: str) -> datetime:
    for layout in ("%Y-%m-%d %H:%M", "%Y-%m-%d"):
        try:
            return datetime.strptime(stamp, layout).replace(tzinfo=timezone.utc)
        except ValueError:
            continue
    return datetime.now(timezone.utc)
