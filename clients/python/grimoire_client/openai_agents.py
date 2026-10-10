"""OpenAI Agents SDK adapter: context in the instructions, memory as tools.

Two independent pieces — use either or both::

    from agents import Agent, Runner
    from grimoire_client import Grimoire
    from grimoire_client.openai_agents import grimoire_instructions, grimoire_tools

    client = Grimoire("http://localhost:9111", agent="support-bot")
    agent = Agent(
        name="support",
        instructions=grimoire_instructions("You answer support questions.", client=client),
        tools=grimoire_tools(client),
    )

``grimoire_instructions`` is the automatic half: before each model call it
appends bounded reference context for the user's latest message (see
``grimoire_client.context``). ``grimoire_tools`` is the agent-initiated half:
``recall``, ``remember`` and ``search_notes``. Context in the prompt cannot make
the model use it, and tools are only used when the model chooses to.

The ``agents`` package is imported lazily, so this module imports without it.
"""

from __future__ import annotations

import asyncio
import inspect
from collections.abc import Callable, Iterable
from typing import Any

from . import Grimoire, GrimoireError
from .context import DEFAULT_BUDGET, ContextSession, context_for

__all__ = ["grimoire_instructions", "grimoire_tools", "latest_user_text"]


def _sdk() -> Any:
    try:
        import agents
    except ImportError as exc:
        raise ImportError(
            "grimoire_tools needs the OpenAI Agents SDK: "
            "pip install 'grimoire-client[openai-agents]'"
        ) from exc
    return agents


def latest_user_text(items: Iterable[Any]) -> str:
    """Text of the last user message in an Agents SDK input-item list."""
    for item in reversed(list(items or [])):
        if not isinstance(item, dict) or item.get("role") != "user":
            continue
        content = item.get("content")
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            parts = [p.get("text", "") for p in content
                     if isinstance(p, dict) and isinstance(p.get("text"), str)]
            return "\n".join(p for p in parts if p)
    return ""


def grimoire_instructions(
    base_instructions: str | Callable[..., Any] = "",
    *,
    client: Grimoire | None = None,
    session: ContextSession | None = None,
    get_input: Callable[[Any], str] | None = None,
    budget: int = DEFAULT_BUDGET,
    mode: str | None = None,
    paths: Iterable[str] | None = None,
) -> Callable[[Any, Any], Any]:
    """A dynamic-instructions callable for ``Agent(instructions=...)``.

    Appends reference context for the latest user message to
    ``base_instructions`` (a string, or the SDK's own ``(context, agent)``
    callable, sync or async). The message is read from the run context's
    ``turn_input``; on an SDK version that does not populate it, pass
    ``get_input`` (``lambda run_context: ...``). Pass a ``ContextSession`` to
    deduplicate across the turns of one conversation. Lookup failures add
    nothing; the base instructions are always returned.
    """
    if session is None:
        session = ContextSession(client, budget=budget, mode=mode, paths=paths)

    async def instructions(run_context: Any, agent: Any) -> str:
        base = base_instructions
        if callable(base):
            base = base(run_context, agent)
            if inspect.isawaitable(base):
                base = await base
        base = base or ""
        try:
            prompt = (get_input(run_context) if get_input
                      else latest_user_text(getattr(run_context, "turn_input", None) or []))
            # The lookup is blocking urllib with a short timeout; keep it off
            # the event loop the agent is running on.
            reference = await asyncio.to_thread(session.context_for, prompt) if prompt else ""
        except Exception:  # noqa: BLE001 - instructions must never fail the run
            reference = ""
        return f"{base}\n\n{reference}" if reference else base

    return instructions


def _format_memories(memories: Iterable[Any]) -> str:
    lines = [f"- {m.text} [{m.path}]" for m in memories]
    return "\n".join(lines) or "No matching memories."


def grimoire_tools(client: Grimoire | None = None, *, limit: int = 5) -> list[Any]:
    """``recall``, ``remember`` and ``search_notes`` as Agents SDK function tools.

    Errors come back to the model as text rather than raising, so a server that
    is down degrades a turn instead of failing the run.
    """
    function_tool = _sdk().function_tool
    if client is None:
        from .context import _default_client
        client = _default_client()
    grimoire = client

    @function_tool
    def recall(query: str) -> str:
        """Recall what is currently remembered about a topic.

        Args:
            query: What to look up, in plain words.
        """
        try:
            return _format_memories(grimoire.search(query, limit=limit))
        except GrimoireError as exc:
            return f"Memory unavailable: {exc.message}"

    @function_tool
    def remember(text: str, topic: str = "") -> str:
        """Record one durable fact. A fact that contradicts a stored one replaces it.

        Args:
            text: The fact, as one self-contained sentence.
            topic: Optional topic to file it under.
        """
        try:
            result = grimoire.add(text, topic=topic)
        except GrimoireError as exc:
            return f"Not saved: {exc.message}"
        return f"{result.op}: {result.why}" if result.why else result.op

    @function_tool
    def search_notes(query: str) -> str:
        """Full-text search over the user's notes. Returns paths and excerpts.

        Args:
            query: Words to search for.
        """
        try:
            hits = grimoire.search_notes(query, limit=limit)
        except GrimoireError as exc:
            return f"Notes unavailable: {exc.message}"
        lines = [f"- {h.get('path', '')}: {str(h.get('snippet') or h.get('excerpt') or '')[:200]}"
                 for h in hits]
        return "\n".join(lines) or "No matching notes."

    return [recall, remember, search_notes]
