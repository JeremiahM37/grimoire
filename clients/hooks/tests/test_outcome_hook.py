import hashlib
import importlib.util
import json
from pathlib import Path

SPEC = importlib.util.spec_from_file_location(
    "grimoire_outcome_hook", Path(__file__).parents[1] / "grimoire_outcome.py"
)
hook = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(hook)


def sid(session):
    return hashlib.sha256(("session\0" + session).encode()).hexdigest()[:32]


def recorder(reply=None):
    calls = []

    def send(base, token, body):
        calls.append((base, token, body))
        return reply or {}

    return calls, send


def transcript(tmp_path, entries):
    path = tmp_path / "t.jsonl"
    path.write_text("\n".join(json.dumps(e) for e in entries) + "\n")
    return str(path)


def assistant(text):
    return {"type": "assistant", "message": {"content": [{"type": "text", "text": text}]}}


def test_post_tool_use_sends_tool_and_target_only(tmp_path):
    calls, send = recorder()
    event = {"hook_event_name": "PostToolUse", "session_id": "abc", "tool_name": "Bash",
             "tool_input": {"command": "git push origin main"}, "tool_response": {"stdout": "SECRET"}}
    assert hook.run(event, {"GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path)}, send) is None
    # The only addition is the outcome code (no error); the output stays here.
    assert calls[0][2] == {"session": sid("abc"), "tool": "Bash", "target": "git push origin main", "err": 0}
    assert "SECRET" not in json.dumps(calls)
    legacy = {"GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path), "GRIMOIRE_TRACE": "0"}
    assert hook.run(event, legacy, send) is None
    assert calls[1][2] == {"session": sid("abc"), "tool": "Bash", "target": "git push origin main"}


def test_non_action_tools_and_other_events_send_nothing():
    calls, send = recorder()
    hook.run({"hook_event_name": "PostToolUse", "session_id": "abc", "tool_name": "Read",
              "tool_input": {"file_path": "/x"}}, {}, send)
    hook.run({"hook_event_name": "UserPromptSubmit", "session_id": "abc"}, {}, send)
    assert calls == []


def test_stop_sends_only_tags_from_the_last_assistant_turn(tmp_path):
    path = transcript(tmp_path, [
        {"type": "user", "message": {"content": "first prompt"}},
        assistant("old answer (m:aaaa)"),
        {"type": "user", "message": {"content": "second prompt with private details"}},
        assistant("working on it, per (m:3e99)"),
        {"type": "user", "message": {"content": [{"type": "tool_result", "content": "m:dead is output"}]}},
        assistant("done; also m:ab12cd and again m:3e99. not-a-tag xm:1234"),
    ])
    calls, send = recorder()
    hook.run({"hook_event_name": "Stop", "session_id": "abc", "transcript_path": path}, {}, send)
    body = calls[0][2]
    assert body == {"session": sid("abc"), "stop": True, "cited": ["ab12cd", "3e99"]}
    assert "private" not in json.dumps(calls)


def test_stop_without_transcript_still_finalises():
    calls, send = recorder()
    hook.run({"hook_event_name": "Stop", "session_id": "abc", "transcript_path": "/nonexistent"}, {}, send)
    assert calls[0][2] == {"session": sid("abc"), "stop": True}


def test_pre_tool_use_is_opt_in_and_only_ever_asks():
    calls, send = recorder({"permission": {"decision": "ask", "reason": "Grimoire rule: no pushes"}})
    event = {"hook_event_name": "PreToolUse", "session_id": "abc", "tool_name": "Bash",
             "tool_input": {"command": "git push"}}
    assert hook.run(event, {}, send) is None and calls == []
    out = hook.run(event, {"GRIMOIRE_OUTCOME_ENFORCE": "1"}, send)
    assert out["hookSpecificOutput"]["permissionDecision"] == "ask"
    assert out["hookSpecificOutput"]["hookEventName"] == "PreToolUse"
    assert calls[0][2]["pre"] is True
    assert hook.permission_output({"decision": "allow", "reason": "x"}) is None
    assert hook.permission_output({"decision": "deny", "reason": "x"}) is None


def test_refuses_unsafe_servers_and_can_be_disabled():
    calls, send = recorder()
    event = {"hook_event_name": "PostToolUse", "session_id": "abc", "tool_name": "Bash",
             "tool_input": {"command": "ls"}}
    hook.run(event, {"GRIMOIRE_URL": "http://example.com"}, send)
    hook.run(event, {"GRIMOIRE_URL": "http://user:pw@127.0.0.1:9111"}, send)
    hook.run(event, {"GRIMOIRE_OUTCOME": "0"}, send)
    assert calls == []
    hook.run(event, {"GRIMOIRE_URL": "https://grimoire.example", "GRIMOIRE_AUTH_TOKEN": "t"}, send)
    assert calls[0][:2] == ("https://grimoire.example", "t")


def test_session_hash_matches_the_context_hook():
    spec = importlib.util.spec_from_file_location("ctx", Path(__file__).parents[1] / "grimoire_context.py")
    ctx = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(ctx)
    assert hook.session_hash("s-1") == ctx.fingerprint("session\0" + "s-1")[:32]


def test_event_flag_names_the_event_for_agents_with_other_event_names():
    sent = []
    event = {"event_type": "after_command", "session_id": "s1", "tool_name": "Bash",
             "tool_input": {"command": "ls"}}
    assert hook.run(event, {}, lambda b, t, body: sent.append(body) or {}, kind="post_action") is None
    assert sent and sent[0]["tool"] == "Bash" and sent[0]["target"] == "ls"
    sent.clear()
    hook.run({"session_id": "s1"}, {}, lambda b, t, body: sent.append(body) or {}, kind="stop")
    assert sent and sent[0]["stop"] is True
