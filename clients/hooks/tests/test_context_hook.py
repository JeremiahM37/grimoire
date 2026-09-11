import importlib.util
import json
from pathlib import Path

import pytest

SPEC = importlib.util.spec_from_file_location(
    "grimoire_context_hook", Path(__file__).parents[1] / "grimoire_context.py"
)
hook = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(hook)
KEY = "a" * 32


@pytest.fixture
def environment(tmp_path):
    return {"GRIMOIRE_CONTEXT_STATE_DIR": str(tmp_path), "GRIMOIRE_CONTEXT_MODE": "scoped",
            "GRIMOIRE_CONTEXT_PATHS": '["memory/kestrel.md", "projects/kestrel/"]'}


def event(prompt="How does kestrel deployment work?", session="one"):
    return {"hook_event_name": "UserPromptSubmit", "session_id": session,
            "cwd": "/project", "prompt": prompt}


def test_repeat_and_fact_deduplication(environment):
    calls = []

    def fetch(base, token, query, excluded, budget, mode, paths):
        assert mode == "scoped"
        assert paths == ["memory/kestrel.md", "projects/kestrel/"]
        calls.append(excluded)
        return {"context": "" if KEY in excluded else "reference", "keys": [] if excluded else [KEY]}

    assert hook.run(event(), environment, fetch, now=100)
    assert hook.run(event(), environment, fetch, now=101) is None
    assert len(calls) == 1
    assert hook.run(event("Kestrel deployment certificates?"), environment, fetch, now=102) is None
    assert calls[-1] == [KEY]
    assert hook.run(event(session="two"), environment, fetch, now=103)


def test_compaction_and_ttl_rehydrate(environment):
    def fetch(base, token, query, excluded, budget, mode, paths):
        return {"context": "reference", "keys": [KEY]}

    hook.run(event(), environment, fetch, now=100)
    reset = {**event(), "hook_event_name": "SessionStart", "source": "compact"}
    assert hook.run(reset, environment, fetch, now=101) is None
    assert hook.run(event(), environment, fetch, now=102)

    def check_expiry(base, token, query, excluded, budget, mode, paths):
        assert excluded == []
        return {"context": "reference", "keys": [KEY]}

    assert hook.run(event(), environment, check_expiry, now=2000)


@pytest.mark.parametrize("prompt", ["", "continue", "Thanks!", "yes", "Hello", "ok"])
def test_chitchat_costs_no_requests_or_context(environment, prompt):
    def unexpected(*args):
        pytest.fail("unnecessary request")
    assert hook.run(event(prompt), environment, unexpected) is None


def test_budget_and_state_contains_no_content(environment):
    environment["GRIMOIRE_CONTEXT_MAX_BYTES"] = "128"
    def fetch(*args):
        return {"context": "private reference text", "keys": [KEY]}
    assert hook.run(event(), environment, fetch, now=100)
    state = next(Path(environment["GRIMOIRE_CONTEXT_STATE_DIR"]).glob("*.json")).read_text()
    assert "private reference text" not in state
    assert "kestrel" not in state
    assert KEY in json.loads(state)["seen"]
    assert hook.run(event(session="new"), environment,
                    lambda *args: {"context": "x" * 129, "keys": [KEY]}) is None


def test_failures_release_lock_and_do_not_mark_seen(environment):
    def fail(*args):
        raise OSError("unavailable")
    with pytest.raises(OSError):
        hook.run(event(), environment, fail)
    assert not list(Path(environment["GRIMOIRE_CONTEXT_STATE_DIR"]).glob("*.lock"))
    assert not list(Path(environment["GRIMOIRE_CONTEXT_STATE_DIR"]).glob("*.json"))


def test_credentials_and_project_isolate_seen_state(environment):
    def fetch(base, token, query, excluded, budget, mode, paths):
        assert excluded == []
        return {"context": "reference", "keys": [KEY]}
    hook.run(event(), environment, fetch)
    hook.run({**event(), "cwd": "/another"}, environment, fetch)
    hook.run(event(), {**environment, "GRIMOIRE_AUTH_TOKEN": "different-principal"}, fetch)


def test_no_insecure_remote_requests(environment):
    environment["GRIMOIRE_URL"] = "http://remote.example:9111"
    assert hook.run(event(), environment, lambda *args: pytest.fail("remote plaintext")) is None


@pytest.mark.parametrize("mode", ["manual", "off", "invalid"])
def test_nonautomatic_modes_never_fetch(environment, mode):
    environment["GRIMOIRE_CONTEXT_MODE"] = mode
    assert hook.run(event(), environment, lambda *args: pytest.fail("disabled")) is None


def test_missing_scope_does_not_fall_back_to_everything(environment):
    environment["GRIMOIRE_CONTEXT_PATHS"] = "[]"
    assert hook.run(event(), environment, lambda *args: pytest.fail("scope widened")) is None


def test_all_mode_is_explicit(environment):
    environment["GRIMOIRE_CONTEXT_MODE"] = "all"
    environment["GRIMOIRE_CONTEXT_PATHS"] = "[]"
    def fetch(base, token, query, excluded, budget, mode, paths):
        assert mode == "all" and paths == []
        return {"context": "reference", "keys": [KEY]}
    assert hook.run(event(), environment, fetch)
