"""Pre-request context for any host that can call a function before a model.

The native Claude Code / Codex hook (``clients/hooks/grimoire_context.py``)
injects bounded, model-free context ahead of each prompt. A plain chatbot, a
custom agent loop or a web handler has no hook, but it can call this::

    from grimoire_client import Grimoire
    from grimoire_client.context import context_for

    reference = context_for("how does kestrel deployment work?",
                            client=Grimoire("http://localhost:9111"))
    system = base_prompt + ("\\n\\n" + reference if reference else "")

It is the same read path the hook uses (``GET /api/memory/context``, lexical,
no model calls) with the same response validation, so the two behave alike.
The hook is a standalone script that is not part of this package, so its rules
are mirrored here rather than imported; ``tests/test_context.py`` pins them.

It never raises: a trivial prompt, an unreachable server, an older server
without the endpoint or a malformed reply all return ``""``. Injecting context
cannot make a model use it — see ``docs/INTEGRATIONS.md``.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable, Iterable

from . import DEFAULT_URL, Grimoire

__all__ = ["ContextSession", "context_for"]

DEFAULT_BUDGET = 2400
FETCH_TIMEOUT = 1.5
REPEAT_SECONDS = 30
SEEN_SECONDS = 1800
MAX_SEEN = 256
MAX_PROMPT_BYTES = 8000
MAX_RESPONSE_BYTES = 64000

_ACKNOWLEDGEMENT = re.compile(
    r"(?:thanks?|thank you|ok(?:ay)?|yes|no|continue|proceed|hi|hello)[.!\s]*",
    re.IGNORECASE,
)
_KEY = re.compile(r"[a-f0-9]{32}")


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def _fingerprint(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


def _default_client() -> Grimoire:
    return Grimoire(
        os.environ.get("GRIMOIRE_URL", DEFAULT_URL),
        token=os.environ.get("GRIMOIRE_AUTH_TOKEN") or None,
        agent="context",
    )


def _clean_prompt(prompt: object) -> str:
    """The prompt to look up, or ``""`` when it is not worth a request."""
    if not isinstance(prompt, str) or len(prompt.encode()) > MAX_PROMPT_BYTES:
        return ""
    prompt = prompt.strip()
    if not prompt or _ACKNOWLEDGEMENT.fullmatch(prompt):
        return ""
    return prompt


def _fetch(client: Grimoire, query: str, excluded: list[str], budget: int,
           mode: str, paths: list[str]) -> dict:
    parameters = urllib.parse.urlencode({
        "q": query, "exclude": ",".join(excluded), "max_bytes": budget, "limit": 5,
        "scope": mode, "path": paths,
    }, doseq=True)
    headers = {"Accept": "application/json"}
    if client.token:
        headers["Authorization"] = "Bearer " + client.token
    request = urllib.request.Request(
        client.url + "/api/memory/context?" + parameters, headers=headers)
    # No ambient proxy and no redirects, as in the hook: a bearer token should
    # only ever travel to the address it was configured for.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), _NoRedirect())
    with opener.open(request, timeout=FETCH_TIMEOUT) as response:
        raw = response.read(MAX_RESPONSE_BYTES + 1)
    if len(raw) > MAX_RESPONSE_BYTES:
        raise ValueError("oversized context response")
    result = json.loads(raw)
    if not isinstance(result, dict):
        raise ValueError("invalid context response")
    return result


def _lookup(prompt: object, client: Grimoire | None, budget: int, mode: str | None,
            paths: Iterable[str] | None, exclude: Iterable[str] | None,
            fetch: Callable = _fetch) -> tuple[str | None, list[str]]:
    """``(context, keys)``; ``None`` context when no lookup succeeded."""
    query = _clean_prompt(prompt)
    if not query:
        return None, []
    paths = list(paths or [])
    mode = mode or ("scoped" if paths else "all")
    # The same scope rules as the hook: scoped needs paths, all takes none, and
    # an empty scope never widens to everything.
    if (mode not in {"all", "scoped"} or len(paths) > 32
            or any(not isinstance(p, str) or len(p) > 512 for p in paths)
            or (mode == "scoped") != bool(paths)):
        return None, []
    budget = max(128, min(8000, int(budget)))
    excluded = [k for k in (exclude or []) if isinstance(k, str)][-MAX_SEEN:]
    try:
        result = fetch(client or _default_client(), query, excluded, budget, mode, paths)
    except (OSError, ValueError, urllib.error.URLError):
        return None, []
    context = result.get("context", "")
    keys = result.get("keys", [])
    if (not isinstance(context, str) or len(context.encode()) > budget
            or not isinstance(keys, list) or len(keys) > 10
            or any(not isinstance(k, str) or not _KEY.fullmatch(k) for k in keys)):
        return None, []
    return context, keys


def context_for(
    prompt: str,
    *,
    client: Grimoire | None = None,
    budget: int = DEFAULT_BUDGET,
    mode: str | None = None,
    paths: Iterable[str] | None = None,
    exclude: Iterable[str] | None = None,
) -> str:
    """Bounded reference context for ``prompt``, or ``""``.

    ``mode`` is ``"all"`` (whole readable corpus) or ``"scoped"`` (only
    ``paths``, vault-relative files or ``dir/`` prefixes); by default it is
    scoped when ``paths`` are given and ``all`` otherwise. ``exclude`` lists
    fact keys already shown. ``budget`` is a UTF-8 byte ceiling (128-8000).
    Stateless — use :class:`ContextSession` to deduplicate across turns.
    ``client`` defaults to ``GRIMOIRE_URL`` / ``GRIMOIRE_AUTH_TOKEN``.
    """
    return _lookup(prompt, client, budget, mode, paths, exclude)[0] or ""


class ContextSession:
    """Per-conversation deduplication with the hook's rules.

    Skips acknowledgements, an identical query repeated within 30 seconds, and
    facts already returned within the last 30 minutes. State is in memory only
    (hashes and timestamps, never note bodies); call :meth:`reset` after a
    compaction or when the conversation is cleared.
    """

    def __init__(
        self,
        client: Grimoire | None = None,
        *,
        budget: int = DEFAULT_BUDGET,
        mode: str | None = None,
        paths: Iterable[str] | None = None,
        clock: Callable[[], float] = time.time,
    ) -> None:
        self.client = client
        self.budget = budget
        self.mode = mode
        self.paths = list(paths or [])
        self._clock = clock
        self.reset()

    def reset(self) -> None:
        self._seen: dict[str, float] = {}
        self._query = ""
        self._checked = 0.0

    def context_for(self, prompt: str) -> str:
        now = self._clock()
        query = _clean_prompt(prompt)
        if not query:
            return ""
        digest = _fingerprint(query)
        if self._query == digest and now - self._checked < REPEAT_SECONDS:
            return ""
        self._seen = {k: t for k, t in self._seen.items() if now - t < SEEN_SECONDS}
        context, keys = _lookup(
            query, self.client, self.budget, self.mode, self.paths, list(self._seen))
        # A failed lookup is not remembered, so the next turn tries again.
        if context is None:
            return ""
        self._query, self._checked = digest, now
        for key in keys:
            self._seen[key] = now
        self._seen = dict(list(self._seen.items())[-MAX_SEEN:])
        return context
