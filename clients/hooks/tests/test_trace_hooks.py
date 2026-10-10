"""Outcome capture for the utilization trace (docs/MEMORY_TRACE.md): codes and
counts per profile, read locally from the tool response, never sent as text."""
import hashlib
import importlib.util
import json
from pathlib import Path

HOOKS = Path(__file__).parents[1]
spec = importlib.util.spec_from_file_location("trace_outcome", HOOKS / "grimoire_outcome.py")
outcome = importlib.util.module_from_spec(spec)
spec.loader.exec_module(outcome)

SESSION = "sess-trace-1"
SID = hashlib.sha256(("session\0" + SESSION).encode()).hexdigest()[:32]


def env(tmp_path, **extra):
    return {"GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path), **extra}


def recorder():
    calls = []
    return calls, lambda base, token, body: calls.append(body) or {}


def post(tool, tool_input, response, name="PostToolUse", **extra):
    event = {"hook_event_name": name, "session_id": SESSION, "tool_name": tool, "tool_input": tool_input}
    if response is not None:
        event["tool_response"] = response
    event.update(extra)
    return event


def run(tmp_path, event, profile=None, kind=None, **envextra):
    calls, send = recorder()
    outcome.run(event, env(tmp_path, **envextra), send, kind=kind, profile=profile)
    return calls


# ---- tool responses of the real agents ------------------------------------

def test_claude_code_success_and_failure_events(tmp_path):
    ok = run(tmp_path, post("Bash", {"command": "ls"}, {"stdout": "a\nb", "stderr": "", "interrupted": False}))
    assert ok[0]["err"] == 0 and "exit" not in ok[0]
    # Claude Code reports a non-zero exit on its own event, with the text in `error`.
    fail = run(tmp_path, {"hook_event_name": "PostToolUseFailure", "session_id": SESSION, "tool_name": "Bash",
                          "tool_input": {"command": "false"}, "error": "Exit code 1\nboom"})
    assert fail[0]["err"] == 1 and fail[0]["exit"] == 1
    interrupted = run(tmp_path, post("Bash", {"command": "sleep 9"}, {"stdout": "", "interrupted": True}))
    assert interrupted[0]["err"] == 1


def test_codex_shapes_json_string_and_text(tmp_path):
    meta = json.dumps({"output": "FAIL", "metadata": {"exit_code": 2, "duration_seconds": 0.4}})
    got = run(tmp_path, post("Bash", {"command": "make"}, meta))
    assert got[0]["err"] == 1 and got[0]["exit"] == 2
    text = run(tmp_path, post("Bash", {"command": "make"}, "Exit code: 0\nWall time: 1s\nOutput:\nok"))
    assert text[0]["err"] == 0 and text[0]["exit"] == 0
    nonzero = run(tmp_path, post("Bash", {"command": "make"}, "Process exited with code 127"))
    assert nonzero[0]["err"] == 1 and nonzero[0]["exit"] == 127
    nothing = run(tmp_path, post("Bash", {"command": "make"}, None))
    assert "err" not in nothing[0]


def test_denial_is_visible_and_text_stays_local(tmp_path):
    got = run(tmp_path, {"hook_event_name": "PostToolUseFailure", "session_id": SESSION, "tool_name": "Bash",
                         "tool_input": {"command": "rm -rf build"},
                         "error": "The user doesn't want to proceed with this tool use. SECRET-OUT"})
    assert got[0]["denied"] is True and "SECRET-OUT" not in json.dumps(got)
    assert run(tmp_path, post("Bash", {"command": "cat x"}, {"stdout": "bash: x: Permission denied"}))[0].get("denied") is None


# ---- test runners ------------------------------------------------------------

def test_test_results_from_common_runners(tmp_path):
    cases = [
        ("go test ./...", "ok  \tpkg/a\t0.1s\nok  \tpkg/b\t0.2s\nFAIL\tpkg/c\t0.1s\n", (2, 1)),
        ("go test -v ./x", "--- PASS: TestA (0.00s)\n--- PASS: TestB (0.00s)\n--- FAIL: TestC (0.00s)\nFAIL\n", (2, 1)),
        ("pytest -q", "..F\n=========== 1 failed, 4 passed, 2 skipped in 0.31s ===========\n", (4, 1)),
        ("python3 -m pytest", "============ 7 passed in 1.20s ============\n", (7, 0)),
        ("npm test", "Tests:       2 failed, 9 passed, 11 total\nTime: 3s\n", (9, 2)),
        ("npx mocha", "  5 passing (20ms)\n  1 failing\n", (5, 1)),
        ("cargo test", "test result: ok. 12 passed; 0 failed; 0 ignored\ntest result: FAILED. 3 passed; 2 failed; 0 ignored\n", (15, 2)),
    ]
    for command, out, want in cases:
        got = run(tmp_path, post("Bash", {"command": command}, {"stdout": out}))[0]
        assert (got.get("tp"), got.get("tf")) == want, (command, got)
    # Output of a command that is not a test runner is never parsed.
    got = run(tmp_path, post("Bash", {"command": "cat results.txt"}, {"stdout": "5 passed in 1.0s"}))[0]
    assert "tp" not in got and "tf" not in got


# ---- edit regions: re-edit and revert ------------------------------------------

def edit(path, old, new):
    return {"file_path": path, "old_string": old, "new_string": new}


def test_reedit_and_revert_of_the_same_lines(tmp_path):
    first = run(tmp_path, post("Edit", edit("/a.go", "return 1  // old behaviour", "return 2  // new behaviour"), {"ok": True}))[0]
    assert first["region"] and "reedit" not in first and "revert" not in first
    # The agent edits the lines it just wrote: the earlier call was re-edited.
    second = run(tmp_path, post("Edit", edit("/a.go", "return 2  // new behaviour", "return 3  // third attempt"), {"ok": True}))[0]
    assert second["reedit"] == [first["region"]] and "revert" not in second
    # Then goes back to the original: the second call was reverted.
    third = run(tmp_path, post("Edit", edit("/a.go", "return 3  // third attempt", "return 1  // old behaviour"), {"ok": True}))[0]
    assert third["revert"] == [first["region"]] and third["reedit"] == [second["region"]]
    # A different file is unrelated.
    other = run(tmp_path, post("Edit", edit("/b.go", "return 2  // new behaviour", "x"), {"ok": True}))[0]
    assert "reedit" not in other and "revert" not in other
    # Only region ids leave: no line of code.
    blob = json.dumps([first, second, third, other])
    assert "return" not in blob and "behaviour" not in blob


def test_failed_edits_leave_no_region_and_patches_are_understood(tmp_path):
    failed = run(tmp_path, {"hook_event_name": "PostToolUseFailure", "session_id": SESSION, "tool_name": "Edit",
                            "tool_input": edit("/a.go", "aaaaaaa line", "bbbbbbb line"), "error": "string not found"})[0]
    assert "region" not in failed
    patch = "*** Begin Patch\n*** Update File: src/m.go\n@@\n-old statement here\n+new statement here\n*** End Patch"
    p1 = run(tmp_path, post("apply_patch", {"input": patch}, "Exit code: 0\n"),
             profile={"actions": {"apply_patch": "command|input"}})[0]
    assert p1["region"]
    again = "*** Begin Patch\n*** Update File: src/m.go\n@@\n-new statement here\n+newer statement here\n*** End Patch"
    p2 = run(tmp_path, post("apply_patch", {"input": again}, "Exit code: 0\n"),
             profile={"actions": {"apply_patch": "command|input"}})[0]
    assert p2["reedit"] == [p1["region"]]


def test_thrash_is_the_third_repeat_of_a_command(tmp_path):
    counts = [run(tmp_path, post("Bash", {"command": "make build"}, {"stdout": ""}))[0].get("thrash") for _ in range(4)]
    assert counts == [None, None, 3, 4]
    assert run(tmp_path, post("Bash", {"command": "make test"}, {"stdout": ""}))[0].get("thrash") is None


# ---- prompts ---------------------------------------------------------------

def test_prompt_event_sends_one_bit(tmp_path):
    for prompt, want in [("no, that's not what I asked", True), ("Stop. Undo that.", True), ("I told you to use tabs", True),
                         ("why did you delete the config?", True), ("looks great, now add tests", False),
                         ("please continue with the next step", False), ("no problem, go ahead", False)]:
        calls = run(tmp_path, {"hook_event_name": "UserPromptSubmit", "session_id": SESSION, "prompt": prompt})
        assert calls == [{"session": SID, "prompt": True, "corr": want}], (prompt, calls)
        assert prompt not in json.dumps(calls)
    assert run(tmp_path, {"hook_event_name": "UserPromptSubmit", "session_id": SESSION,
                          "prompt": "<task-notification>done</task-notification>"}) == []


# ---- profiles ----------------------------------------------------------------

CUSTOM = {"name": "myagent",
          "hooks": {"events": {"post_action": "tool_finished", "prompt": "user_message", "stop": "turn_end"}},
          "event_fields": {"event": "type", "prompt": "message.text", "tool": "call.name", "tool_input": "call.args",
                           "session": "conversation", "tool_response": "call.result", "tool_use_id": "call.id"},
          "actions": {"shell": "cmd"}, "delegation_tools": []}


def test_a_profile_with_other_field_names_works_without_code(tmp_path):
    event = {"type": "tool_finished", "conversation": SESSION,
             "call": {"name": "shell", "args": {"cmd": "go test ./..."}, "id": "call_77",
                      "result": {"output": "ok  \tpkg\t0.1s\n", "exit_code": 0}}}
    got = run(tmp_path, event, profile=CUSTOM)
    assert got == [{"session": SID, "tool": "shell", "target": "go test ./...", "err": 0, "exit": 0,
                    "tp": 1, "tf": 0, "tu": "call_77"}]
    # A tool the profile does not list draws nothing, and so does an unknown event.
    assert run(tmp_path, {**event, "call": {"name": "read", "args": {"cmd": "x"}}}, profile=CUSTOM) == []
    assert run(tmp_path, {**event, "type": "other"}, profile=CUSTOM) == []
    prompt = run(tmp_path, {"type": "user_message", "conversation": SESSION, "message": {"text": "no, wrong file"}}, profile=CUSTOM)
    assert prompt == [{"session": SID, "prompt": True, "corr": True}]


def test_claude_and_codex_ids_are_picked_up_from_the_payload(tmp_path):
    got = run(tmp_path, post("Bash", {"command": "ls"}, {"stdout": ""}, tool_use_id="toolu_01ABC"))
    assert got[0]["tu"] == "toolu_01ABC"


def test_trace_off_restores_the_old_body(tmp_path):
    got = run(tmp_path, post("Bash", {"command": "go test ./..."}, {"stdout": "ok  \tp\t1s"}), GRIMOIRE_TRACE="0")
    assert got == [{"session": SID, "tool": "Bash", "target": "go test ./..."}]
    assert run(tmp_path, {"hook_event_name": "UserPromptSubmit", "session_id": SESSION, "prompt": "no"}, GRIMOIRE_TRACE="0") == []


def test_tags_in_a_call_travel_as_cited(tmp_path):
    got = run(tmp_path, post("Bash", {"command": "git commit -m 'use snapshot (m:3e99)'"}, {"stdout": ""}))
    assert got[0]["cited"] == ["3e99"]


# ---- pending-action hash covers edit content (docs/MEMORY_TRACE.md) --------

ctx_spec = importlib.util.spec_from_file_location("trace_context", HOOKS / "grimoire_context.py")
context = importlib.util.module_from_spec(ctx_spec)
ctx_spec.loader.exec_module(context)

PATCH_A = "*** Begin Patch\n*** Update File: a.go\n@@\n-old\n+new one\n*** End Patch"
PATCH_B = PATCH_A.replace("new one", "new two")

EDIT_CASES = [
    ("claude-code", "Edit", {"file_path": "/r/a.go", "old_string": "x", "new_string": "y"},
     {"file_path": "/r/a.go", "old_string": "x", "new_string": "z"}),
    ("claude-code", "MultiEdit", {"file_path": "/r/a.go", "edits": [{"old_string": "x", "new_string": "y"}]},
     {"file_path": "/r/a.go", "edits": [{"old_string": "x", "new_string": "q"}]}),
    ("claude-code", "Write", {"file_path": "/r/a.go", "content": "one"},
     {"file_path": "/r/a.go", "content": "two"}),
    ("codex", "apply_patch", {"command": PATCH_A}, {"command": PATCH_B}),
    ("codex", "apply_patch", {"input": PATCH_A}, {"input": PATCH_B}),
]


def pre_ec(tool, tool_input, tmp_path, profile=None):
    seen = []

    def fetch(base, token, query, excluded, budget, mode, paths, extra=None):
        seen.append(dict(extra or {}))
        return {"context": "", "keys": []}

    event = {"hook_event_name": "PreToolUse", "session_id": SESSION, "tool_name": tool,
             "tool_input": tool_input, "tool_use_id": "toolu_1"}
    context.run(event, {"GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path), "GRIMOIRE_CONTEXT_MODE": "all"}, fetch, profile=profile, kind="pre_action")
    return seen[0].get("ec") if seen else None


def test_edit_content_hash_differs_per_edit_and_matches_both_sides(tmp_path):
    for agent, tool, first, second in EDIT_CASES:
        profile = None
        if agent == "codex":
            profile = json.loads((HOOKS.parents[1] / "go/internal/agentprofile/builtin/codex.json").read_text())
            profile["actions"] = {"Bash": "command", "apply_patch": "command|input"}
            profile.setdefault("delegation_tools", [])
        a, b = pre_ec(tool, first, tmp_path, profile), pre_ec(tool, second, tmp_path, profile)
        assert a and b and a != b, (agent, tool)
        # The outcome hook sends the same value for the same call.
        post_a = run(tmp_path, post(tool, first, "ok"), profile=profile)
        assert post_a[0]["ec"] == a, (agent, tool)
        assert pre_ec(tool, first, tmp_path, profile) == a


def test_non_edit_calls_carry_no_content_hash(tmp_path):
    assert pre_ec("Bash", {"command": "git status"}, tmp_path) is None
    assert "ec" not in run(tmp_path, post("Bash", {"command": "git status"}, "ok"))[0]
    assert context.edit_content_hash("Edit", {"file_path": "/a"}) is None


def test_trace_off_sends_no_content_hash(tmp_path):
    seen = []
    event = {"hook_event_name": "PreToolUse", "session_id": SESSION, "tool_name": "Edit", "tool_use_id": "toolu_2",
             "tool_input": {"file_path": "/a", "old_string": "x", "new_string": "y"}}
    context.run(event, {"GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path), "GRIMOIRE_CONTEXT_MODE": "all", "GRIMOIRE_TRACE": "0"},
                lambda *a, **k: seen.append(a[-1] if a else k) or {"context": "", "keys": []}, kind="pre_action")
    assert not any("ec" in (x or {}) for x in seen if isinstance(x, dict))
