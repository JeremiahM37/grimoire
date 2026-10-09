import importlib.util
import io
import json
import sys
from pathlib import Path

import pytest

SPEC = importlib.util.spec_from_file_location(
    "grimoire_context_hook_profiles", Path(__file__).parents[1] / "grimoire_context.py"
)
hook = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(hook)
KEY = "b" * 32

CODEX = {
    "hooks": {"events": {"session_start": "SessionStart", "prompt": "UserPromptSubmit",
                         "pre_action": "PreToolUse"}},
    "event_fields": {"event": "hook_event_name", "prompt": "prompt", "tool": "tool_name",
                     "tool_input": "tool_input", "session": "session_id", "cwd": "cwd",
                     "source": "source"},
    "actions": {"Bash": "command", "apply_patch": "command|input"},
    "output": "claude-json",
}
# A made-up agent: different event names, nested payload, plain stdout, no session id.
ZED = {
    "name": "zed",
    "hooks": {"events": {"prompt": "user_message", "pre_action": "before_command"}},
    "event_fields": {"event": "type", "prompt": "message.text", "tool": "command.kind",
                     "tool_input": "command.args", "session": "conversation", "cwd": "workspace"},
    "actions": {"shell": "cmd"},
    "output": "plain-stdout",
    "session_fallback": True,
}


@pytest.fixture
def environment(tmp_path):
    return {"GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path / "state"), "GRIMOIRE_CONTEXT_MODE": "all",
            "GRIMOIRE_AGENT_DIR": str(tmp_path / "agents")}


def fetcher(calls):
    def fetch(base, token, query, excluded, budget, mode, paths, extra=None):
        calls.append((query, extra))
        return {"context": "remember: use uv", "keys": [KEY]}
    return fetch


def test_codex_prompt_and_command_shapes(environment):
    calls = []
    prompt = {"hook_event_name": "UserPromptSubmit", "session_id": "s1", "cwd": "/w",
              "turn_id": "t", "model": "m", "prompt": "How do I deploy the kestrel service?"}
    out = hook.run(prompt, environment, fetcher(calls), now=100, profile=CODEX)
    assert out["hookSpecificOutput"] == {"hookEventName": "UserPromptSubmit",
                                         "additionalContext": "remember: use uv"}
    command = {"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": "/w",
               "tool_name": "Bash", "tool_input": {"command": "pip install requests"}}
    out = hook.run(command, environment, fetcher(calls), now=101, profile=CODEX)
    assert out["hookSpecificOutput"]["hookEventName"] == "PreToolUse"
    assert calls[-1][0] == "Bash pip install requests"
    assert calls[-1][1]["stage"] == "action"


def test_codex_patch_uses_the_first_field_present(environment):
    calls = []
    patch = {"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": "/w",
             "tool_name": "apply_patch", "tool_input": {"input": "*** Begin Patch\n*** Add File: a.py"}}
    assert hook.run(patch, environment, fetcher(calls), now=1, profile=CODEX)
    assert calls[0][0].startswith("apply_patch *** Begin Patch")
    # an edit-like tool needs the stricter relevance bar, as Claude's Edit does
    assert calls[0][1]["min_rel"] == "0.8"


def test_codex_unknown_tools_and_events_cost_nothing(environment):
    calls = []
    other = {"hook_event_name": "PreToolUse", "session_id": "s", "tool_name": "web_search",
             "tool_input": {"query": "x"}}
    post = {"hook_event_name": "PostToolUse", "session_id": "s", "tool_name": "Bash",
            "tool_input": {"command": "ls"}}
    assert hook.run(other, environment, fetcher(calls), now=1, profile=CODEX) is None
    assert hook.run(post, environment, fetcher(calls), now=1, profile=CODEX) is None
    assert calls == []


def test_shell_command_arrays_are_joined():
    assert hook.lookup({"command": ["git", "push"]}, "command") == "git push"


def test_made_up_agent_with_nested_fields_and_plain_output(environment):
    calls = []
    message = {"type": "user_message", "conversation": "c9", "workspace": "/p",
               "message": {"text": "Why does the kestrel build fail?"}}
    out = hook.run(message, environment, fetcher(calls), now=10, profile=ZED)
    assert out == "remember: use uv"
    action = {"type": "before_command", "workspace": "/p",
              "command": {"kind": "shell", "args": {"cmd": "docker compose up"}}}
    out = hook.run(action, environment, fetcher(calls), now=11, profile=ZED)
    assert out == "remember: use uv"
    assert calls[-1][0] == "shell docker compose up"


def test_session_fallback_is_only_for_profiles_that_ask(environment):
    calls = []
    message = {"type": "user_message", "workspace": "/p", "message": {"text": "kestrel deploy"}}
    strict = dict(ZED, session_fallback=False)
    assert hook.run(message, environment, fetcher(calls), now=1, profile=strict) is None
    assert hook.run(message, environment, fetcher(calls), now=1, profile=ZED)


def test_event_flag_names_the_event_when_the_payload_does_not(environment):
    calls = []
    bare = {"session_id": "s", "prompt": "How does the kestrel deploy work?"}
    assert hook.run(bare, environment, fetcher(calls), now=1) is None
    out = hook.run(bare, environment, fetcher(calls), now=2, kind="prompt")
    assert out["hookSpecificOutput"]["additionalContext"] == "remember: use uv"


def test_without_a_profile_the_default_is_claude_code():
    assert hook.load_profile("", {}) is hook.DEFAULT_PROFILE
    assert hook.load_profile("../etc/passwd", {}) is hook.DEFAULT_PROFILE
    assert hook.load_profile("nope", {"GRIMOIRE_AGENT_DIR": "/nonexistent"}) is hook.DEFAULT_PROFILE


def test_main_reads_the_profile_the_installer_wrote(tmp_path, monkeypatch, capsys):
    agents = tmp_path / "agents"
    agents.mkdir()
    (agents / "zed.json").write_text(json.dumps(ZED))
    environment = {"GRIMOIRE_AGENT_DIR": str(agents), "GRIMOIRE_CONTEXT_MODE": "all",
                   "GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path / "state")}
    real = hook.run

    def run(event, env, profile=None, kind=None):
        return real(event, env, lambda *a, **k: {"context": "from main", "keys": [KEY]}, 5, profile, kind)

    monkeypatch.setattr(hook, "run", run)
    payload = json.dumps({"type": "user_message", "conversation": "c", "workspace": "/p",
                          "message": {"text": "kestrel deployment question"}}).encode()
    monkeypatch.setattr(sys, "stdin", type("S", (), {"buffer": io.BytesIO(payload)})())
    hook.main(["--agent", "zed"], environment)
    assert capsys.readouterr().out == "from main\n"


ASK = {"decision": "ask", "reason": "Grimoire rule: never push unasked [m:abc123]"}


def asking(context="remember: never push", permission=ASK):
    def fetch(base, token, query, excluded, budget, mode, paths, extra=None):
        return {"context": context, "keys": [KEY] if context else [], "permission": permission}
    return fetch


def push_event(tool="Bash", inp=None):
    return {"hook_event_name": "PreToolUse", "session_id": "s1", "cwd": "/w",
            "tool_name": tool, "tool_input": inp or {"command": "git push"}}


@pytest.mark.parametrize("profile", [hook.DEFAULT_PROFILE, CODEX])
def test_enforce_ask_emits_permission_decision(environment, profile):
    out = hook.run(push_event(), environment, asking(), now=1, profile=profile)["hookSpecificOutput"]
    assert out["permissionDecision"] == "ask"
    assert out["permissionDecisionReason"] == ASK["reason"]
    assert out["additionalContext"] == "remember: never push"
    assert out["hookEventName"] == "PreToolUse"


def test_enforce_ask_is_emitted_even_with_no_context(environment):
    out = hook.run(push_event(), environment, asking(context=""), now=1)["hookSpecificOutput"]
    assert out["permissionDecision"] == "ask"
    assert "additionalContext" not in out


@pytest.mark.parametrize("permission", [
    {"decision": "allow", "reason": "x"}, {"decision": "deny", "reason": "x"},
    {"decision": "ask"}, {"decision": "ask", "reason": "  "}, "ask", None])
def test_only_a_well_formed_ask_is_honoured(environment, permission):
    out = hook.run(push_event(), environment, asking(permission=permission), now=1)
    assert "permissionDecision" not in out["hookSpecificOutput"]
    assert hook.run(push_event(), environment, asking(context="", permission=permission), now=2) is None


def test_plain_stdout_agents_get_the_reason_printed(environment):
    event = {"type": "before_command", "workspace": "/p",
             "command": {"kind": "shell", "args": {"cmd": "git push"}}}
    out = hook.run(event, environment, asking(), now=1, profile=ZED)
    assert out == ASK["reason"] + "\nremember: never push"
    assert hook.run(event, environment, asking(context=""), now=2, profile=ZED) == ASK["reason"]
