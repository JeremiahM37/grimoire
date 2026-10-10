"""Pre-request context helper, against a stub of GET /api/memory/context.

The server's half of the contract is tested in go/internal/api; this pins the
client's half and that the rules match clients/hooks/grimoire_context.py.
"""

from __future__ import annotations

import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest
from grimoire_client import Grimoire
from grimoire_client.context import ContextSession, context_for

KEY = "a" * 32


@pytest.fixture
def stub():
    state = {"calls": [], "reply": {"context": "reference", "keys": [KEY]}, "raw": None}

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            parsed = urllib.parse.urlsplit(self.path)
            state["calls"].append({
                "path": parsed.path, "query": urllib.parse.parse_qs(parsed.query, keep_blank_values=True),
                "auth": self.headers.get("Authorization"),
            })
            body = state["raw"] if state["raw"] is not None else json.dumps(state["reply"]).encode()
            self.send_response(200)
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *args):
            pass

    server = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    state["client"] = Grimoire(f"http://127.0.0.1:{server.server_port}", token="tok")
    yield state
    server.shutdown()


def test_calls_the_hook_endpoint_with_its_parameters(stub):
    got = context_for("How does kestrel deployment work?", client=stub["client"],
                      paths=["memory/kestrel.md"], exclude=[KEY], budget=1000)
    assert got == "reference"
    call = stub["calls"][0]
    assert call["path"] == "/api/memory/context"
    assert call["auth"] == "Bearer tok"
    assert call["query"]["scope"] == ["scoped"]
    assert call["query"]["path"] == ["memory/kestrel.md"]
    assert call["query"]["max_bytes"] == ["1000"]
    assert call["query"]["exclude"] == [KEY]
    assert call["query"]["limit"] == ["5"]


@pytest.mark.parametrize("prompt", ["", "  ", "thanks", "Thanks!", "ok", "continue", "Hello."])
def test_trivial_prompts_make_no_request(stub, prompt):
    assert context_for(prompt, client=stub["client"]) == ""
    assert stub["calls"] == []


def test_oversized_prompt_makes_no_request(stub):
    assert context_for("x" * 8001, client=stub["client"]) == ""
    assert stub["calls"] == []


def test_scope_rules_never_widen(stub):
    assert context_for("kestrel?", client=stub["client"], mode="scoped") == ""
    assert context_for("kestrel?", client=stub["client"], mode="all", paths=["a.md"]) == ""
    assert context_for("kestrel?", client=stub["client"], mode="bogus") == ""
    assert stub["calls"] == []


def test_default_mode_is_all_without_paths(stub):
    context_for("kestrel?", client=stub["client"])
    assert stub["calls"][0]["query"]["scope"] == ["all"]


@pytest.mark.parametrize("reply", [
    {"context": "x" * 129, "keys": []},
    {"context": "ok", "keys": ["a" * 32] * 11},
    {"context": "ok", "keys": ["not-hex"]},
    {"context": 5, "keys": []},
    {"context": "ok", "keys": "nope"},
])
def test_invalid_responses_are_dropped(stub, reply):
    stub["reply"] = reply
    assert context_for("kestrel?", client=stub["client"], budget=128) == ""


def test_malformed_and_oversized_bodies_are_dropped(stub):
    stub["raw"] = b"not json"
    assert context_for("kestrel?", client=stub["client"]) == ""
    stub["raw"] = json.dumps({"context": "x", "keys": [], "pad": "y" * 70000}).encode()
    assert context_for("kestrel?", client=stub["client"]) == ""


def test_unreachable_server_never_raises():
    client = Grimoire("http://127.0.0.1:1")
    assert context_for("kestrel?", client=client) == ""
    assert ContextSession(client).context_for("kestrel?") == ""


def test_default_client_reads_environment(stub, monkeypatch):
    monkeypatch.setenv("GRIMOIRE_URL", stub["client"].url)
    monkeypatch.setenv("GRIMOIRE_AUTH_TOKEN", "envtok")
    assert context_for("kestrel?") == "reference"
    assert stub["calls"][0]["auth"] == "Bearer envtok"


def test_session_deduplicates_like_the_hook(stub):
    clock = [100.0]
    session = ContextSession(stub["client"], clock=lambda: clock[0])
    assert session.context_for("How does kestrel deployment work?") == "reference"
    clock[0] = 101
    assert session.context_for("How does kestrel deployment work?") == ""   # repeat <30s
    assert len(stub["calls"]) == 1
    clock[0] = 102
    stub["reply"] = {"context": "", "keys": []}
    session.context_for("Kestrel deployment certificates?")
    assert stub["calls"][-1]["query"]["exclude"] == [KEY]                    # fact dedup
    clock[0] = 100 + 1801
    session.context_for("Kestrel deployment ports?")
    assert stub["calls"][-1]["query"]["exclude"] == [""]                     # 30-min expiry
    session.context_for("something else entirely here")
    session.reset()
    clock[0] += 1
    session.context_for("Kestrel deployment ports?")
    assert stub["calls"][-1]["query"]["exclude"] == [""]


def test_session_retries_after_a_failed_lookup(stub):
    session = ContextSession(stub["client"], clock=lambda: 100.0)
    stub["raw"] = b"garbage"
    assert session.context_for("kestrel deployment?") == ""
    stub["raw"] = None
    assert session.context_for("kestrel deployment?") == "reference"
