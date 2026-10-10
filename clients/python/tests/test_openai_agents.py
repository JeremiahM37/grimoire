"""OpenAI Agents SDK adapter.

The instructions callable and the module import are tested with fakes, so they
run without the SDK. Two tests drive the real ``agents`` package and are
skipped where it is not installed.
"""

from __future__ import annotations

import asyncio
import sys
import types

import pytest
from grimoire_client import GrimoireError, Memory
from grimoire_client import openai_agents as adapter
from grimoire_client.context import ContextSession


class FakeClient:
    url = "http://127.0.0.1:1"
    token = None

    def __init__(self):
        self.added = []
        self.fail = False

    def search(self, query="", *, limit=10, **_):
        if self.fail:
            raise GrimoireError(503, "down")
        return [Memory.from_json({"id": "1", "text": "prefers tabs", "path": "memory/p.md",
                                  "score": 1.0})]

    def add(self, text, **kwargs):
        if self.fail:
            raise GrimoireError(503, "down")
        self.added.append((text, kwargs))
        return types.SimpleNamespace(op="ADD", why="")

    def search_notes(self, query, *, limit=20):
        return [{"path": "a.md", "snippet": "tabs everywhere"}]


class StubSession:
    def __init__(self, reference="reference"):
        self.reference, self.prompts = reference, []

    def context_for(self, prompt):
        self.prompts.append(prompt)
        return self.reference


def run(coro):
    return asyncio.run(coro)


def ctx(*items):
    return types.SimpleNamespace(turn_input=list(items))


def test_module_imports_without_the_sdk_and_says_how_to_fix(monkeypatch):
    monkeypatch.setitem(sys.modules, "agents", None)
    with pytest.raises(ImportError, match=r"grimoire-client\[openai-agents\]"):
        adapter.grimoire_tools(FakeClient())


def test_instructions_append_context_for_the_latest_user_message():
    session = StubSession()
    instructions = adapter.grimoire_instructions("Be brief.", session=session)
    run_context = ctx(
        {"role": "user", "content": "first question"},
        {"role": "assistant", "content": "answer"},
        {"role": "user", "content": [{"type": "input_text", "text": "how does kestrel deploy?"}]},
    )
    assert run(instructions(run_context, None)) == "Be brief.\n\nreference"
    assert session.prompts == ["how does kestrel deploy?"]


def test_instructions_accept_callable_base_and_get_input():
    async def base(run_context, agent):
        return "dynamic base"
    session = StubSession()
    instructions = adapter.grimoire_instructions(
        base, session=session, get_input=lambda run_context: "explicit prompt")
    assert run(instructions(ctx(), None)) == "dynamic base\n\nreference"
    assert session.prompts == ["explicit prompt"]


def test_instructions_never_fail_or_pad_when_lookup_is_empty_or_broken():
    assert run(adapter.grimoire_instructions(
        "Base.", session=StubSession(""))(ctx({"role": "user", "content": "hi there"}), None)) == "Base."
    boom = StubSession()
    boom.context_for = lambda prompt: 1 / 0
    assert run(adapter.grimoire_instructions(
        "Base.", session=boom)(ctx({"role": "user", "content": "hi there"}), None)) == "Base."
    assert run(adapter.grimoire_instructions("Base.", session=StubSession())(ctx(), None)) == "Base."


def test_default_session_is_a_context_session():
    instructions = adapter.grimoire_instructions("x", client=FakeClient())
    closure = {c.cell_contents.__class__ for c in instructions.__closure__ if c.cell_contents}
    assert ContextSession in closure


def fake_sdk(monkeypatch):
    module = types.ModuleType("agents")
    module.function_tool = lambda fn: fn   # the decorator, minus the SDK
    monkeypatch.setitem(sys.modules, "agents", module)


def test_tools_with_a_fake_sdk(monkeypatch):
    fake_sdk(monkeypatch)
    client = FakeClient()
    recall, remember, search_notes = adapter.grimoire_tools(client)
    assert [t.__name__ for t in (recall, remember, search_notes)] == [
        "recall", "remember", "search_notes"]
    assert "prefers tabs" in recall("indentation")
    assert remember("uses tabs", topic="prefs") == "ADD"
    assert client.added == [("uses tabs", {"topic": "prefs"})]
    assert "a.md" in search_notes("tabs")
    client.fail = True
    assert "unavailable" in recall("x").lower()
    assert remember("y").startswith("Not saved")


def test_tools_with_the_real_sdk():
    agents = pytest.importorskip("agents", reason="openai-agents is not installed")
    tools = adapter.grimoire_tools(FakeClient())
    assert [t.name for t in tools] == ["recall", "remember", "search_notes"]
    assert all(isinstance(t, agents.FunctionTool) for t in tools)
    remember = tools[1]
    assert set(remember.params_json_schema["properties"]) == {"text", "topic"}


def test_real_agent_accepts_the_instructions_callable():
    agents = pytest.importorskip("agents", reason="openai-agents is not installed")
    session = StubSession()
    agent = agents.Agent(
        name="t", instructions=adapter.grimoire_instructions("Base.", session=session))
    wrapper = agents.RunContextWrapper(context=None)
    wrapper.turn_input = [{"role": "user", "content": "how does kestrel deploy?"}]
    assert run(agent.get_system_prompt(wrapper)) == "Base.\n\nreference"
