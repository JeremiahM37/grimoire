import importlib.util
import json
from pathlib import Path

import pytest

SPEC = importlib.util.spec_from_file_location(
    "grimoire_bank_session_hook", Path(__file__).parents[1] / "grimoire_bank_session.py"
)
hook = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(hook)


@pytest.fixture
def environment(tmp_path):
    # The digest has its own tests below; the others look at the transcript retain alone.
    return {"GRIMOIRE_BANK_SESSIONS": "1", "GRIMOIRE_BANK_DIGEST": "0",
            "GRIMOIRE_BANK_STATE_DIR": str(tmp_path / "state")}


@pytest.fixture
def repo(tmp_path):
    root = tmp_path / "My Repo"
    (root / ".git").mkdir(parents=True)
    (root / "src").mkdir()
    return root


def claude_transcript(path):
    lines = [
        {"type": "user", "timestamp": "2026-10-07T10:00:00Z",
         "message": {"role": "user", "content": "Why does the build fail?\n<system-reminder>secret context</system-reminder>"}},
        {"type": "assistant", "timestamp": "2026-10-07T10:00:05Z",
         "message": {"role": "assistant", "content": [
             {"type": "thinking", "thinking": "hmm"},
             {"type": "text", "text": "The cache is stale; clear it."},
             {"type": "tool_use", "name": "Bash", "input": {"command": "rm -rf cache"}}]}},
        {"type": "user", "message": {"role": "user", "content": [
            {"type": "tool_result", "content": "done"}]}},
        {"type": "user", "isMeta": True, "message": {"role": "user", "content": "meta"}},
        {"type": "user", "isSidechain": True, "message": {"role": "user", "content": "side"}},
        "not json at all",
    ]
    path.write_text("\n".join(json.dumps(x) if not isinstance(x, str) else x for x in lines))
    return path


def event(transcript, cwd, name="Stop", session="abc-123"):
    return {"hook_event_name": name, "session_id": session, "cwd": str(cwd),
            "transcript_path": str(transcript)}


def test_retains_text_turns_into_the_repo_bank(environment, repo, tmp_path):
    sent = []

    def send(base, token, path, body, timeout):
        sent.append((base, path, body))
        return {"success": True}

    transcript = claude_transcript(tmp_path / "t.jsonl")
    assert hook.run(event(transcript, repo / "src"), environment, send) is None
    base, path, body = sent[0]
    assert base == "http://127.0.0.1:9111"
    assert path == "/api/banks/coding-agent%3Amy-repo/memories"
    item = body["items"][0]
    assert item["document_id"] == "session:abc-123"
    assert item["context"] == "coding-agent session transcript"
    assert item["tags"] == ["source:session"]
    assert item["update_mode"] == "replace"
    assert item["timestamp"] == "2026-10-07T10:00:00Z"
    assert body["async"] is True
    assert item["content"] == [
        {"speaker": "user", "text": "Why does the build fail?", "timestamp": "2026-10-07T10:00:00Z"},
        {"speaker": "assistant", "text": "The cache is stale; clear it.",
         "timestamp": "2026-10-07T10:00:05Z"},
    ]
    assert "secret context" not in json.dumps(body)

    # The same transcript again sends nothing; a new turn sends the whole thing.
    hook.run(event(transcript, repo, name="SessionEnd"), environment, send)
    assert len(sent) == 1
    with transcript.open("a") as handle:
        handle.write("\n" + json.dumps({"type": "user", "message": {"role": "user", "content": "thanks, and the tests?"}}))
    hook.run(event(transcript, repo), environment, send)
    assert len(sent) == 2 and len(sent[1][2]["items"][0]["content"]) == 3


def test_codex_transcript_lines(environment, repo, tmp_path):
    transcript = tmp_path / "rollout.jsonl"
    transcript.write_text("\n".join(json.dumps(x) for x in [
        {"timestamp": "2026-10-07T09:00:00Z", "type": "response_item",
         "payload": {"type": "message", "role": "user",
                     "content": [{"type": "input_text", "text": "Rename the flag"}]}},
        {"type": "response_item", "payload": {"type": "function_call", "name": "shell"}},
        {"type": "response_item", "payload": {"type": "message", "role": "assistant",
                                              "content": [{"type": "output_text", "text": "Renamed."}]}},
    ]))
    sent = []
    hook.run(event(transcript, repo), environment, lambda *a: sent.append(a) or {})
    turns = sent[0][3]["items"][0]["content"]
    assert [t["speaker"] for t in turns] == ["user", "assistant"]
    assert turns[1]["text"] == "Renamed."


def test_opt_in_and_url_safety(environment, repo, tmp_path):
    transcript = claude_transcript(tmp_path / "t.jsonl")
    sent = []
    send = lambda *a: sent.append(a) or {}  # noqa: E731
    hook.run(event(transcript, repo), {**environment, "GRIMOIRE_BANK_SESSIONS": "0"}, send)
    hook.run(event(transcript, repo), {**environment, "GRIMOIRE_URL": "http://example.com"}, send)
    hook.run(event(transcript, repo), {**environment, "GRIMOIRE_URL": "https://u:p@example.com"}, send)
    hook.run(event(transcript, tmp_path / "nowhere"), environment, send)  # not a repo, no bank
    hook.run(event(transcript, repo, session="../../etc"), environment, send)
    hook.run(event(transcript, repo, name="PreToolUse"), environment, send)
    assert sent == []
    hook.run(event(transcript, repo), {**environment, "GRIMOIRE_URL": "https://grimoire.example",
                                        "GRIMOIRE_BANK": "coding-agent:explicit"}, send)
    assert sent[0][2] == "/api/banks/coding-agent%3Aexplicit/memories"
    hook.run(event(transcript, repo, session="other"), {**environment, "GRIMOIRE_BANK": "Bad Id"}, send)
    assert len(sent) == 1


def test_failures_leave_no_marker_so_the_next_stop_retries(environment, repo, tmp_path):
    transcript = claude_transcript(tmp_path / "t.jsonl")
    attempts = []

    def failing(*args):
        attempts.append(args)
        raise OSError("connection refused")

    with pytest.raises(OSError):
        hook.run(event(transcript, repo), environment, failing)
    sent = []
    hook.run(event(transcript, repo), environment, lambda *a: sent.append(a) or {})
    assert len(sent) == 1


def test_server_without_a_queue_retains_in_the_foreground(environment, repo, tmp_path):
    import io
    import urllib.error

    transcript = claude_transcript(tmp_path / "t.jsonl")
    bodies = []

    def old_server(base, token, path, body, timeout):
        bodies.append(body)
        if body.get("async"):
            raise urllib.error.HTTPError(base + path, 400, "Bad Request", {}, io.BytesIO(
                b'{"detail": "async retain is not available yet; send async=false"}'))
        return {"success": True}

    hook.run(event(transcript, repo), environment, old_server)
    assert [b.get("async") for b in bodies] == [True, None]

    def other_400(base, token, path, body, timeout):
        raise urllib.error.HTTPError(base + path, 400, "Bad Request", {}, io.BytesIO(b'{"detail": "items"}'))

    with pytest.raises(urllib.error.HTTPError):
        hook.run(event(transcript, repo, session="s2"), environment, other_400)


def test_oversized_transcript_keeps_the_newest_turns(environment, repo, tmp_path, monkeypatch):
    monkeypatch.setattr(hook, "MAX_CONTENT_BYTES", 400)
    transcript = tmp_path / "big.jsonl"
    transcript.write_text("\n".join(json.dumps(
        {"type": "user", "message": {"role": "user", "content": f"turn {i} " + "x" * 50}})
        for i in range(20)))
    sent = []
    hook.run(event(transcript, repo), environment, lambda *a: sent.append(a) or {})
    turns = sent[0][3]["items"][0]["content"]
    assert turns[-1]["text"].startswith("turn 19")
    assert len(json.dumps(turns).encode()) <= 400


def test_recall_injection_is_opt_in_and_bounded(environment, repo):
    prompt = {"hook_event_name": "UserPromptSubmit", "session_id": "s", "cwd": str(repo),
              "prompt": "How do we cut a release?"}
    calls = []

    def send(base, token, path, body, timeout):
        calls.append((path, body, timeout))
        return {"results": [
            {"text": "Releases are tagged from main", "authority": "human",
             "occurred_start": "2026-09-01T00:00:00Z"},
            {"text": "Use the release script", "disputed_by": "h1"},
            {"text": "y" * 5000}]}

    assert hook.run(prompt, environment, send) is None  # recall not enabled
    out = hook.run(prompt, {**environment, "GRIMOIRE_BANK_RECALL": "1"}, send)
    context = out["hookSpecificOutput"]["additionalContext"]
    assert calls[0][0] == "/api/banks/coding-agent%3Amy-repo/memories/recall"
    assert calls[0][1]["query"] == "How do we cut a release?" and calls[0][2] <= 2
    assert "- Releases are tagged from main [written by a person] (2026-09-01)" in context
    assert "[disputed]" in context
    assert "y" * 100 not in context
    assert len(context.encode()) <= hook.MAX_CONTEXT_BYTES
    assert context.startswith("<grimoire_bank_memories")  # stripped again when retained
    assert hook.clean("before " + context + " after") == "before  after"
    assert hook.run({**prompt, "prompt": "thanks"}, {**environment, "GRIMOIRE_BANK_RECALL": "1"},
                    send) is None


def test_private_spans_and_secrets_never_leave_the_machine(environment, repo, tmp_path):
    key = "ghp_" + "aB3dE5gH7j" * 4
    lines = [
        {"type": "user", "message": {"role": "user", "content":
            "Deploy it. <private>my pin is 4411</private> then use " + key}},
        {"type": "assistant", "message": {"role": "assistant", "content": [
            {"type": "text", "text": "Ok.\n<private>unclosed: hush"}]}},
        {"type": "user", "message": {"role": "user", "content": "password: Zx9Qm2Lp7Rt4Vw8Yc1Bn5 stays out"}},
    ]
    transcript = tmp_path / "t.jsonl"
    transcript.write_text("\n".join(json.dumps(x) for x in lines))
    sent = []
    hook.run(event(transcript, repo), environment, lambda b, t, p, body, to: sent.append(body) or {})
    wire = json.dumps(sent[0])
    for leak in ("4411", key, "hush", "Zx9Qm2Lp7Rt4Vw8Yc1Bn5"):
        assert leak not in wire
    assert "[REDACTED:GitHub token]" in wire
    assert sent[0]["items"][0]["scan_secrets"] is True


def test_sanitize_leaves_placeholders_and_prose_alone():
    text = 'api_key = "TODO"  and ordinary words about tokens'
    assert hook.sanitize(text) == text


def start_event(repo, source="startup"):
    return {"hook_event_name": "SessionStart", "session_id": "s", "cwd": str(repo), "source": source}


def test_fit_items_drops_the_lowest_value_until_it_fits():
    items = ["- most valuable"] + [f"- filler {i} {'z' * 50}" for i in range(40)]
    text, kept, dropped = hook.fit_items("<a>\n", items, "\n</a>", 500)
    assert len(text) <= 500 and kept + dropped == len(items) and dropped > 0
    assert "- most valuable" in text and "left out" in text
    assert "filler 0 " in text and "filler 39 " not in text  # the tail went first
    assert hook.fit_items("<a>\n", ["x" * 900], "</a>", 500) == ("", 0, 1)


def test_hook_limit_is_configurable_and_clamped():
    assert hook.hook_limit({}) == 9000
    assert hook.hook_limit({"GRIMOIRE_HOOK_MAX_CHARS": "2000"}) == 2000
    assert hook.hook_limit({"GRIMOIRE_HOOK_MAX_CHARS": "99999"}) == hook.MAX_HOOK_CHARS < 10000
    assert hook.hook_limit({"GRIMOIRE_HOOK_MAX_CHARS": "5"}) == hook.MIN_HOOK_CHARS
    assert hook.hook_limit({"GRIMOIRE_HOOK_MAX_CHARS": "junk"}) == 9000


def test_session_start_injection_is_opt_in_and_never_exceeds_the_limit(environment, repo):
    seen = []

    def get(base, token, path, timeout):
        seen.append(path)
        return {"context": "<grimoire_bank_context>\n" + "line\n" * 5000 + "</grimoire_bank_context>"}

    assert hook.run(start_event(repo), environment, get=get) is None  # not enabled
    on = {**environment, "GRIMOIRE_BANK_CONTEXT": "1", "GRIMOIRE_HOOK_MAX_CHARS": "1500"}
    out = hook.run(start_event(repo, "compact"), on, get=get)
    text = out["hookSpecificOutput"]["additionalContext"]
    assert out["hookSpecificOutput"]["hookEventName"] == "SessionStart"
    assert len(text) <= 1500
    assert seen[0] == "/api/banks/coding-agent%3Amy-repo/context?max_chars=1500&source=compact"
    assert hook.run(start_event(repo), on, get=lambda *a: {"context": ""}) is None


def test_stop_writes_a_rule_digest_and_session_end_asks_for_the_model_once(environment, repo, tmp_path):
    env = {**environment, "GRIMOIRE_BANK_DIGEST": "1"}
    transcript = claude_transcript(tmp_path / "t.jsonl")
    calls = []

    def send(base, token, path, body, timeout):
        calls.append((path, body))
        return {}

    hook.run(event(transcript, repo, "Stop"), env, send)
    digests = [c for c in calls if c[0].endswith("/digest")]
    path, body = digests[0]
    assert path == "/api/banks/coding-agent%3Amy-repo/sessions/abc-123/digest"
    assert body["use_model"] is False
    assert [t["speaker"] for t in body["turns"]] == ["user", "assistant"]
    hook.run(event(transcript, repo, "Stop"), env, send)  # same input: nothing sent again
    assert len([c for c in calls if c[0].endswith("/digest")]) == 1
    hook.run(event(transcript, repo, "SessionEnd"), env, send)
    digests = [c for c in calls if c[0].endswith("/digest")]
    assert len(digests) == 2 and digests[1][1]["use_model"] is True
    hook.run(event(transcript, repo, "SessionEnd"), env, send)
    assert len([c for c in calls if c[0].endswith("/digest")]) == 2  # one model call per session
    off = {**env, "GRIMOIRE_BANK_DIGEST_MODEL": "0", "GRIMOIRE_BANK_STATE_DIR": str(tmp_path / "s2")}
    calls.clear()
    hook.run(event(transcript, repo, "SessionEnd"), off, send)
    assert [c[1]["use_model"] for c in calls if c[0].endswith("/digest")] == [False]


def test_a_failed_retain_still_writes_the_digest(environment, repo, tmp_path):
    import urllib.error

    env = {**environment, "GRIMOIRE_BANK_DIGEST": "1"}
    transcript = claude_transcript(tmp_path / "t.jsonl")
    seen = []

    def send(base, token, path, body, timeout):
        seen.append(path)
        if path.endswith("/memories"):
            raise urllib.error.URLError("down")
        return {}

    with pytest.raises(urllib.error.URLError):
        hook.run(event(transcript, repo), env, send)
    assert any(p.endswith("/digest") for p in seen)


def tool_event(repo, name, tool_input, response=None, session="abc-123", event_name="PostToolUse"):
    out = {"hook_event_name": event_name, "session_id": session, "cwd": str(repo),
           "tool_name": name, "tool_input": tool_input}
    if response is not None:
        out["tool_response"] = response
    return out


def test_tool_capture_is_opt_in_local_and_rule_based(environment, repo):
    no_network = lambda *a, **k: pytest.fail("PostToolUse must not touch the network")  # noqa: E731
    edit = tool_event(repo, "Edit", {"file_path": str(repo / "src" / "app.py")})
    assert hook.run(edit, environment, no_network) is None
    assert not list(Path(environment["GRIMOIRE_BANK_STATE_DIR"]).glob("*.activity.jsonl")) \
        if Path(environment["GRIMOIRE_BANK_STATE_DIR"]).exists() else True
    on = {**environment, "GRIMOIRE_BANK_TOOLS": "1"}
    key = "ghp_" + "aB3dE5gH7j" * 4
    exit_code = {"exit_code": 2, "stdout": "PRINTED-OUTPUT-MUST-NOT-BE-KEPT"}
    for event_ in (
        edit,
        tool_event(repo, "Write", {"file_path": str(repo / "new.txt")}),
        tool_event(repo, "Edit", {"file_path": str(repo / "src" / "app.py")}),  # same file twice
        tool_event(repo, "Bash", {"command": "pytest -q"}, exit_code),
        tool_event(repo, "Bash", {"command": "curl -H 'Authorization: " + key + "' x <private>hush</private>"}, {}),
        tool_event(repo, "Read", {"file_path": str(repo / "README.md")}),  # reads are not recorded
        tool_event(repo, "Bash", {"command": "make"}, {}, event_name="PostToolUseFailure"),
    ):
        assert hook.run(event_, on, no_network) is None
    buffer = next(Path(environment["GRIMOIRE_BANK_STATE_DIR"]).glob("*.activity.jsonl"))
    raw = buffer.read_text()
    assert oct(buffer.stat().st_mode & 0o777) == "0o600"
    for leak in (key, "hush", "PRINTED-OUTPUT"):
        assert leak not in raw
    activity = hook.tool_activity(on, "abc-123")
    assert activity["files"] == [str(Path("src") / "app.py"), "new.txt"]
    commands = {c["command"].split()[0]: c.get("exit") for c in activity["commands"]}
    assert commands["pytest"] == 2 and commands["make"] == 1 and "[REDACTED:GitHub token]" in raw


def test_stop_sends_the_buffer_once_without_a_model_and_session_end_clears_it(environment, repo, tmp_path):
    env = {**environment, "GRIMOIRE_BANK_TOOLS": "1", "GRIMOIRE_BANK_DIGEST": "1"}
    exit_code = {"exit_code": 1}
    hook.run(tool_event(repo, "Edit", {"file_path": str(repo / "a.py")}), env)
    hook.run(tool_event(repo, "Bash", {"command": "make test"}, exit_code), env)
    transcript = claude_transcript(tmp_path / "t.jsonl")
    calls = []
    send = lambda base, token, path, body, timeout: calls.append((path, body)) or {}  # noqa: E731
    hook.run(event(transcript, repo, "Stop"), env, send)
    activity_calls = [c for c in calls if c[1].get("items", [{}])[0].get("document_id") == "activity:abc-123"]
    assert len(activity_calls) == 1
    body = activity_calls[0][1]
    assert body["mode"] == "chunks" and body["async"] is True  # chunks mode: no extraction model
    assert "- a.py" in body["items"][0]["content"] and "`make test` (exit 1)" in body["items"][0]["content"]
    digest_body = [c[1] for c in calls if c[0].endswith("/digest")][0]
    assert digest_body["activity"]["files"] == ["a.py"]
    assert digest_body["activity"]["commands"] == [{"command": "make test", "exit": 1}]
    hook.run(event(transcript, repo, "Stop"), env, send)  # nothing new
    assert len([c for c in calls if c[1].get("items", [{}])[0].get("document_id") == "activity:abc-123"]) == 1
    hook.run(event(transcript, repo, "SessionEnd"), env, send)
    assert not list(Path(env["GRIMOIRE_BANK_STATE_DIR"]).glob("*.activity.jsonl"))


def test_stale_buffers_of_sessions_that_never_ended_are_swept(environment, repo):
    env = {**environment, "GRIMOIRE_BANK_TOOLS": "1"}
    hook.run(tool_event(repo, "Edit", {"file_path": str(repo / "a.py")}), env)
    buffer = next(Path(env["GRIMOIRE_BANK_STATE_DIR"]).glob("*.activity.jsonl"))
    hook.sweep_activity(env, now=buffer.stat().st_mtime + 60)
    assert buffer.exists()
    hook.sweep_activity(env, now=buffer.stat().st_mtime + hook.ACTIVITY_MAX_AGE + 60)
    assert not buffer.exists()
