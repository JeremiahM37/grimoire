"""Grimoire client — talk to a Grimoire server from Python.

Zero dependencies, on purpose: the server is one static binary with no
runtime, and a client that drags in a stack to reach it undoes the property
people install it for. Everything here is stdlib ``urllib``.

The memory surface is the reason most people arrive, so it comes first and
carries mem0-compatible aliases (``add``/``search``/``get_all``/``delete``)
beside the native names. Switching a script over is meant to be an import
change, not a rewrite — with one behavioural difference worth knowing about:
:meth:`Grimoire.add` reconciles. A fact that contradicts one already on file
supersedes it, and a fact already recorded writes nothing. The result says
which happened.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass, field
from typing import Any, Iterable, Mapping

__all__ = [
    "Grimoire",
    "GrimoireError",
    "NotFound",
    "Unauthorized",
    "VaultLocked",
    "Memory",
    "Result",
]

DEFAULT_URL = "http://localhost:9111"
DEFAULT_TIMEOUT = 30.0


class GrimoireError(Exception):
    """A request the server refused."""

    def __init__(self, status: int, message: str, url: str = "") -> None:
        super().__init__(f"{status} {message}" + (f" ({url})" if url else ""))
        self.status = status
        self.message = message
        self.url = url


class NotFound(GrimoireError):
    """The note, fact or entry does not exist — or is not readable by you.

    The server deliberately does not distinguish those two: telling an
    unauthorized caller that something exists is itself a disclosure.
    """


class Unauthorized(GrimoireError):
    """No credential, or the wrong one."""


class VaultLocked(GrimoireError):
    """The credential vault is locked; a human has to unlock it."""


def _error_for(status: int, message: str, url: str) -> GrimoireError:
    if status == 404:
        return NotFound(status, message, url)
    if status in (401, 403):
        return Unauthorized(status, message, url)
    if status == 423:
        return VaultLocked(status, message, url)
    return GrimoireError(status, message, url)


@dataclass(frozen=True)
class Memory:
    """One remembered fact."""

    id: str
    text: str
    path: str = ""
    agent: str = ""
    task: str = ""
    session: str = ""
    category: str = ""
    stamp: str = ""
    expires: str = ""
    immutable: bool = False
    superseded_by: str = ""
    helpful: int = 0
    unhelpful: int = 0
    score: float = 0.0
    scores: Mapping[str, float] = field(default_factory=dict)

    @property
    def superseded(self) -> bool:
        """Whether a later fact replaced this one."""
        return bool(self.superseded_by)

    @classmethod
    def from_json(cls, raw: Mapping[str, Any]) -> "Memory":
        # Constructed field by field rather than with **raw: a server that
        # grows a field must not break a client that has not been updated.
        return cls(
            id=raw.get("id", ""),
            text=raw.get("text", ""),
            path=raw.get("path", ""),
            agent=raw.get("agent", ""),
            task=raw.get("task", ""),
            session=raw.get("session", ""),
            category=raw.get("category", ""),
            stamp=raw.get("stamp", ""),
            expires=raw.get("expires", ""),
            immutable=bool(raw.get("immutable", False)),
            superseded_by=raw.get("superseded_by", ""),
            helpful=int(raw.get("helpful", 0) or 0),
            unhelpful=int(raw.get("unhelpful", 0) or 0),
            score=float(raw.get("score", 0.0) or 0.0),
            scores=raw.get("scores") or {},
        )


@dataclass(frozen=True)
class Result:
    """What one remembered fact did to what was already known."""

    op: str
    id: str = ""
    text: str = ""
    target: str = ""
    path: str = ""
    why: str = ""

    @property
    def stored(self) -> bool:
        """Whether this wrote a new fact (as opposed to NOOP or DELETE)."""
        return self.op in ("ADD", "UPDATE")

    @classmethod
    def from_json(cls, raw: Mapping[str, Any]) -> "Result":
        return cls(
            op=raw.get("op", ""),
            id=raw.get("id", ""),
            text=raw.get("text", ""),
            target=raw.get("target", ""),
            path=raw.get("path", ""),
            why=raw.get("why", ""),
        )


class Grimoire:
    """A Grimoire server.

    >>> g = Grimoire("http://localhost:9111", token="...")
    >>> g.add("the user prefers tabs", topic="prefs")
    Result(op='ADD', ...)
    >>> [m.text for m in g.search("indentation")]
    ['the user prefers tabs']
    """

    def __init__(
        self,
        url: str = DEFAULT_URL,
        token: str | None = None,
        *,
        agent: str = "python-client",
        timeout: float = DEFAULT_TIMEOUT,
    ) -> None:
        self.url = url.rstrip("/")
        self.token = token
        self.agent = agent
        self.timeout = timeout

    # ---- memory -------------------------------------------------------

    def add(
        self,
        text: str,
        *,
        topic: str = "",
        agent: str | None = None,
        task: str = "",
        session: str = "",
        category: str = "",
        expires_in: str = "",
        expires: str = "",
        immutable: bool = False,
        infer: bool = True,
        scope: str = "",
    ) -> Result:
        """Record a fact, reconciled against what is already known.

        Returns the first :class:`Result`. One statement can assert several
        facts and each is reconciled separately, so use :meth:`remember` when
        you need all of them.

        ``scope`` bounds what this write may supersede — the default is the
        whole vault, ``"topic"`` / ``"session"`` / ``"agent"`` confine it.
        """
        return _first_result(
            self.remember(
                text,
                topic=topic,
                agent=agent,
                task=task,
                session=session,
                category=category,
                expires_in=expires_in,
                expires=expires,
                immutable=immutable,
                infer=infer,
                scope=scope,
            )
        )

    def remember(self, text: str, **kwargs: Any) -> dict[str, Any]:
        """:meth:`add`, returning the raw response including every result."""
        body: dict[str, Any] = {"text": text, "agent": kwargs.pop("agent", None) or self.agent}
        for key in (
            "topic", "task", "session", "category", "expires_in", "expires",
            "scope",
        ):
            value = kwargs.pop(key, "")
            if value:
                body[key] = value
        if kwargs.pop("immutable", False):
            body["immutable"] = True
        infer = kwargs.pop("infer", True)
        if infer is False:
            body["infer"] = False
        if kwargs:
            raise TypeError(f"unexpected arguments: {sorted(kwargs)}")
        return self._request("POST", "/api/memory", body)

    def add_many(self, items: Iterable[Mapping[str, Any]]) -> list[dict[str, Any]]:
        """Record several facts in one call.

        Each is reconciled against everything written before it, so a batch
        that contradicts itself ends with the last fact standing.
        """
        payload = []
        for item in items:
            entry = dict(item)
            entry.setdefault("agent", self.agent)
            payload.append(entry)
        out = self._request("POST", "/api/memory/batch", {"items": payload})
        return out.get("results", [])

    def search(
        self,
        query: str = "",
        *,
        limit: int = 10,
        agent: str = "",
        task: str = "",
        session: str = "",
        category: str = "",
        path: str = "",
        include_superseded: bool = False,
        include_expired: bool = False,
        as_of: str = "",
        explain: bool = False,
    ) -> list[Memory]:
        """Recall facts, most relevant first.

        Only what is currently believed, unless you ask otherwise:
        ``include_superseded`` also returns replaced beliefs, and ``as_of``
        (an RFC3339 instant) reconstructs what was believed then.
        """
        params: dict[str, str] = {"limit": str(limit)}
        if query:
            params["q"] = query
        for key, value in (
            ("agent", agent), ("task", task), ("session", session),
            ("category", category), ("path", path), ("as_of", as_of),
        ):
            if value:
                params[key] = value
        for key, flag in (
            ("include_superseded", include_superseded),
            ("include_expired", include_expired),
            ("explain", explain),
        ):
            if flag:
                params[key] = "1"
        raw = self._request("GET", "/api/memory?" + urllib.parse.urlencode(params))
        return [Memory.from_json(item) for item in raw or []]

    def get_all(self, **kwargs: Any) -> list[Memory]:
        """Every fact in scope, newest first. mem0-compatible alias."""
        kwargs.setdefault("limit", 200)
        return self.search("", **kwargs)

    def history(self, as_of: str, **kwargs: Any) -> list[Memory]:
        """What was believed at an instant.

        Answerable because a replaced fact is struck through rather than
        deleted — see the ``as_of`` note in the server docs.
        """
        return self.search(kwargs.pop("query", ""), as_of=as_of, **kwargs)

    def update(
        self,
        path: str,
        memory_id: str,
        *,
        text: str | None = None,
        category: str | None = None,
        expires: str | None = None,
        expires_in: str | None = None,
        immutable: bool | None = None,
    ) -> dict[str, Any]:
        """Edit one fact in place, leaving every other line of the note alone."""
        body: dict[str, Any] = {"path": path, "id": memory_id}
        for key, value in (
            ("text", text), ("category", category), ("expires", expires),
            ("expires_in", expires_in), ("immutable", immutable),
        ):
            if value is not None:
                body[key] = value
        return self._request("PATCH", "/api/memory/entry", body)

    def delete(self, path: str, memory_id: str, *, hard: bool = False) -> dict[str, Any]:
        """Retract one fact.

        By default it stops being recalled but stays in the note, struck
        through, so a person can see what was believed and undo it. ``hard``
        removes the line — still one rollback away until history rotates.
        """
        params = {"path": path, "id": memory_id, "agent": self.agent}
        if hard:
            params["hard"] = "1"
        return self._request(
            "DELETE", "/api/memory/entry?" + urllib.parse.urlencode(params)
        )

    def feedback(self, path: str, memory_id: str, *, helpful: bool) -> dict[str, Any]:
        """Report whether a recalled fact earned its place.

        A nudge in ranking, not a verdict: this cannot bury a fact that is the
        only answer to some other question. For a fact that is *wrong*, use
        :meth:`delete`.
        """
        return self._request("POST", "/api/memory/feedback",
                             {"path": path, "id": memory_id, "helpful": helpful})

    def graph(
        self,
        entity: str = "",
        *,
        depth: int = 1,
        limit: int = 50,
        agent: str = "",
        session: str = "",
        category: str = "",
    ) -> dict[str, Any]:
        """What memory knows about a thing, and what it is connected to.

        Returns ``nodes`` (entities, with how many facts mention each and how
        many hops from the seed), ``edges`` (pairs that share a fact, naming
        the facts), and ``entries`` — the facts themselves, so an edge can be
        read rather than trusted. With no ``entity``, the busiest entities.
        """
        params: dict[str, str] = {"depth": str(depth), "limit": str(limit)}
        for key, value in (
            ("entity", entity), ("agent", agent), ("session", session),
            ("category", category),
        ):
            if value:
                params[key] = value
        return self._request("GET", "/api/memory/graph?" + urllib.parse.urlencode(params))

    def scopes(self) -> dict[str, Any]:
        """The agents, sessions and categories memory has been recorded under."""
        return self._request("GET", "/api/memory/facets")

    def export(self, **filters: str) -> dict[str, Any]:
        """Every fact you may read, superseded and expired ones included."""
        query = urllib.parse.urlencode({k: v for k, v in filters.items() if v})
        return self._request("GET", "/api/memory/export" + (f"?{query}" if query else ""))

    def consolidate(self, *, topic: str = "", path: str = "") -> dict[str, Any]:
        """Compact the memory namespace. Snapshotted first, so it is reviewable."""
        body = {k: v for k, v in (("topic", topic), ("path", path)) if v}
        return self._request("POST", "/api/memory/consolidate", body)

    # ---- knowledge ----------------------------------------------------

    def ask(self, question: str, *, limit: int = 8) -> dict[str, Any]:
        """Ask the knowledge base, with citations."""
        return self._request("POST", "/api/ask", {"question": question, "k": limit})

    def search_notes(self, query: str, *, limit: int = 20) -> list[dict[str, Any]]:
        """Full-text search over notes."""
        params = urllib.parse.urlencode({"q": query, "limit": limit})
        return self._request("GET", "/api/search?" + params) or []

    def read_note(self, path: str) -> dict[str, Any]:
        """One note, with its frontmatter."""
        return self._request("GET", "/api/notes/" + urllib.parse.quote(path))

    def write_note(
        self, path: str, body: str, frontmatter: Mapping[str, Any] | None = None
    ) -> dict[str, Any]:
        """Create or replace a note."""
        payload: dict[str, Any] = {"body": body}
        if frontmatter:
            payload["frontmatter"] = dict(frontmatter)
        return self._request("PUT", "/api/notes/" + urllib.parse.quote(path), payload)

    def briefing(self, *, memories: int = 5) -> dict[str, Any]:
        """Standing context for an agent joining a session."""
        return self._request("GET", f"/api/briefing?memories={memories}")

    def get_fact(self, key: str, *, note: str = "") -> list[dict[str, Any]]:
        """Look up an exact recorded value rather than paraphrasing prose."""
        params = {"key": key}
        if note:
            params["note"] = note
        return self._request("GET", "/api/facts?" + urllib.parse.urlencode(params)) or []

    def health(self) -> dict[str, Any]:
        """Server liveness and corpus counts."""
        return self._request("GET", "/api/health")

    # ---- transport ----------------------------------------------------

    def _request(self, method: str, path: str, body: Any = None) -> Any:
        url = self.url + path
        data = None
        headers = {"Accept": "application/json"}
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        if self.token:
            headers["Authorization"] = f"Bearer {self.token}"
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
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
            # A body that is not JSON is a proxy or a captive portal answering
            # instead of the server; say so rather than raising a decode error
            # from somewhere deep in the caller's code.
            raise GrimoireError(0, "response was not json", url) from None


def _first_result(response: Mapping[str, Any]) -> Result:
    """The per-fact record for a write.

    ``results`` is the authoritative list — the top level repeats only enough
    of the first entry for a caller that wants one line back, so parsing the
    list is what keeps ``target`` and ``why`` from silently going missing.
    """
    results = response.get("results") or []
    if results:
        return Result.from_json(results[0])
    return Result.from_json(response)


def _message_of(payload: bytes) -> str:
    try:
        parsed = json.loads(payload)
    except (json.JSONDecodeError, ValueError):
        return payload.decode(errors="replace").strip() or "request failed"
    if isinstance(parsed, dict):
        for key in ("error", "message", "detail"):
            if parsed.get(key):
                return str(parsed[key])
    return str(parsed)
