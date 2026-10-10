"""Pydantic AI adapters: memory tools and a context helper for dynamic prompts.

    from pydantic_ai import Agent
    from grimoire_client import Grimoire
    from grimoire_client.pydantic_ai import grimoire_context, grimoire_tools

    client = Grimoire("http://localhost:9111")
    agent = Agent(
        "anthropic:claude-sonnet-4-5",
        tools=grimoire_tools(client, agent_name="support-bot"),
    )

    @agent.system_prompt
    def memory_context(ctx) -> str:
        return grimoire_context(client, ctx.prompt or "")

``grimoire_tools`` returns plain functions. Pydantic AI reads their typed
signatures and docstrings to build the tool schema, so the types and the
``Args:`` sections below are the tool's documentation to the model. The
``pydantic_ai`` package itself is never imported, so this module loads without
it.

``grimoire_context`` is the automatic half: it returns bounded reference context
for a query (the same read path as the Claude Code hook), and returns ``""`` on
any failure. Context in the prompt is a suggestion; the tools are how a model
records and looks things up on purpose.
"""

from __future__ import annotations

from collections.abc import Callable, Iterable

from . import Grimoire
from ._common import recall_facts, remember_fact, search_notes_text
from .context import DEFAULT_BUDGET, context_for

__all__ = ["grimoire_context", "grimoire_tools"]


def grimoire_tools(client: Grimoire, agent_name: str = "pydantic-ai", *,
                   limit: int = 5) -> list[Callable[..., str]]:
    """``remember``, ``recall`` and ``search_notes`` as Pydantic AI tools.

    ``agent_name`` is stamped on every write so recall can say which agent
    recorded a fact. Each tool answers in text and never raises.
    """

    def remember(text: str, topic: str = "") -> str:
        """Record one durable fact. A fact that contradicts a stored one replaces it.

        Args:
            text: The fact, as one self-contained sentence.
            topic: Optional topic to file it under, such as "preferences".
        """
        return remember_fact(client, agent_name, text, topic)

    def recall(query: str) -> str:
        """Recall what is currently remembered about a topic.

        Only currently believed facts are returned; replaced beliefs are left out.

        Args:
            query: What to look up, in plain words.
        """
        return recall_facts(client, query, limit)

    def search_notes(query: str) -> str:
        """Full-text search over the user's notes. Returns paths and excerpts.

        Args:
            query: Words to search for.
        """
        return search_notes_text(client, query, limit)

    return [remember, recall, search_notes]


def grimoire_context(client: Grimoire, query: str, *, budget: int = DEFAULT_BUDGET,
                     paths: Iterable[str] | None = None) -> str:
    """Bounded reference context for ``query``, for a dynamic system prompt.

    Returns ``""`` when there is nothing relevant or the server cannot be
    reached, so a system prompt built from it is always valid.
    """
    return context_for(query, client=client, budget=budget, paths=paths)
