"""Tests for the Python client.

They run against a stub HTTP server rather than a real Grimoire: what is being
tested here is the client's half of the contract — the method, the path, the
query string, the body it builds and the objects it parses back — and a real
server would make those assertions harder to see, not easier. The server's own
half is tested in go/internal/api.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from grimoire_client import (
    Grimoire,
    GrimoireError,
    Memory,
    NotFound,
    Unauthorized,
    VaultLocked,
)


class Recorder:
    """What the stub saw, and what it should answer with."""

    def __init__(self) -> None:
        self.calls: list[dict] = []
        self.reply: object = {}
        self.status = 200

    @property
    def last(self) -> dict:
        assert self.calls, "no request was made"
        return self.calls[-1]


@pytest.fixture
def server():
    rec = Recorder()

    class Handler(BaseHTTPRequestHandler):
        def _handle(self) -> None:
            length = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(length) if length else b""
            rec.calls.append(
                {
                    "method": self.command,
                    "path": self.path,
                    "body": json.loads(raw) if raw else None,
                    "headers": dict(self.headers),
                }
            )
            payload = json.dumps(rec.reply).encode()
            self.send_response(rec.status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = _handle

        def log_message(self, *args) -> None:  # keep pytest output readable
            pass

    httpd = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    rec.url = f"http://127.0.0.1:{httpd.server_port}"
    try:
        yield rec
    finally:
        httpd.shutdown()
        httpd.server_close()


@pytest.fixture
def client(server):
    return Grimoire(server.url, token="test-token", agent="pytest-agent")


# ---- memory ------------------------------------------------------------


def test_add_posts_the_fact_with_its_agent(server, client):
    server.reply = {"op": "ADD", "id": "a1", "results": [{"op": "ADD", "id": "a1"}]}
    result = client.add("the user prefers tabs", topic="prefs")

    assert server.last["method"] == "POST"
    assert server.last["path"] == "/api/memory"
    assert server.last["body"] == {
        "text": "the user prefers tabs",
        "agent": "pytest-agent",
        "topic": "prefs",
    }
    assert result.op == "ADD"
    assert result.stored is True


def test_add_sends_only_the_scope_fields_it_was_given(server, client):
    # An empty string is a value the server validates and rejects; a field
    # nobody set must not be sent at all.
    server.reply = {"op": "ADD"}
    client.add("x")
    assert set(server.last["body"]) == {"text", "agent"}


def test_add_forwards_scope_lifetime_and_pinning(server, client):
    server.reply = {"op": "ADD"}
    client.add(
        "x",
        session="run-9",
        category="gotcha",
        expires_in="72h",
        immutable=True,
        task="ticket-4",
    )
    body = server.last["body"]
    assert body["session"] == "run-9"
    assert body["category"] == "gotcha"
    assert body["expires_in"] == "72h"
    assert body["immutable"] is True
    assert body["task"] == "ticket-4"


def test_infer_false_is_sent_and_true_is_the_unstated_default(server, client):
    server.reply = {"op": "ADD"}
    client.add("x", infer=False)
    assert server.last["body"]["infer"] is False

    client.add("x")
    assert "infer" not in server.last["body"]


def test_add_rejects_unknown_arguments(client):
    with pytest.raises(TypeError, match="user_id"):
        client.remember("x", user_id="nope")


def test_result_reports_what_happened(server, client):
    server.reply = {"op": "UPDATE", "id": "new", "results": [
        {"op": "UPDATE", "id": "new", "target": "old", "why": "supersedes: ..."}]}
    result = client.add("the user prefers tabs")
    assert result.op == "UPDATE"
    assert result.target == "old"
    assert result.stored is True

    server.reply = {"op": "NOOP", "results": [{"op": "NOOP", "target": "old"}]}
    assert client.add("the user prefers tabs").stored is False


def test_search_builds_the_query_string(server, client):
    server.reply = []
    client.search(
        "indentation",
        limit=5,
        agent="claude",
        session="run-1",
        category="preference",
        include_superseded=True,
        explain=True,
    )
    path = server.last["path"]
    for fragment in (
        "q=indentation", "limit=5", "agent=claude", "session=run-1",
        "category=preference", "include_superseded=1", "explain=1",
    ):
        assert fragment in path, path


def test_search_omits_flags_that_are_off(server, client):
    server.reply = []
    client.search("x")
    assert "include_superseded" not in server.last["path"]
    assert "explain" not in server.last["path"]


def test_search_parses_facts(server, client):
    server.reply = [
        {
            "id": "a1", "text": "the user prefers tabs", "path": "memory/prefs.md",
            "agent": "claude", "session": "run-1", "score": 0.82,
            "scores": {"semantic": 0.7, "keyword": 1.0, "entity": 0.0, "recency": 1.0},
        }
    ]
    (fact,) = client.search("indentation", explain=True)
    assert isinstance(fact, Memory)
    assert fact.text == "the user prefers tabs"
    assert fact.path == "memory/prefs.md"
    assert fact.score == pytest.approx(0.82)
    assert fact.scores["keyword"] == 1.0
    assert fact.superseded is False


def test_unknown_server_fields_do_not_break_parsing(server, client):
    # A server that grows a field must not break a client that has not been
    # updated to know about it.
    server.reply = [{"id": "a1", "text": "x", "brand_new_field": 42}]
    (fact,) = client.search("x")
    assert fact.text == "x"


def test_superseded_fact_is_marked(server, client):
    server.reply = [{"id": "a1", "text": "old belief", "superseded_by": "b2"}]
    (fact,) = client.search("x", include_superseded=True)
    assert fact.superseded is True


def test_get_all_asks_for_everything_in_scope(server, client):
    server.reply = []
    client.get_all(agent="claude")
    assert "limit=200" in server.last["path"]
    assert "q=" not in server.last["path"]


def test_history_asks_as_of_an_instant(server, client):
    server.reply = []
    client.history("2026-08-14T09:00:00Z")
    assert "as_of=2026-08-14T09%3A00%3A00Z" in server.last["path"]


def test_add_many_defaults_each_items_agent(server, client):
    server.reply = {"results": [{"op": "ADD"}, {"op": "UPDATE"}]}
    results = client.add_many([
        {"text": "one", "topic": "b"},
        {"text": "two", "agent": "other-agent"},
    ])
    items = server.last["body"]["items"]
    assert items[0]["agent"] == "pytest-agent"
    assert items[1]["agent"] == "other-agent", "an explicit agent must win"
    assert len(results) == 2


def test_add_many_does_not_mutate_the_callers_dicts(server, client):
    server.reply = {"results": []}
    original = {"text": "one"}
    client.add_many([original])
    assert original == {"text": "one"}


def test_update_sends_only_the_fields_given(server, client):
    server.reply = {}
    client.update("memory/prefs.md", "a1", text="new wording")
    assert server.last["method"] == "PATCH"
    assert server.last["body"] == {
        "path": "memory/prefs.md", "id": "a1", "text": "new wording"}


def test_update_can_clear_a_pin(server, client):
    # False is a value, not an absence: `if value` would drop it.
    server.reply = {}
    client.update("memory/prefs.md", "a1", immutable=False)
    assert server.last["body"]["immutable"] is False


def test_delete_is_a_soft_retraction_by_default(server, client):
    server.reply = {}
    client.delete("memory/prefs.md", "a1")
    assert server.last["method"] == "DELETE"
    assert "hard" not in server.last["path"]
    assert "agent=pytest-agent" in server.last["path"]

    client.delete("memory/prefs.md", "a1", hard=True)
    assert "hard=1" in server.last["path"]


def test_scopes_and_export(server, client):
    server.reply = {"agents": {"claude": 3}}
    assert client.scopes()["agents"]["claude"] == 3
    assert server.last["path"] == "/api/memory/facets"

    server.reply = {"count": 2, "entries": []}
    client.export(agent="claude")
    assert "/api/memory/export?agent=claude" == server.last["path"]


def test_export_with_no_filters_sends_no_query_string(server, client):
    server.reply = {"count": 0, "entries": []}
    client.export()
    assert server.last["path"] == "/api/memory/export"


# ---- knowledge ---------------------------------------------------------


def test_knowledge_surface(server, client):
    server.reply = {"answer": "8443", "citations": []}
    client.ask("what port", limit=3)
    assert server.last["path"] == "/api/ask"
    assert server.last["body"] == {"question": "what port", "k": 3}

    server.reply = []
    client.search_notes("gateway")
    assert "/api/search?q=gateway" in server.last["path"]

    server.reply = {"body": "# x"}
    client.read_note("sub/note.md")
    assert server.last["path"] == "/api/notes/sub/note.md"

    server.reply = {}
    client.write_note("a.md", "# A", {"title": "A"})
    assert server.last["method"] == "PUT"
    assert server.last["body"] == {"body": "# A", "frontmatter": {"title": "A"}}


def test_paths_with_spaces_are_escaped(server, client):
    server.reply = {}
    client.read_note("my notes/a b.md")
    assert " " not in server.last["path"]
    assert "my%20notes/a%20b.md" in server.last["path"]


# ---- transport ---------------------------------------------------------


def test_token_is_sent_as_a_bearer(server, client):
    server.reply = {}
    client.health()
    assert server.last["headers"]["Authorization"] == "Bearer test-token"


def test_no_token_means_no_header(server):
    client = Grimoire(server.url)
    server.reply = {}
    client.health()
    assert "Authorization" not in server.last["headers"]


@pytest.mark.parametrize(
    "status,expected",
    [(404, NotFound), (401, Unauthorized), (403, Unauthorized),
     (423, VaultLocked), (500, GrimoireError)],
)
def test_errors_map_to_types(server, client, status, expected):
    server.status = status
    server.reply = {"error": "nope"}
    with pytest.raises(expected) as excinfo:
        client.health()
    assert excinfo.value.status == status
    assert "nope" in excinfo.value.message


def test_unreachable_server_is_a_grimoire_error():
    client = Grimoire("http://127.0.0.1:1", timeout=1.0)
    with pytest.raises(GrimoireError) as excinfo:
        client.health()
    assert excinfo.value.status == 0
    assert "cannot reach" in excinfo.value.message


def test_trailing_slash_in_the_url_does_not_double_up(server):
    client = Grimoire(server.url + "/")
    server.reply = {}
    client.health()
    assert server.last["path"] == "/api/health"


def test_langgraph_delete_rechecks_the_key_it_was_given(server):
    # A server that ignores the `task` filter answers with every fact in the
    # note. The store then deletes what it is handed — so it has to check the
    # key itself before deleting anything.
    pytest_langgraph = pytest.importorskip(
        "langgraph", reason="langgraph is not installed")
    from grimoire_client.langgraph import GrimoireStore

    del pytest_langgraph
    store = GrimoireStore(Grimoire(server.url))
    server.reply = [
        {"id": "keep", "text": "another key's fact", "path": "memory/x.md", "task": "other"},
        {"id": "drop", "text": "the target", "path": "memory/x.md", "task": "target"},
    ]
    store._delete("x", "target")

    deleted = [c for c in server.calls if c["method"] == "DELETE"]
    assert len(deleted) == 1, "the store deleted a fact belonging to another key"
    assert "id=drop" in deleted[0]["path"]


def test_scope_is_forwarded(server, client):
    server.reply = {"op": "ADD"}
    client.add("x", scope="topic")
    assert server.last["body"]["scope"] == "topic"

    client.add("x")
    assert "scope" not in server.last["body"], "the default must stay unstated"


def test_feedback_posts_a_verdict(server, client):
    server.reply = {"helpful": 1, "unhelpful": 0, "usefulness": 0.66}
    client.feedback("memory/ops.md", "a1", helpful=True)
    assert server.last["path"] == "/api/memory/feedback"
    assert server.last["body"] == {"path": "memory/ops.md", "id": "a1", "helpful": True}


def test_feedback_counts_are_parsed(server, client):
    server.reply = [{"id": "a1", "text": "x", "helpful": 3, "unhelpful": 1}]
    (fact,) = client.search("x")
    assert (fact.helpful, fact.unhelpful) == (3, 1)


def test_graph_builds_its_query(server, client):
    server.reply = {"seed": "priya sharma", "nodes": [], "edges": [], "entries": []}
    client.graph("priya", depth=2, limit=10, session="run-1")
    path = server.last["path"]
    for fragment in ("entity=priya", "depth=2", "limit=10", "session=run-1"):
        assert fragment in path, path


def test_graph_without_an_entity_asks_for_the_overview(server, client):
    server.reply = {"seed": "", "nodes": [], "edges": [], "entries": []}
    client.graph()
    assert "entity=" not in server.last["path"]
