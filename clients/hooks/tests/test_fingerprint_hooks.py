"""Tag-free use detection in the two hooks: the context hook stores the salted
hashes the server sends, the outcome hook matches them against what the agent
did and said and reports counts only."""
import hashlib
import importlib.util
import json
from pathlib import Path

HOOKS = Path(__file__).parents[1]


def load(name, file):
    spec = importlib.util.spec_from_file_location(name, HOOKS / file)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


outcome = load("fp_outcome", "grimoire_outcome.py")
context = load("fp_context", "grimoire_context.py")
SALT = "0123456789abcdef"
SESSION = "sess-fp-1"
SID = hashlib.sha256(("session\0" + SESSION).encode()).hexdigest()[:32]
MEMORY_TEXT = "Snapshot first with ~/tailscale-helpers/snapshot.sh before touching the tailnet"
TOKENS = ["snapshot.sh", "tailscale-helpers"]


def wire(tag="3e99", tokens=TOKENS, salt=SALT):
    return {"v": 1, "salt": salt, "items": {tag: [outcome.fp_hash(salt, t) for t in tokens]}, "none": []}


def inject(tmp_path, tag="3e99", tokens=TOKENS, salt=SALT):
    """What the context hook does with a server response."""
    context.store_fingerprints(tmp_path, SID, {"fp": wire(tag, tokens, salt)})


def env(tmp_path, **extra):
    return {"GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path), **extra}


def recorder():
    calls = []
    return calls, lambda base, token, body: calls.append(body) or {}


def post_tool(command):
    return {"hook_event_name": "PostToolUse", "session_id": SESSION, "tool_name": "Bash",
            "tool_input": {"command": command}}


def stop(tmp_path, *texts, prompt="do the thing"):
    entries = [{"type": "user", "message": {"content": prompt}}]
    entries += [{"type": "assistant", "message": {"content": [{"type": "text", "text": t}]}} for t in texts]
    path = tmp_path / "t.jsonl"
    path.write_text("\n".join(json.dumps(e) for e in entries) + "\n")
    return {"hook_event_name": "Stop", "session_id": SESSION, "transcript_path": str(path)}


def test_vectors_match_the_go_implementation():
    vectors = json.loads((Path(__file__).parent / "fingerprint_vectors.json").read_text())
    assert len(vectors) >= 8
    for vector in vectors:
        assert outcome.candidates(vector["text"]) == vector["candidates"], vector["text"]


def test_context_hook_stores_only_hashes_and_validates(tmp_path):
    inject(tmp_path)
    raw = (tmp_path / ("fp-" + SID + ".json")).read_text()
    assert "snapshot" not in raw and "tailscale" not in raw
    state = json.loads(raw)
    assert state["v"] == 1 and state["turn"] == 0 and state["items"][0]["tag"] == "3e99"
    # Malformed or foreign responses store nothing.
    for bad in [{}, {"fp": []}, {"fp": {"v": 2, "salt": SALT, "items": {"3e99": ["a" * 10]}}},
                {"fp": {"v": 1, "salt": SALT, "items": {"zz": ["a" * 10]}}},
                {"fp": {"v": 1, "salt": SALT, "items": {"3e99": ["notahash"]}}}]:
        other = tmp_path / "other"
        other.mkdir()
        context.store_fingerprints(other, SID, bad)
        assert not list(other.iterdir()) or json.loads(next(other.iterdir()).read_text())["items"] == []
        for f in other.iterdir():
            f.unlink()
        other.rmdir()
    # Re-injecting a tag replaces it and the list is bounded.
    inject(tmp_path, tokens=["other.sh"])
    assert len(json.loads(raw := (tmp_path / ("fp-" + SID + ".json")).read_text())["items"]) == 1
    for i in range(40):
        inject(tmp_path, tag="%04x" % i, tokens=["tok%d.sh" % i])
    assert len(json.loads((tmp_path / ("fp-" + SID + ".json")).read_text())["items"]) <= 24


def test_post_tool_use_sends_only_a_count_when_a_fingerprint_appears(tmp_path):
    inject(tmp_path)
    calls, send = recorder()
    secret = "cat /etc/shadow-private-secret"
    outcome.run(post_tool(secret), env(tmp_path), send)
    assert calls[0] == {"session": SID, "tool": "Bash", "target": secret}  # no match: unchanged
    outcome.run(post_tool("~/tailscale-helpers/snapshot.sh before-change"), env(tmp_path), send)
    body = calls[1]
    assert body["fp"] == {"3e99": 2}
    # Only the counts leave; no hash, token or text beyond the existing target.
    assert set(body) == {"session", "tool", "target", "fp"}
    state = json.loads((tmp_path / ("fp-" + SID + ".json")).read_text())
    assert state["items"][0]["tools"] == 2


def test_stop_matches_the_assistant_text_and_never_sends_it(tmp_path):
    inject(tmp_path)
    calls, send = recorder()
    private = "my private reasoning about the task"
    outcome.run(stop(tmp_path, private + " I ran snapshot.sh as the notes asked"), env(tmp_path), send)
    assert calls[0] == {"session": SID, "stop": True, "fp": {"3e99": 1}}
    assert private not in json.dumps(calls)


def test_tags_and_fingerprints_travel_together(tmp_path):
    inject(tmp_path)
    calls, send = recorder()
    outcome.run(stop(tmp_path, "used snapshot.sh (m:3e99)"), env(tmp_path), send)
    assert calls[0]["cited"] == ["3e99"] and calls[0]["fp"] == {"3e99": 1}


def test_use_window_is_the_turn_plus_one(tmp_path):
    inject(tmp_path)
    calls, send = recorder()
    e = env(tmp_path)
    outcome.run(stop(tmp_path, "nothing relevant"), e, send)           # turn 0 ends
    assert "fp" not in calls[0]
    outcome.run(stop(tmp_path, "now snapshot.sh"), e, send)             # turn 1: still in window
    assert calls[1]["fp"] == {"3e99": 1}
    outcome.run(stop(tmp_path, "snapshot.sh again"), e, send)           # turn 2: expired
    assert "fp" not in calls[2]
    state = json.loads((tmp_path / ("fp-" + SID + ".json")).read_text())
    assert state["items"] == [] and state["turn"] == 3


def test_window_is_configurable(tmp_path):
    inject(tmp_path)
    calls, send = recorder()
    e = env(tmp_path, GRIMOIRE_FP_WINDOW_TURNS="0")
    outcome.run(stop(tmp_path, "nothing"), e, send)
    outcome.run(stop(tmp_path, "snapshot.sh"), e, send)
    assert "fp" not in calls[1]
    inject(tmp_path)
    e = env(tmp_path, GRIMOIRE_FP_WINDOW_TOOLS="1")
    outcome.run(post_tool("ls"), e, send)
    outcome.run(post_tool("ls -l"), e, send)
    outcome.run(post_tool("snapshot.sh"), e, send)   # third call, bound is 1
    assert "fp" not in calls[-1]


def test_hash_salt_is_per_item_and_other_sessions_do_not_match(tmp_path):
    inject(tmp_path, salt="aaaaaaaaaaaaaaaa")
    inject(tmp_path, tag="beef", tokens=["quokka.yaml"], salt="bbbbbbbbbbbbbbbb")
    calls, send = recorder()
    outcome.run(post_tool("edit quokka.yaml and snapshot.sh"), env(tmp_path), send)
    assert calls[0]["fp"] == {"3e99": 1, "beef": 1}
    assert outcome.fp_hash("aaaaaaaaaaaaaaaa", "x.sh") != outcome.fp_hash("bbbbbbbbbbbbbbbb", "x.sh")
    # A different session has its own file: nothing matches there.
    calls.clear()
    other = dict(post_tool("snapshot.sh"), session_id="someone-else")
    outcome.run(other, env(tmp_path), send)
    assert "fp" not in calls[0]


def test_off_switch_and_missing_state_change_nothing(tmp_path):
    inject(tmp_path)
    calls, send = recorder()
    outcome.run(post_tool("snapshot.sh"), env(tmp_path, GRIMOIRE_FINGERPRINTS="0"), send)
    assert "fp" not in calls[0]
    fresh = tmp_path / "empty"
    fresh.mkdir()
    outcome.run(post_tool("snapshot.sh"), env(fresh), send)
    outcome.run(stop(tmp_path, "snapshot.sh"), env(fresh), send)
    assert calls[1] == {"session": SID, "tool": "Bash", "target": "snapshot.sh"}
    assert calls[2] == {"session": SID, "stop": True}


def test_candidates_normalisation_examples():
    c = outcome.candidates("Run /home/admin/Tailscale-Helpers/Snapshot.SH, then `--dry-run=1` at HTTPS://Host.Example:8443/x")
    for token in ["snapshot.sh", "tailscale-helpers", "--dry-run", "host.example", "host.example:8443", "x"[:0] or "snapshot.sh"]:
        assert token in c
    assert outcome.candidates("") == [] and outcome.candidates("ab -x") == []
