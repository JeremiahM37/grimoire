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
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Iterable, Mapping
from typing import TYPE_CHECKING, Any

from . import GrimoireError, NotFound, _error_for, _message_of

if TYPE_CHECKING:  # pragma: no cover
    from . import Grimoire

__all__ = ["AsyncBank", "Bank", "NotAvailable"]


class NotAvailable(NotFound):
    """The server does not have this bank feature (yet).

    Raised for the newer bank endpoints — reflect, observations, mental
    models, operations, webhooks, templates — when the server answers 404 for
    the route itself. A 404 for a missing bank is still :class:`NotFound`.
    """


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

        ``async_=True`` asks the server to queue the work and return an
        ``operation_id``. A server without the operations queue refuses that;
        the retain is then run synchronously and the response carries
        ``"async_fallback": True`` so the caller can tell.
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

    # ---- newer endpoints ----------------------------------------------
    #
    # Everything below targets routes a server may not have yet. Each call
    # raises NotAvailable when the route is missing, so a caller can hide the
    # feature instead of failing. Keep them together: this block is the one
    # place to align when those routes land.

    def reflect(
        self,
        query: str,
        *,
        budget: str | None = None,
        max_tokens: int | None = None,
        context: str | None = None,
        response_schema: Mapping[str, Any] | None = None,
        types: list[str] | None = None,
        tags: list[str] | None = None,
        tags_match: str | None = None,
        include_facts: bool = True,
        include_tool_calls: bool = False,
    ) -> dict[str, Any]:
        """Answer ``query`` from the bank, citing the facts it used."""
        body: dict[str, Any] = {"query": query}
        for key, value in (
            ("budget", budget), ("max_tokens", max_tokens), ("context", context),
            ("response_schema", dict(response_schema) if response_schema else None),
            ("types", types), ("tags", tags), ("tags_match", tags_match),
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
        return self._feature("POST", "/reflect", body)

    def observations(
        self, *, q: str | None = None, limit: int | None = None, offset: int | None = None
    ) -> dict[str, Any]:
        return self._feature("GET", "/observations" + _q({"q": q, "limit": limit, "offset": offset}))

    def consolidate(self) -> dict[str, Any]:
        return self._feature("POST", "/consolidate", {})

    def mental_models(self) -> dict[str, Any]:
        return self._feature("GET", "/mental-models")

    def mental_model(self, model_id: str) -> dict[str, Any]:
        return self._feature("GET", "/mental-models/" + _seg(model_id))

    def create_mental_model(
        self,
        name: str,
        source_query: str,
        *,
        id: str | None = None,
        tags: list[str] | None = None,
        max_tokens: int | None = None,
        refresh_after_consolidation: bool | None = None,
    ) -> dict[str, Any]:
        body: dict[str, Any] = {"name": name, "source_query": source_query, "tags": tags or []}
        if id:
            body["id"] = id
        if max_tokens is not None:
            body["max_tokens"] = max_tokens
        if refresh_after_consolidation is not None:
            body["trigger"] = {"refresh_after_consolidation": refresh_after_consolidation}
        return self._feature("POST", "/mental-models", body)

    def refresh_mental_model(self, model_id: str) -> dict[str, Any]:
        return self._feature("POST", "/mental-models/" + _seg(model_id) + "/refresh", {})

    def accept_proposal(self, model_id: str) -> dict[str, Any]:
        """Accept a refresh the server held back because a person edited the page."""
        return self._feature("POST", "/mental-models/" + _seg(model_id) + "/proposal/accept", {})

    def reject_proposal(self, model_id: str) -> dict[str, Any]:
        return self._feature("POST", "/mental-models/" + _seg(model_id) + "/proposal/reject", {})

    def delete_mental_model(self, model_id: str) -> dict[str, Any]:
        return self._feature("DELETE", "/mental-models/" + _seg(model_id))

    def operations(
        self,
        *,
        status: str | None = None,
        type: str | None = None,
        limit: int | None = None,
        offset: int | None = None,
    ) -> dict[str, Any]:
        return self._feature("GET", "/operations" + _q(
            {"status": status, "type": type, "limit": limit, "offset": offset}))

    def operation(self, operation_id: str) -> dict[str, Any]:
        return self._feature("GET", "/operations/" + _seg(operation_id))

    def cancel_operation(self, operation_id: str) -> dict[str, Any]:
        return self._feature("DELETE", "/operations/" + _seg(operation_id))

    def webhooks(self) -> dict[str, Any]:
        return self._feature("GET", "/webhooks")

    def create_webhook(
        self,
        url: str,
        *,
        secret: str | None = None,
        event_types: list[str] | None = None,
        enabled: bool = True,
    ) -> dict[str, Any]:
        body: dict[str, Any] = {"url": url, "enabled": enabled}
        if secret:
            body["secret"] = secret
        if event_types:
            body["event_types"] = event_types
        return self._feature("POST", "/webhooks", body)

    def update_webhook(self, webhook_id: str, **fields: Any) -> dict[str, Any]:
        return self._feature("PATCH", "/webhooks/" + _seg(webhook_id), fields)

    def delete_webhook(self, webhook_id: str) -> dict[str, Any]:
        return self._feature("DELETE", "/webhooks/" + _seg(webhook_id))

    # ---- transport -----------------------------------------------------

    def _req(self, method: str, path: str, body: Any = None) -> Any:
        return bank_request(self.client, method, f"/api/banks/{_seg(self.id)}{path}", body)

    def _feature(self, method: str, path: str, body: Any = None) -> Any:
        return feature_request(self.client, method, f"/api/banks/{_seg(self.id)}{path}", body)


def feature_request(client: Grimoire, method: str, path: str, body: Any = None) -> Any:
    """A call to a route the server may not have; 404 becomes NotAvailable.

    The message tells the two 404s apart: the router answers an unknown route
    with plain text "404 page not found"; the bank handlers answer a missing
    bank or item with a JSON ``detail``, which stays :class:`NotFound`.
    A 405 (the path exists for another method only) is also "not available".
    """
    try:
        return bank_request(client, method, path, body)
    except GrimoireError as exc:
        missing_route = isinstance(exc, NotFound) and "page not found" in exc.message.lower()
        if missing_route or exc.status == 405:
            raise NotAvailable(404, "not available on this server", exc.url) from None
        raise


def bank_request(client: Grimoire, method: str, path: str, body: Any = None) -> Any:
    """The client's transport, plus the agent header bank writes are stamped with."""
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
            raw = resp.read()
    except urllib.error.HTTPError as exc:
        raise _error_for(exc.code, _message_of(exc.read()), url) from None
    except urllib.error.URLError as exc:
        raise GrimoireError(0, f"cannot reach grimoire: {exc.reason}", url) from None
    if not raw:
        return None
    try:
        return json.loads(raw)
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
        """Bank templates the server offers (raises NotAvailable if none)."""
        out = feature_request(self.client, "GET", "/api/bank-templates")
        return (out or {}).get("templates", [])


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
