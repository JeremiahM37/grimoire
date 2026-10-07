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
    return {"GRIMOIRE_BANK_SESSIONS": "1", "GRIMOIRE_BANK_STATE_DIR": str(tmp_path / "state")}


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
