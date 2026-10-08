"""Memory banks — retain raw content, recall it with a hybrid search.

A bank is a named, isolated memory an agent fills by handing over raw content
(a transcript, a document, a day's notes) and queries with ``recall``. The
server extracts the facts; what a person corrects in the files outranks what a
model extracted. See ``docs/MEMORY_BANKS.md`` in the repository.

>>> from grimoire_client import Grimoire
>>> bank = Grimoire("http://localhost:9111").bank("support")
>>> bank.retain("Dana moved the migration to May.", document_id="chat-1")
>>> [f["text"] for f in bank.recall("when is the migration?")["results"]]

Like the rest of the client this is stdlib only. :class:`AsyncBank` runs the
same calls on a worker thread so an asyncio program can await them without a
second HTTP stack.
"""

from __future__ import annotations

import asyncio
import json
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Iterable, Mapping
from typing import TYPE_CHECKING, Any

from . import GrimoireError, NotFound, _error_for, _message_of

if TYPE_CHECKING:  # pragma: no cover
    from . import Grimoire

__all__ = ["TERMINAL_STATUSES", "AsyncBank", "Bank", "ModelRequired", "NotAvailable"]

#: Operation statuses after which nothing more happens.
TERMINAL_STATUSES = frozenset({"completed", "failed", "cancelled"})


class NotAvailable(NotFound):
    """The server does not have this bank feature.

    Raised for the reasoning endpoints — reflect, observations, mental
    models, directives, operations, webhooks, templates — by a server from
    before them, which answers 404 for the route itself. A 404 for a missing
    bank is still :class:`NotFound`.
    """


class ModelRequired(GrimoireError):
    """The call needs a language model and the server has none configured
    (409 ``{"code": "model_required"}``): consolidation, a mental-model
    refresh. Reflect does not raise it; it answers extractively instead."""

    code = "model_required"


def _code_of(payload: bytes) -> str:
    try:
        parsed = json.loads(payload)
    except (json.JSONDecodeError, ValueError):
        return ""
    return str(parsed.get("code", "")) if isinstance(parsed, dict) else ""


def _q(params: Mapping[str, Any]) -> str:
    clean = {k: v for k, v in params.items() if v is not None and v != ""}
    for k, v in list(clean.items()):
        if isinstance(v, bool):
            clean[k] = "true" if v else "false"
    return ("?" + urllib.parse.urlencode(clean, doseq=True)) if clean else ""


def _seg(value: str) -> str:
    # A bank id, fact id or model id is ONE path segment: "/" is escaped, so a
    # mental model at "people/dana" stays one segment.
    return urllib.parse.quote(value, safe="")


def _iso(value: Any) -> Any:
    if value is None or isinstance(value, str):
        return value
    isoformat = getattr(value, "isoformat", None)
    return isoformat() if isoformat else str(value)


class Bank:
    """One memory bank on a Grimoire server."""

    def __init__(self, client: Grimoire, bank_id: str) -> None:
        self.client = client
        self.id = bank_id

    def __repr__(self) -> str:
        return f"Bank({self.id!r})"

    # ---- profile -------------------------------------------------------

    def profile(self) -> dict[str, Any]:
        """The bank's name, missions, disposition, directives and settings."""
        return self._req("GET", "")

    get = profile

    def update(self, **fields: Any) -> dict[str, Any]:
        """Patch the profile: ``name``, ``mission``, ``retain_mission``,
        ``disposition`` ({skepticism, literalism, empathy}, 1..5),
        ``directives`` ([{text, tags}]), ``tags``, ``config`` (merged; an empty
        value removes a setting)."""
        return self._req("PATCH", "", fields)

    def delete(self) -> dict[str, Any]:
        """Delete the bank and every file in it."""
        return self._req("DELETE", "")

    # ---- retain --------------------------------------------------------

    def retain(
        self,
        content: Any = None,
        *,
        items: Iterable[Mapping[str, Any]] | None = None,
        document_id: str | None = None,
        timestamp: Any = None,
        context: str | None = None,
        tags: list[str] | None = None,
        metadata: Mapping[str, Any] | None = None,
        entities: list[Mapping[str, Any]] | None = None,
        update_mode: str | None = None,
        document_tags: list[str] | None = None,
        mode: str | None = None,
        async_: bool = False,
    ) -> dict[str, Any]:
        """Hand the bank raw content to extract facts from.

        ``content`` is text, or a list of ``{speaker, text, timestamp}`` turns.
        Pass ``items`` instead for several sources in one call. The same
        ``document_id`` again updates that document (only changed chunks are
        re-extracted). ``mode="chunks"`` stores each chunk as a fact with no
        model call.

        ``async_=True`` queues the work: the server answers at once with
        ``{"async": True, "operation_id", "operation_ids", "items_count"}``
        (one operation per 500 items); :meth:`wait_operation` follows it. A
        server without the operations queue refuses that; the retain is then
        run synchronously and the response carries ``"async_fallback": True``
        so the caller can tell.
        """
        if items is None:
            if content is None:
                raise ValueError("retain needs content or items")
            item: dict[str, Any] = {"content": content}
            for key, value in (
                ("document_id", document_id),
                ("timestamp", _iso(timestamp)),
                ("context", context),
                ("tags", tags),
                ("metadata", dict(metadata) if metadata else None),
                ("entities", entities),
                ("update_mode", update_mode),
            ):
                if value is not None:
                    item[key] = value
            payload_items = [item]
        else:
            payload_items = [dict(i) for i in items]
            for i in payload_items:
                if "timestamp" in i:
                    i["timestamp"] = _iso(i["timestamp"])
        body: dict[str, Any] = {"items": payload_items}
        if document_tags:
            body["document_tags"] = document_tags
        if mode:
            body["mode"] = mode
        if not async_:
            return self._req("POST", "/memories", body)
        try:
            return self._req("POST", "/memories", {**body, "async": True})
        except GrimoireError as exc:
            if exc.status != 400 or "async" not in exc.message.lower():
                raise
        out = self._req("POST", "/memories", body)
        if isinstance(out, dict):
            out["async_fallback"] = True
        return out

    # ---- recall --------------------------------------------------------

    def recall(
        self,
        query: str,
        *,
        budget: str | None = None,
        max_tokens: int | None = None,
        types: list[str] | None = None,
        tags: list[str] | None = None,
        tags_match: str | None = None,
        query_timestamp: Any = None,
        include_entities: bool = True,
        include_chunks: bool | int = False,
        include_source_facts: bool = False,
        trace: bool = False,
    ) -> dict[str, Any]:
        """Recall facts relevant to ``query``, packed into ``max_tokens``.

        Returns the server's response: ``results`` (each with ``authority``
        human/agent and ``disputed_by`` when a person's fact contradicts it),
        plus ``entities``, ``chunks`` and ``trace`` when asked for.
        ``include_chunks`` may be an int: the token budget for source chunks.
        """
        body: dict[str, Any] = {"query": query}
        for key, value in (
            ("budget", budget),
            ("max_tokens", max_tokens),
            ("types", types),
            ("tags", tags),
            ("tags_match", tags_match),
            ("query_timestamp", _iso(query_timestamp)),
        ):
            if value is not None:
                body[key] = value
        include: dict[str, Any] = {}
        if not include_entities:
            include["entities"] = None
        if include_chunks:
            include["chunks"] = (
                {"max_tokens": include_chunks}
                if isinstance(include_chunks, int) and not isinstance(include_chunks, bool)
                else {}
            )
        if include_source_facts:
            include["source_facts"] = {}
        if include:
            body["include"] = include
        if trace:
            body["trace"] = True
        return self._req("POST", "/memories/recall", body)

    # ---- browse --------------------------------------------------------

    def list_memories(
        self,
        *,
        q: str | None = None,
        type: str | None = None,
        document_id: str | None = None,
        authority: str | None = None,
        limit: int | None = None,
        offset: int | None = None,
    ) -> dict[str, Any]:
        """Facts in file order: ``{"items": [...], "total": n}``."""
        return self._req("GET", "/memories" + _q({
            "q": q, "type": type, "document_id": document_id,
            "authority": authority, "limit": limit, "offset": offset,
        }))

    def memory(self, fact_id: str) -> dict[str, Any]:
        return self._req("GET", "/memories/" + _seg(fact_id))

    def delete_memory(self, fact_id: str, *, force: bool = False) -> dict[str, Any]:
        """Delete one fact. A fact a person wrote needs ``force=True``."""
        return self._req("DELETE", "/memories/" + _seg(fact_id) + _q({"force": force or None}))

    def entities(self, *, q: str | None = None, limit: int | None = None) -> list[dict[str, Any]]:
        out = self._req("GET", "/entities" + _q({"q": q, "limit": limit}))
        return (out or {}).get("items", [])

    def entity(self, id_or_name: str, *, limit: int | None = None) -> dict[str, Any]:
        """One entity with the facts that mention it and its companions."""
        return self._req("GET", "/entities/" + _seg(id_or_name) + _q({"limit": limit}))

    def documents(self, *, limit: int | None = None, offset: int | None = None) -> dict[str, Any]:
        return self._req("GET", "/documents" + _q({"limit": limit, "offset": offset}))

    def document(self, document_id: str) -> dict[str, Any]:
        return self._req("GET", "/documents/" + _seg(document_id))

    def delete_document(self, document_id: str, *, force: bool = False) -> dict[str, Any]:
        """Delete a source and the model's facts from it. A person's facts are
        kept (marked ``doc_removed``) unless ``force=True``."""
        return self._req("DELETE", "/documents/" + _seg(document_id) + _q({"force": force or None}))

    def chunk(self, chunk_id: str) -> dict[str, Any]:
        return self._req("GET", "/chunks/" + _seg(chunk_id))

    # ---- coding-agent surfaces ----------------------------------------

    def context(self, *, max_chars: int | None = None, source: str | None = None) -> dict[str, Any]:
        """What a coding agent is shown at session start, rendered under
        ``max_chars`` (default 9000): ``{"context", "chars", "limit", "included", "dropped"}``."""
        return self._req("GET", "/context" + _q({"max_chars": max_chars, "source": source}))

    def index(
        self,
        query: str | None = None,
        *,
        types: list[str] | None = None,
        since: str | None = None,
        limit: int | None = None,
        offset: int | None = None,
    ) -> dict[str, Any]:
        """A compact, citable index: ``{"items": [{ref, id, type, title, date, tokens}], "total"}``,
        newest first, or ranked by ``query``. Fetch the few you need with :meth:`get`."""
        return self._req("GET", "/index" + _q({
            "q": query, "types": ",".join(types) if types else None, "since": since,
            "limit": limit, "offset": offset,
        }))

    def timeline(self, anchor: str, *, before: int | None = None, after: int | None = None) -> dict[str, Any]:
        """The entries dated around ``anchor`` (a ``#ref`` or ``YYYY-MM-DD``), oldest first."""
        return self._req("GET", "/timeline" + _q({"anchor": anchor, "before": before, "after": after}))

    def file_memory(self, path: str, *, limit: int | None = None) -> dict[str, Any]:
        """What the bank remembers about one file: ``{"path", "items": [{kind, text, date, human}]}``."""
        return self._req("GET", "/file-memory" + _q({"path": path, "limit": limit}))

    def duplicates(self, *, min_score: float | None = None, type: str | None = None,
                   limit: int | None = None) -> dict[str, Any]:
        """Near-duplicate candidates: ``{"candidates": [{type, score, keep, merge, shared}]}``."""
        return self._req("GET", "/duplicates" + _q({"min_score": min_score, "type": type, "limit": limit}))

    def merge_duplicates(self, keep: str, merge: str) -> dict[str, Any]:
        """Strike ``merge`` through into ``keep``; its text is kept, never deleted."""
        return self._req("POST", "/duplicates/merge", {"keep": keep, "merge": merge})

    def get_entries(self, ids: list[str]) -> dict[str, Any]:
        """Entries in full by ``#ref`` or id: ``{"items": [...], "missing": [...]}``."""
        return self._req("GET", "/lookup" + _q({"ids": ",".join(ids)}))

    def write_digest(
        self,
        session_id: str,
        turns: list[dict[str, Any]],
        *,
        activity: dict[str, Any] | None = None,
        use_model: bool = False,
    ) -> dict[str, Any]:
        """Write a session's "where we left off" note. A person's edit to the
        generated text pins it: the reply then has ``pinned: true``."""
        return self._req("POST", "/sessions/" + _seg(session_id) + "/digest", {
            "turns": turns, "activity": activity or {}, "use_model": use_model})

    def sessions(self, *, limit: int | None = None) -> dict[str, Any]:
        """Session digests, newest first."""
        return self._req("GET", "/sessions" + _q({"limit": limit}))

    # ---- reasoning endpoints ------------------------------------------
    #
    # Reflect, observations, mental models, directives, operations, webhooks
    # and templates. A server from before these routes answers them with the
    # router's plain 404; each call turns that into NotAvailable, so a caller
    # can hide the feature instead of failing. A call that needs a language
    # model the server does not have raises ModelRequired.

    def stats(self) -> dict[str, Any]:
        """Counts: facts by type, observations, documents, entities, mental
        models, pending consolidation, operations by status, and whether a
        model is configured (``model_available``)."""
        return self._feature("GET", "/stats")

    def reflect(
        self,
        query: str,
        *,
        budget: str | None = None,
        max_tokens: int | None = None,
        context: str | None = None,
        response_schema: Mapping[str, Any] | None = None,
        fact_types: list[str] | None = None,
        types: list[str] | None = None,
        tags: list[str] | None = None,
        tags_match: str | None = None,
        tag_groups: list[Mapping[str, Any]] | None = None,
        apply_all_directives: bool | None = None,
        exclude_mental_models: bool | None = None,
        exclude_mental_model_ids: list[str] | None = None,
        query_timestamp: Any = None,
        include_facts: bool = True,
        include_tool_calls: bool = False,
        trace: bool = False,
    ) -> dict[str, Any]:
        """Answer ``query`` by reasoning over the bank.

        The response always carries ``text``, ``mode`` (``llm``, or
        ``extractive`` when the server has no model) and ``based_on``
        (``memories``, ``observations``, ``mental_models``, ``directives``).
        ``trace`` (the tool calls) is returned only with
        ``include_tool_calls=True`` or ``trace=True``. ``types`` is accepted as
        an alias of ``fact_types``.
        """
        body: dict[str, Any] = {"query": query}
        for key, value in (
            ("budget", budget), ("max_tokens", max_tokens), ("context", context),
            ("response_schema", dict(response_schema) if response_schema else None),
            ("fact_types", fact_types if fact_types is not None else types),
            ("tags", tags), ("tags_match", tags_match),
            ("tag_groups", [dict(g) for g in tag_groups] if tag_groups else None),
            ("apply_all_directives", apply_all_directives),
            ("exclude_mental_models", exclude_mental_models),
            ("exclude_mental_model_ids", exclude_mental_model_ids),
            ("query_timestamp", _iso(query_timestamp)),
        ):
            if value is not None:
                body[key] = value
        include: dict[str, Any] = {}
        if include_facts:
            include["facts"] = {}
        if include_tool_calls:
            include["tool_calls"] = {}
        if include:
            body["include"] = include
        if trace:
            body["trace"] = True
        return self._feature("POST", "/reflect", body)

    # observations and consolidation

    def observations(
        self,
        *,
        q: str | None = None,
        authority: str | None = None,
        tags: list[str] | None = None,
        tags_match: str | None = None,
        include_history: bool = False,
        limit: int | None = None,
        offset: int | None = None,
    ) -> dict[str, Any]:
        """``{"items": [...], "total": n}``, plus ``history`` with
        ``include_history=True``."""
        return self._feature("GET", "/observations" + _q({
            "q": q, "authority": authority, "tags": ",".join(tags) if tags else None,
            "tags_match": tags_match, "include_history": include_history or None,
            "limit": limit, "offset": offset}))

    def observation(self, observation_id: str) -> dict[str, Any]:
        """``{"observation": {...}, "history": [...]}``."""
        return self._feature("GET", "/observations/" + _seg(observation_id))

    def update_observation(self, observation_id: str, text: str) -> dict[str, Any]:
        """Replace an observation's text. The edit is marked human, so consolidation keeps it."""
        return self._feature("PATCH", "/observations/" + _seg(observation_id), {"text": text})

    def delete_observation(self, observation_id: str, *, force: bool = False) -> dict[str, Any]:
        """Retire one observation into history. A person's needs ``force=True``."""
        return self._feature("DELETE", "/observations/" + _seg(observation_id) + _q({"force": force or None}))

    def clear_observations(self) -> dict[str, Any]:
        """Retire every model observation (a person's stay): ``{"retired": n}``."""
        return self._feature("DELETE", "/observations")

    def consolidate(self) -> dict[str, Any]:
        """Queue a consolidation: ``{"operation_id", "deduplicated"}``.
        Raises ModelRequired when the server has no model."""
        return self._feature("POST", "/consolidate", {})

    # mental models

    def mental_models(
        self,
        *,
        tags: list[str] | None = None,
        tags_match: str | None = None,
        folder: str | None = None,
        detail: bool = False,
    ) -> dict[str, Any]:
        """``{"items": [...], "total": n}``; bodies only with ``detail=True``."""
        return self._feature("GET", "/mental-models" + _q({
            "tags": ",".join(tags) if tags else None, "tags_match": tags_match,
            "folder": folder, "detail": "full" if detail else None}))

    def mental_model(self, model_id: str) -> dict[str, Any]:
        """One model with its ``body``, ``authority``, ``is_stale`` and any
        ``pending_proposal``."""
        return self._feature("GET", "/mental-models/" + _seg(model_id))

    def create_mental_model(
        self,
        name: str,
        question: str | None = None,
        *,
        id: str | None = None,
        folder: str | None = None,
        tags: list[str] | None = None,
        refresh: str | None = None,
        max_tokens: int | None = None,
        budget: str | None = None,
        fact_types: list[str] | None = None,
        body: str | None = None,
        source_query: str | None = None,
        refresh_after_consolidation: bool | None = None,
    ) -> dict[str, Any]:
        """Create a standing question; returns ``{"mental_model",
        "mental_model_id", "operation_id"}``. ``operation_id`` is the first
        refresh, or ``None`` when ``body`` was given or the server has no model.
        ``id`` is the whole path (``"people/dana"``); without one the id is
        ``folder`` plus a slug of ``name``.
        ``source_query`` and ``refresh_after_consolidation`` are accepted for
        older callers (``refresh="auto"``/``"manual"``)."""
        question = question if question is not None else source_query
        if not question:
            raise ValueError("create_mental_model needs a question")
        if refresh is None and refresh_after_consolidation is not None:
            refresh = "auto" if refresh_after_consolidation else "manual"
        out: dict[str, Any] = {"name": name, "question": question}
        for key, value in (
            ("id", id), ("folder", folder), ("tags", tags), ("refresh", refresh),
            ("max_tokens", max_tokens), ("budget", budget), ("fact_types", fact_types),
            ("body", body),
        ):
            if value is not None:
                out[key] = value
        return self._feature("POST", "/mental-models", out)

    def update_mental_model(self, model_id: str, **fields: Any) -> dict[str, Any]:
        """Patch any create field. ``body`` is a person's edit (later refreshes
        file a proposal instead of overwriting it); ``folder`` moves the model,
        which changes its id — use the returned model's ``id``."""
        return self._feature("PATCH", "/mental-models/" + _seg(model_id), fields)

    def move_mental_model(self, model_id: str, folder: str) -> dict[str, Any]:
        """Move a model to ``folder`` (``""`` for the top). Returns the model
        under its new id."""
        return self.update_mental_model(model_id, folder=folder)

    def delete_mental_model(self, model_id: str) -> dict[str, Any]:
        return self._feature("DELETE", "/mental-models/" + _seg(model_id))

    def refresh_mental_model(self, model_id: str, *, mode: str | None = None) -> dict[str, Any]:
        """Queue a refresh: ``{"operation_id", "status", "deduplicated"}``.
        ``mode="delta"`` edits only the sections new facts touch (a person's
        sections are never changed); ``"full"`` rewrites. Default: the model's
        own setting. Raises ModelRequired when a full refresh has no server model."""
        return self._feature("POST", "/mental-models/" + _seg(model_id) + "/refresh",
                             {"mode": mode} if mode else {})

    def accept_proposal(self, model_id: str) -> dict[str, Any]:
        """Make the pending proposal the answer; returns the model."""
        return self._feature("POST", "/mental-models/" + _seg(model_id) + "/proposal/accept", {})

    def reject_proposal(self, model_id: str) -> dict[str, Any]:
        """Discard the pending proposal, keeping the person's text."""
        return self._feature("POST", "/mental-models/" + _seg(model_id) + "/proposal/reject", {})

    def mental_model_history(self, model_id: str, version: str | None = None) -> dict[str, Any]:
        """The model's versions, or one version's ``content``."""
        path = "/mental-models/" + _seg(model_id) + "/history"
        if version is not None:
            path += "/" + _seg(str(version))
        return self._feature("GET", path)

    def mental_model_tree(self, *, folder: str | None = None) -> list[dict[str, Any]]:
        """The knowledge-page tree: nodes ``{kind: folder|page, name, path,
        model?, children?}``."""
        out = self._feature("GET", "/mental-models-tree" + _q({"folder": folder}))
        return (out or {}).get("roots", [])

    def export_mental_models(self, *, markdown: bool = False) -> Any:
        """``[{path, content}]`` (an ``index.md`` plus one file per page), or
        with ``markdown=True`` one markdown document as a string."""
        if markdown:
            return self._feature("GET", "/mental-models-export?format=markdown", raw=True)
        out = self._feature("GET", "/mental-models-export")
        return (out or {}).get("files", [])

    # directives

    def directives(self, *, tags: list[str] | None = None, active_only: bool = False) -> dict[str, Any]:
        """``{"items": [...], "total": n}``."""
        return self._feature("GET", "/directives" + _q({
            "tags": ",".join(tags) if tags else None, "active_only": active_only}))

    def create_directive(
        self,
        text: str,
        *,
        name: str | None = None,
        tags: list[str] | None = None,
        priority: int | None = None,
        is_active: bool | None = None,
    ) -> dict[str, Any]:
        body: dict[str, Any] = {"text": text}
        for key, value in (("name", name), ("tags", tags), ("priority", priority), ("is_active", is_active)):
            if value is not None:
                body[key] = value
        return self._feature("POST", "/directives", body)

    def update_directive(self, directive_id: str, **fields: Any) -> dict[str, Any]:
        """Patch ``text``, ``name``, ``tags``, ``priority`` or ``is_active``."""
        return self._feature("PATCH", "/directives/" + _seg(directive_id), fields)

    def delete_directive(self, directive_id: str) -> dict[str, Any]:
        return self._feature("DELETE", "/directives/" + _seg(directive_id))

    # operations

    def operations(
        self,
        *,
        status: str | None = None,
        type: str | None = None,
        limit: int | None = None,
        offset: int | None = None,
    ) -> dict[str, Any]:
        """``{"bank_id", "operations": [...], "total"}``, newest first. Each
        operation has ``kind`` (= ``type``: retain, consolidation,
        refresh_mental_model) and ``status`` (queued, running, completed,
        failed, cancelled)."""
        return self._feature("GET", "/operations" + _q(
            {"status": status, "type": type, "limit": limit, "offset": offset}))

    def operation(self, operation_id: str) -> dict[str, Any]:
        """One operation with its ``payload`` and ``result``."""
        return self._feature("GET", "/operations/" + _seg(operation_id))

    def cancel_operation(self, operation_id: str) -> dict[str, Any]:
        return self._feature("DELETE", "/operations/" + _seg(operation_id))

    def wait_operation(
        self, operation_id: str, *, timeout: float = 120.0, interval: float = 0.5
    ) -> dict[str, Any]:
        """Poll an operation until it is completed, failed or cancelled, and
        return it. Raises TimeoutError if it is still going after ``timeout``
        seconds. A failed operation is returned, not raised: read ``error``."""
        deadline = time.monotonic() + timeout
        while True:
            op = self.operation(operation_id)
            if op.get("status") in TERMINAL_STATUSES:
                return op
            if time.monotonic() >= deadline:
                raise TimeoutError(f"operation {operation_id} is still {op.get('status')}")
            time.sleep(interval)

    # webhooks

    def webhooks(self) -> dict[str, Any]:
        """``{"items": [...], "total": n}``."""
        return self._feature("GET", "/webhooks")

    def create_webhook(
        self,
        url: str,
        *,
        secret: str | None = None,
        events: list[str] | None = None,
        event_types: list[str] | None = None,
        enabled: bool = True,
    ) -> dict[str, Any]:
        """Register a webhook. The secret (generated when not given) is in the
        response this once."""
        body: dict[str, Any] = {"url": url, "enabled": enabled}
        if secret:
            body["secret"] = secret
        if events or event_types:
            body["events"] = events or event_types
        return self._feature("POST", "/webhooks", body)

    def update_webhook(self, webhook_id: str, **fields: Any) -> dict[str, Any]:
        return self._feature("PATCH", "/webhooks/" + _seg(webhook_id), fields)

    def delete_webhook(self, webhook_id: str) -> dict[str, Any]:
        return self._feature("DELETE", "/webhooks/" + _seg(webhook_id))

    def webhook_deliveries(self, webhook_id: str, *, limit: int | None = None) -> list[dict[str, Any]]:
        out = self._feature("GET", "/webhooks/" + _seg(webhook_id) + "/deliveries" + _q({"limit": limit}))
        return (out or {}).get("items", [])

    # templates

    def export(self) -> dict[str, Any]:
        """The bank's configuration as a template manifest."""
        return self._feature("GET", "/export")

    def import_template(
        self,
        manifest: Mapping[str, Any] | None = None,
        *,
        template: str | None = None,
        dry_run: bool = False,
    ) -> dict[str, Any]:
        """Apply a manifest, or a built-in template by id, creating the bank
        if needed. Additive: nothing is removed."""
        if (manifest is None) == (template is None):
            raise ValueError("pass a manifest or a template id")
        body = {"template": template} if template is not None else {"manifest": dict(manifest or {})}
        return self._feature("POST", "/import" + _q({"dry_run": dry_run or None}), body)

    # ---- transport -----------------------------------------------------

    def _req(self, method: str, path: str, body: Any = None) -> Any:
        return bank_request(self.client, method, f"/api/banks/{_seg(self.id)}{path}", body)

    def _feature(self, method: str, path: str, body: Any = None, *, raw: bool = False) -> Any:
        return feature_request(self.client, method, f"/api/banks/{_seg(self.id)}{path}", body, raw=raw)


def feature_request(client: Grimoire, method: str, path: str, body: Any = None, *, raw: bool = False) -> Any:
    """A call to a route the server may not have; 404 becomes NotAvailable.

    The message tells the two 404s apart: the router answers an unknown route
    with plain text "404 page not found"; the bank handlers answer a missing
    bank or item with a JSON ``detail``, which stays :class:`NotFound`.
    A 405 (the path exists for another method only) is also "not available".
    """
    try:
        return bank_request(client, method, path, body, raw=raw)
    except GrimoireError as exc:
        missing_route = isinstance(exc, NotFound) and "page not found" in exc.message.lower()
        if missing_route or exc.status == 405:
            raise NotAvailable(404, "not available on this server", exc.url) from None
        raise


def bank_request(client: Grimoire, method: str, path: str, body: Any = None, *, raw: bool = False) -> Any:
    """The client's transport, plus the agent header bank writes are stamped with.

    ``raw=True`` returns the body as text instead of parsing JSON."""
    url = client.url + path
    data = None
    headers = {"Accept": "application/json"}
    if getattr(client, "agent", ""):
        headers["X-Grimoire-Agent"] = client.agent
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    if client.token:
        headers["Authorization"] = f"Bearer {client.token}"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=client.timeout) as resp:
            payload = resp.read()
    except urllib.error.HTTPError as exc:
        err = exc.read()
        message = _message_of(err)
        if exc.code == 409 and _code_of(err) == "model_required":
            raise ModelRequired(exc.code, message, url) from None
        raise _error_for(exc.code, message, url) from None
    except urllib.error.URLError as exc:
        raise GrimoireError(0, f"cannot reach grimoire: {exc.reason}", url) from None
    if raw:
        return payload.decode("utf-8", "replace")
    if not payload:
        return None
    try:
        return json.loads(payload)
    except json.JSONDecodeError:
        raise GrimoireError(0, "response was not json", url) from None


class Banks:
    """The bank collection: ``g.banks.list()``, ``g.banks.create(...)``."""

    def __init__(self, client: Grimoire) -> None:
        self.client = client

    def __call__(self, bank_id: str) -> Bank:
        return Bank(self.client, bank_id)

    def list(self) -> list[dict[str, Any]]:
        out = bank_request(self.client, "GET", "/api/banks")
        return (out or {}).get("banks", [])

    def create(self, bank_id: str, **fields: Any) -> dict[str, Any]:
        """Create a bank. Fields: ``name``, ``mission``, ``retain_mission``,
        ``disposition``, ``directives``, ``tags``, ``config``. A bank is also
        created by its first retain."""
        return bank_request(self.client, "POST", "/api/banks", {"bank_id": bank_id, **fields})

    def templates(self) -> list[dict[str, Any]]:
        """Built-in bank templates, ``[{id, name, description, manifest}]``
        (raises NotAvailable on a server without them). On a server with
        accounts this needs a signed-in user."""
        out = feature_request(self.client, "GET", "/api/bank-templates")
        return (out or {}).get("templates", [])

    def template(self, template_id: str) -> dict[str, Any]:
        return feature_request(self.client, "GET", "/api/bank-templates/" + _seg(template_id))


class AsyncBank:
    """:class:`Bank` for asyncio: every method is awaitable.

    Calls run on a worker thread (``asyncio.to_thread``), so there is no second
    HTTP stack to install and the behaviour is exactly the sync client's.
    """

    def __init__(self, client: Grimoire, bank_id: str) -> None:
        self.sync = Bank(client, bank_id)
        self.id = bank_id

    def __getattr__(self, name: str) -> Any:
        target = getattr(self.sync, name)
        if not callable(target):
            return target

        async def call(*args: Any, **kwargs: Any) -> Any:
            return await asyncio.to_thread(target, *args, **kwargs)

        call.__name__ = name
        call.__doc__ = target.__doc__
        return call
