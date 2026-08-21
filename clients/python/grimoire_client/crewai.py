"""CrewAI storage backend and embedder backed by Grimoire.

CrewAI's ``StorageBackend`` protocol is **vector-in**: ``search`` receives a
query embedding rather than the query text, and records carry their own
vectors. Honouring that with a server that owns its embedding space would
normally mean storing CrewAI's vectors — which cannot be derived from the
markdown, so the index would stop being rebuildable from the vault, and that
property is what the rest of the design rests on.

The way out is to make it ONE space instead of two. CrewAI takes an embedder,
and :class:`GrimoireEmbedder` is one that calls the server's own model. Every
vector CrewAI then computes — for a record it saves and for a query it runs —
is in the space the stored facts already live in, nothing foreign is persisted,
and the server refuses a vector of the wrong width rather than returning a
confident number computed across two unrelated coordinate systems.

    from crewai.memory.unified_memory import Memory
    from grimoire_client import Grimoire
    from grimoire_client.crewai import GrimoireEmbedder, GrimoireStorage

    client = Grimoire("http://localhost:9111")
    memory = Memory(
        storage=GrimoireStorage(client),
        embedder=GrimoireEmbedder(client),
    )

Facts land in ``memory/<scope>.md`` — files you open, diff and roll back —
rather than in a vector database.
"""

from __future__ import annotations

import json
from datetime import UTC, datetime
from typing import Any

from . import Grimoire, NotFound
from .langgraph import slugify

try:  # pragma: no cover - exercised only where crewai is installed
    from crewai.memory.types import MemoryRecord
except ImportError as exc:  # pragma: no cover
    raise ImportError(
        "GrimoireStorage needs crewai: pip install 'grimoire-client[crewai]'"
    ) from exc

__all__ = ["GrimoireEmbedder", "GrimoireStorage"]

DEFAULT_SCOPE = "crew"


class GrimoireEmbedder:
    """An embedding function that uses the server's model.

    Passing this to CrewAI is what makes the storage backend correct rather
    than merely functional: without it, CrewAI embeds with one model and
    Grimoire stored with another, and every similarity between them is
    meaningless. The server rejects a vector of the wrong width, so the
    mistake surfaces as an error instead of as retrieval quietly degrading.
    """

    def __init__(self, client: Grimoire) -> None:
        self.client = client

    def __call__(self, texts: list[str]) -> list[list[float]]:
        clean = [t if t and t.strip() else " " for t in texts]
        if not clean:
            return []
        out = self.client._request("POST", "/api/embed", {"texts": clean})
        return out.get("embeddings", [])


class GrimoireStorage:
    """A CrewAI ``StorageBackend`` whose memories are markdown files.

    Scopes become notes, record ids become the fact's provenance field, and a
    record with no metadata is stored as readable text rather than JSON — so a
    crew's memory stays something a person can read.
    """

    def __init__(self, client: Grimoire, *, reconcile: bool = False) -> None:
        self.client = client
        # Off by default for the same reason as the LangGraph store: a backend
        # asked to return a record by id has to return it. Turn it on for a
        # crew whose memory is beliefs rather than state, and a contradiction
        # will supersede instead of accumulating.
        self.reconcile = reconcile

    # ---- StorageBackend ------------------------------------------------

    def save(self, records: list[MemoryRecord]) -> None:
        for record in records:
            self._save_one(record)

    def search(
        self,
        query_embedding: list[float],
        scope_prefix: str | None = None,
        categories: list[str] | None = None,
        metadata_filter: dict[str, Any] | None = None,
        limit: int = 10,
        min_score: float = 0.0,
    ) -> list[tuple[MemoryRecord, float]]:
        body: dict[str, Any] = {"embedding": query_embedding, "limit": max(limit * 3, limit)}
        if scope_prefix:
            body["path"] = note_for(scope_prefix)
        if categories:
            # Grimoire carries one category per fact; a multi-category filter
            # narrows on the first and the rest is applied here, rather than
            # silently ignoring the others.
            body["category"] = slugify(categories[0])
        hits = self.client._request("POST", "/api/memory/search", body) or []

        out: list[tuple[MemoryRecord, float]] = []
        for hit in hits:
            record = self._record_of(hit)
            if categories and not set(categories) & set(record.categories):
                continue
            if metadata_filter and not _matches(record.metadata, metadata_filter):
                continue
            score = float(hit.get("scores", {}).get("semantic", hit.get("score", 0.0)))
            if score < min_score:
                continue
            out.append((record, score))
            if len(out) >= limit:
                break
        return out

    def delete(
        self,
        scope_prefix: str | None = None,
        categories: list[str] | None = None,
        record_ids: list[str] | None = None,
        older_than: datetime | None = None,
        metadata_filter: dict[str, Any] | None = None,
    ) -> int:
        removed = 0
        for hit in self._select(scope_prefix, categories, record_ids):
            record = self._record_of(hit)
            if metadata_filter and not _matches(record.metadata, metadata_filter):
                continue
            if older_than and record.created_at and record.created_at >= older_than:
                continue
            try:
                # Hard, not a retraction: a backend that keeps returning a
                # deleted record from search is broken, whatever the audit
                # trail says. The note's history still holds it.
                self.client.delete(hit["path"], hit["id"], hard=True)
                removed += 1
            except NotFound:
                pass
        return removed

    def update(self, record: MemoryRecord) -> None:
        self.delete(record_ids=[record.id])
        self._save_one(record)

    def get_record(self, record_id: str) -> MemoryRecord | None:
        for hit in self.client.search("", task=record_id, limit=1):
            return self._record_of(_as_dict(hit))
        return None

    def list_records(
        self, scope_prefix: str | None = None, limit: int = 200, offset: int = 0
    ) -> list[MemoryRecord]:
        facts = self.client.search(
            "",
            path=note_for(scope_prefix) if scope_prefix else "",
            limit=limit + offset,
        )
        return [self._record_of(_as_dict(f)) for f in facts[offset:]]

    def reset(self) -> None:
        """Forget everything this backend can see. CrewAI calls it in tests."""
        for fact in self.client.get_all(limit=1000):
            try:
                self.client.delete(fact.path, fact.id, hard=True)
            except NotFound:
                pass

    # ---- mapping -------------------------------------------------------

    def _save_one(self, record: MemoryRecord) -> None:
        # A save with a known id replaces what that id held, which
        # reconciliation would not necessarily do.
        self.delete(record_ids=[record.id])
        self.client.remember(
            encode_record(record),
            topic=topic_for(record.scope or DEFAULT_SCOPE),
            task=record.id,
            category=slugify(record.categories[0]) if record.categories else "",
            infer=self.reconcile,
            # Confined to this scope's note, so one crew's memory can never
            # supersede another's.
            scope="topic",
        )

    def _select(
        self,
        scope_prefix: str | None,
        categories: list[str] | None,
        record_ids: list[str] | None,
    ) -> list[dict[str, Any]]:
        if record_ids:
            hits: list[dict[str, Any]] = []
            for rid in record_ids:
                hits.extend(
                    _as_dict(f) for f in self.client.search("", task=rid, limit=50)
                    # The task filter is checked again here: a server that does
                    # not know it answers with every fact in scope, and the
                    # caller then deletes what it is handed.
                    if f.task == rid
                )
            return hits
        facts = self.client.search(
            "",
            path=note_for(scope_prefix) if scope_prefix else "",
            category=slugify(categories[0]) if categories else "",
            limit=1000,
        )
        return [_as_dict(f) for f in facts]

    def _record_of(self, hit: dict[str, Any]) -> MemoryRecord:
        payload = decode_record(hit.get("text", ""))
        return MemoryRecord(
            id=hit.get("task") or hit.get("id", ""),
            content=payload["content"],
            scope=scope_of(hit.get("path", "")),
            categories=payload.get("categories") or (
                [hit["category"]] if hit.get("category") else []
            ),
            metadata=payload.get("metadata") or {},
            created_at=_stamp(hit.get("stamp", "")),
        )


# ---- encoding ----------------------------------------------------------


def encode_record(record: MemoryRecord) -> str:
    """A record as the text of a fact.

    A record that is only content — the overwhelmingly common case for a crew
    recording what it learned — is written as that content, so the note stays
    something a person reads. Anything carrying metadata is JSON, so nothing is
    lost.
    """
    extra = {k: v for k, v in (record.metadata or {}).items()}
    if not extra and len(record.categories or []) <= 1:
        return record.content
    return json.dumps(
        {"content": record.content, "metadata": extra,
         "categories": list(record.categories or [])},
        separators=(",", ":"), sort_keys=True,
    )


def decode_record(text: str) -> dict[str, Any]:
    """The inverse of :func:`encode_record`."""
    stripped = text.strip()
    if stripped.startswith("{") and stripped.endswith("}"):
        try:
            parsed = json.loads(stripped)
        except json.JSONDecodeError:
            return {"content": text}
        if isinstance(parsed, dict) and "content" in parsed:
            return parsed
    return {"content": text}


def topic_for(scope: str) -> str:
    """A CrewAI scope path as one memory topic slug.

    An empty scope lands in the default note rather than in one called
    "untitled" — slugify's own fallback for empty input, which would be a
    confusing name for a crew's memory to appear under.
    """
    if not scope or not scope.strip():
        return DEFAULT_SCOPE
    return slugify(scope.replace("/", "-"))


def note_for(scope: str) -> str:
    return f"memory/{topic_for(scope)}.md"


def scope_of(path: str) -> str:
    return path.removeprefix("memory/").removesuffix(".md")


def _matches(metadata: dict[str, Any], wanted: dict[str, Any]) -> bool:
    return all(metadata.get(key) == value for key, value in wanted.items())


def _as_dict(fact) -> dict[str, Any]:
    return {
        "id": fact.id, "text": fact.text, "path": fact.path, "task": fact.task,
        "category": fact.category, "stamp": fact.stamp, "score": fact.score,
    }


def _stamp(stamp: str) -> datetime:
    for layout in ("%Y-%m-%d %H:%M", "%Y-%m-%d"):
        try:
            return datetime.strptime(stamp, layout).replace(tzinfo=UTC)
        except ValueError:
            continue
    return datetime.now(UTC)
