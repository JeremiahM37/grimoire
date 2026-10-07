"""Tests for the bank client and the OpenAI-compatible memory wrapper.

Like test_client.py these run against a stub HTTP server: under test is the
client's half of the contract — method, path, body, and how replies and
refusals are turned into results. The server's half is tested in Go.
"""

from __future__ import annotations

import asyncio
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from types import SimpleNamespace

import pytest
from grimoire_client import (
    AsyncBank,
    Grimoire,
    GrimoireError,
    ModelRequired,
    NotAvailable,
    NotFound,
    with_memory,
)
from grimoire_client.openai_memory import format_memories


class Stub:
    """Routes keyed by (method, path-without-query) -> (status, body)."""

    def __init__(self) -> None:
        self.calls: list[dict] = []
        self.routes: dict[tuple[str, str], tuple[int, object]] = {}
        self.default: tuple[int, object] = (200, {})

    def on(self, method: str, path: str, body: object = None, status: int = 200) -> None:
        self.routes[(method, path)] = (status, {} if body is None else body)

    def find(self, method: str, path: str) -> list[dict]:
        return [c for c in self.calls if c["method"] == method and c["path"].split("?")[0] == path]


@pytest.fixture
def stub():
    rec = Stub()

    class Handler(BaseHTTPRequestHandler):
        def _handle(self) -> None:
            length = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(length) if length else b""
            rec.calls.append({"method": self.command, "path": self.path,
                              "body": json.loads(raw) if raw else None,
                              "headers": dict(self.headers)})
            status, body = rec.routes.get((self.command, self.path.split("?")[0]), rec.default)
            if isinstance(body, str):
                payload, ctype = body.encode(), "text/plain; charset=utf-8"
            else:
                payload, ctype = json.dumps(body).encode(), "application/json"
            self.send_response(status)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)

        do_GET = do_POST = do_PATCH = do_DELETE = _handle

        def log_message(self, *args) -> None:
            pass

    httpd = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    rec.url = f"http://127.0.0.1:{httpd.server_port}"
    try:
        yield rec
    finally:
        httpd.shutdown()
        httpd.server_close()


@pytest.fixture
def g(stub):
    return Grimoire(stub.url, token="tok", agent="pytest-agent")


# ---- bank client -----------------------------------------------------


def test_list_and_create(stub, g):
    stub.on("GET", "/api/banks", {"banks": [{"bank_id": "a"}]})
    assert g.banks.list() == [{"bank_id": "a"}]
    stub.on("POST", "/api/banks", {"bank_id": "b"}, 201)
    g.banks.create("b", mission="m", disposition={"skepticism": 4, "literalism": 3, "empathy": 3})
    call = stub.find("POST", "/api/banks")[-1]
    assert call["body"] == {"bank_id": "b", "mission": "m",
                            "disposition": {"skepticism": 4, "literalism": 3, "empathy": 3}}
    assert call["headers"]["Authorization"] == "Bearer tok"
    assert call["headers"]["X-Grimoire-Agent"] == "pytest-agent"


def test_bank_id_with_colon_is_one_segment(stub, g):
    g.bank("coding-agent:grimoire").profile()
    assert stub.calls[-1]["path"] == "/api/banks/coding-agent%3Agrimoire"


def test_retain_builds_one_item(stub, g):
    stub.on("POST", "/api/banks/s/memories", {"success": True})
    g.bank("s").retain("Dana moved to Lyon.", document_id="d1", timestamp="2024-01-02T00:00:00Z",
                       context="chat", tags=["x"], metadata={"k": "v"}, mode="chunks")
    assert stub.calls[-1]["body"] == {
        "items": [{"content": "Dana moved to Lyon.", "document_id": "d1",
                   "timestamp": "2024-01-02T00:00:00Z", "context": "chat", "tags": ["x"],
                   "metadata": {"k": "v"}}],
        "mode": "chunks",
    }


def test_retain_requires_content():
    with pytest.raises(ValueError):
        Grimoire().bank("s").retain()


def test_async_retain_falls_back_to_sync_when_unsupported(g):
    bank = g.bank("s")
    sent = []

    def fake(method, path, body=None):
        sent.append(body)
        if body.get("async"):
            raise GrimoireError(400, "async retain is not available yet; send async=false")
        return {"success": True}

    bank._req = fake  # type: ignore[method-assign]
    assert bank.retain("x", async_=True) == {"success": True, "async_fallback": True}
    assert [b.get("async") for b in sent] == [True, None]


def test_other_400_is_not_swallowed(g):
    bank = g.bank("s")

    def fake(method, path, body=None):
        raise GrimoireError(400, "items must hold 1..500 entries")

    bank._req = fake  # type: ignore[method-assign]
    with pytest.raises(GrimoireError):
        bank.retain("x", async_=True)


def test_recall_body(stub, g):
    stub.on("POST", "/api/banks/s/memories/recall", {"results": []})
    g.bank("s").recall("q", budget="low", max_tokens=100, types=["world"], tags=["a"],
                       tags_match="all", include_entities=False, include_chunks=500, trace=True)
    assert stub.calls[-1]["body"] == {
        "query": "q", "budget": "low", "max_tokens": 100, "types": ["world"], "tags": ["a"],
        "tags_match": "all", "include": {"entities": None, "chunks": {"max_tokens": 500}},
        "trace": True,
    }


def test_list_memories_and_force_delete(stub, g):
    stub.on("GET", "/api/banks/s/memories", {"items": [], "total": 0})
    g.bank("s").list_memories(type="world", authority="human", limit=5)
    assert stub.calls[-1]["path"] == "/api/banks/s/memories?type=world&authority=human&limit=5"
    g.bank("s").delete_memory("f1", force=True)
    assert stub.calls[-1]["method"] == "DELETE"
    assert stub.calls[-1]["path"] == "/api/banks/s/memories/f1?force=true"
    g.bank("s").delete_document("doc/1")
    assert stub.calls[-1]["path"] == "/api/banks/s/documents/doc%2F1"


def test_missing_route_is_not_available_but_missing_bank_is_not_found(stub, g):
    stub.on("POST", "/api/banks/s/reflect", "404 page not found\n", 404)
    with pytest.raises(NotAvailable):
        g.bank("s").reflect("why?")
    stub.on("POST", "/api/banks/s/reflect", {"detail": "no such bank"}, 404)
    with pytest.raises(NotFound) as info:
        g.bank("s").reflect("why?")
    assert not isinstance(info.value, NotAvailable)
    stub.on("DELETE", "/api/banks/s/operations/o1", "Method Not Allowed", 405)
    with pytest.raises(NotAvailable):
        g.bank("s").cancel_operation("o1")


def test_reflect_body_and_shape(stub, g):
    stub.on("POST", "/api/banks/s/reflect", {"text": "because", "mode": "extractive",
                                             "based_on": {"memories": [], "observations": [],
                                                          "mental_models": [], "directives": []},
                                             "trace": None})
    out = g.bank("s").reflect("why?", budget="low")
    assert out["text"] == "because" and out["mode"] == "extractive"
    assert stub.calls[-1]["body"] == {"query": "why?", "budget": "low", "include": {"facts": {}}}
    g.bank("s").reflect("why?", types=["world"], tags=["a"], tags_match="any", include_tool_calls=True,
                        exclude_mental_models=True)
    assert stub.calls[-1]["body"] == {"query": "why?", "fact_types": ["world"], "tags": ["a"],
                                      "tags_match": "any", "exclude_mental_models": True,
                                      "include": {"facts": {}, "tool_calls": {}}}
    g.bank("s").reflect("why?", include_facts=False, trace=True)
    assert stub.calls[-1]["body"] == {"query": "why?", "trace": True}


def test_mental_model_calls(stub, g):
    bank = g.bank("s")
    stub.on("POST", "/api/banks/s/mental-models",
            {"mental_model": {"id": "people/dana"}, "mental_model_id": "people/dana", "operation_id": None}, 201)
    out = bank.create_mental_model("Dana", "Who is Dana?", folder="people", id="dana", tags=["t"],
                                   refresh="auto", budget="mid", fact_types=["world"])
    assert out["mental_model_id"] == "people/dana" and out["operation_id"] is None
    assert stub.calls[-1]["body"] == {"name": "Dana", "question": "Who is Dana?", "id": "dana",
                                      "folder": "people", "tags": ["t"], "refresh": "auto",
                                      "budget": "mid", "fact_types": ["world"]}
    # The old keyword names still work.
    bank.create_mental_model("Ctx", source_query="what matters?", refresh_after_consolidation=False)
    assert stub.calls[-1]["body"] == {"name": "Ctx", "question": "what matters?", "refresh": "manual"}
    with pytest.raises(ValueError):
        bank.create_mental_model("No question")

    bank.refresh_mental_model("people/dana")
    assert stub.calls[-1]["path"] == "/api/banks/s/mental-models/people%2Fdana/refresh"
    bank.accept_proposal("people/dana")
    assert stub.calls[-1]["path"] == "/api/banks/s/mental-models/people%2Fdana/proposal/accept"
    bank.reject_proposal("m1")
    assert stub.calls[-1]["path"] == "/api/banks/s/mental-models/m1/proposal/reject"
    bank.update_mental_model("people/dana", body="My own words.")
    assert stub.calls[-1]["method"] == "PATCH" and stub.calls[-1]["body"] == {"body": "My own words."}
    bank.move_mental_model("people/dana", "team")
    assert stub.calls[-1]["body"] == {"folder": "team"}
    bank.mental_model_history("people/dana", "3")
    assert stub.calls[-1]["path"] == "/api/banks/s/mental-models/people%2Fdana/history/3"
    bank.mental_models(folder="people", detail=True, tags=["a", "b"])
    assert stub.calls[-1]["path"] == "/api/banks/s/mental-models?tags=a%2Cb&folder=people&detail=full"

    stub.on("GET", "/api/banks/s/mental-models-tree", {"roots": [{"kind": "folder", "name": "people"}]})
    assert bank.mental_model_tree()[0]["name"] == "people"
    stub.on("GET", "/api/banks/s/mental-models-export", {"files": [{"path": "index.md", "content": "#"}]})
    assert bank.export_mental_models()[0]["path"] == "index.md"
    stub.on("GET", "/api/banks/s/mental-models-export", "# Index\n")
    assert bank.export_mental_models(markdown=True) == "# Index\n"
    assert stub.calls[-1]["path"] == "/api/banks/s/mental-models-export?format=markdown"


def test_model_required_is_its_own_error(stub, g):
    stub.on("POST", "/api/banks/s/mental-models/m/refresh",
            {"detail": "model_required: this needs a language model and none is configured",
             "code": "model_required"}, 409)
    with pytest.raises(ModelRequired) as info:
        g.bank("s").refresh_mental_model("m")
    assert info.value.status == 409 and "model_required" in info.value.message
    stub.on("POST", "/api/banks/s/consolidate", {"detail": "bank already exists"}, 409)
    with pytest.raises(GrimoireError) as other:
        g.bank("s").consolidate()
    assert not isinstance(other.value, ModelRequired)


def test_observations_directives_and_webhooks(stub, g):
    bank = g.bank("s")
    stub.on("GET", "/api/banks/s/observations", {"items": [{"id": "o1"}], "total": 1, "history": []})
    assert bank.observations(authority="human", include_history=True)["items"][0]["id"] == "o1"
    assert stub.calls[-1]["path"] == "/api/banks/s/observations?authority=human&include_history=true"
    bank.delete_observation("o1", force=True)
    assert stub.calls[-1]["path"] == "/api/banks/s/observations/o1?force=true"
    bank.clear_observations()
    assert (stub.calls[-1]["method"], stub.calls[-1]["path"]) == ("DELETE", "/api/banks/s/observations")

    stub.on("POST", "/api/banks/s/directives", {"id": "d1", "text": "Be brief."}, 201)
    bank.create_directive("Be brief.", name="Brevity", priority=2, tags=["x"])
    assert stub.calls[-1]["body"] == {"text": "Be brief.", "name": "Brevity", "tags": ["x"], "priority": 2}
    bank.update_directive("d1", is_active=False)
    assert stub.calls[-1]["body"] == {"is_active": False}
    bank.directives()
    assert stub.calls[-1]["path"] == "/api/banks/s/directives?active_only=false"

    bank.create_webhook("https://example.com/h", event_types=["retain.completed"])
    assert stub.calls[-1]["body"] == {"url": "https://example.com/h", "enabled": True,
                                      "events": ["retain.completed"]}
    stub.on("GET", "/api/banks/s/webhooks/w1/deliveries", {"items": [{"id": "x"}]})
    assert bank.webhook_deliveries("w1", limit=5) == [{"id": "x"}]


def test_operations_and_wait(stub, g):
    bank = g.bank("s")
    stub.on("GET", "/api/banks/s/operations",
            {"bank_id": "s", "operations": [{"id": "op-1", "kind": "retain", "type": "retain",
                                             "status": "queued"}], "total": 1})
    out = bank.operations(status="queued", type="retain")
    assert out["operations"][0]["kind"] == "retain" and out["total"] == 1
    assert stub.calls[-1]["path"] == "/api/banks/s/operations?status=queued&type=retain"

    states = iter(["queued", "running", "completed"])
    bank.operation = lambda op_id: {"id": op_id, "status": next(states)}  # type: ignore[method-assign]
    assert bank.wait_operation("op-1", interval=0)["status"] == "completed"
    bank.operation = lambda op_id: {"id": op_id, "status": "running"}  # type: ignore[method-assign]
    with pytest.raises(TimeoutError):
        bank.wait_operation("op-1", timeout=0, interval=0)


def test_templates_and_import(stub, g):
    stub.on("GET", "/api/bank-templates", {"templates": [{"id": "assistant"}]})
    assert g.banks.templates() == [{"id": "assistant"}]
    g.banks.template("coding-agent")
    assert stub.calls[-1]["path"] == "/api/bank-templates/coding-agent"
    g.bank("s").import_template(template="support", dry_run=True)
    assert stub.calls[-1]["path"] == "/api/banks/s/import?dry_run=true"
    assert stub.calls[-1]["body"] == {"template": "support"}
    g.bank("s").import_template({"version": "1", "directives": [{"text": "x"}]})
    assert stub.calls[-1]["body"] == {"manifest": {"version": "1", "directives": [{"text": "x"}]}}
    with pytest.raises(ValueError):
        g.bank("s").import_template()


def test_templates_not_available(stub, g):
    stub.on("GET", "/api/bank-templates", "404 page not found\n", 404)
    with pytest.raises(NotAvailable):
        g.banks.templates()


def test_async_bank(stub, g):
    stub.on("POST", "/api/banks/s/memories/recall", {"results": [{"id": "f", "text": "t"}]})
    bank = g.async_bank("s")
    assert isinstance(bank, AsyncBank)
    out = asyncio.run(bank.recall("q"))
    assert out["results"][0]["id"] == "f"


def test_coding_agent_surfaces(stub, g):
    stub.on("GET", "/api/banks/s/index", {"items": [], "total": 0})
    stub.on("GET", "/api/banks/s/timeline", {"entries": []})
    stub.on("GET", "/api/banks/s/lookup", {"items": [], "missing": []})
    stub.on("GET", "/api/banks/s/context", {"context": "x", "chars": 1})
    stub.on("POST", "/api/banks/s/sessions/a%3A1/digest", {"written": True})
    bank = g.bank("s")
    bank.index("cache", types=["fact"], limit=5)
    bank.timeline("#f3a9c1b2", before=2)
    bank.get_entries(["#f3a9c1b2", "o77aa001"])
    bank.context(max_chars=3000, source="resume")
    bank.write_digest("a:1", [{"speaker": "user", "text": "hi"}], use_model=True)
    assert stub.find("GET", "/api/banks/s/index")[0]["path"] == "/api/banks/s/index?q=cache&types=fact&limit=5"
    assert stub.find("GET", "/api/banks/s/timeline")[0]["path"] == "/api/banks/s/timeline?anchor=%23f3a9c1b2&before=2"
    assert stub.find("GET", "/api/banks/s/lookup")[0]["path"] == "/api/banks/s/lookup?ids=%23f3a9c1b2%2Co77aa001"
    assert stub.find("GET", "/api/banks/s/context")[0]["path"] == "/api/banks/s/context?max_chars=3000&source=resume"
    body = stub.find("POST", "/api/banks/s/sessions/a%3A1/digest")[0]["body"]
    assert body["use_model"] is True and body["turns"][0]["text"] == "hi"


# ---- memory wrapper ------------------------------------------------------


class FakeCompletions:
    def __init__(self, reply="Sure, Lyon it is.") -> None:
        self.calls: list[dict] = []
        self.reply = reply

    def create(self, **kwargs):
        self.calls.append(kwargs)
        if kwargs.get("stream"):
            return iter([{"choices": [{"delta": {"content": p}}]} for p in ("Ly", "on")])
        return SimpleNamespace(choices=[SimpleNamespace(message=SimpleNamespace(content=self.reply))])


class AsyncFakeCompletions(FakeCompletions):
    async def create(self, **kwargs):
        return FakeCompletions.create(self, **kwargs)


def fake_client(completions):
    return SimpleNamespace(chat=SimpleNamespace(completions=completions), models="passthrough")


FACT = {"id": "f1", "text": "Dana lives in Lyon", "type": "world",
        "occurred_start": "2024-05-01", "occurred_end": "2024-05-31", "authority": "agent"}


def test_wrapper_injects_memories_and_retains_the_exchange(stub, g):
    stub.on("POST", "/api/banks/u/memories/recall", {"results": [FACT]})
    stub.on("POST", "/api/banks/u/memories", {"success": True})
    completions = FakeCompletions()
    llm = with_memory(fake_client(completions), g.bank("u"), session_id="chat-7", max_tokens=512,
                      types=["world"], tags=["chat"])
    messages = [{"role": "system", "content": "Be brief."},
                {"role": "user", "content": "Where does Dana live?"}]
    llm.chat.completions.create(model="m1", messages=messages)

    recall = stub.find("POST", "/api/banks/u/memories/recall")[-1]["body"]
    assert recall == {"query": "Where does Dana live?", "budget": "mid", "max_tokens": 512,
                      "types": ["world"]}
    sent = completions.calls[-1]["messages"]
    assert sent[0]["role"] == "system"
    assert sent[0]["content"].startswith("Be brief.\n\n# Relevant memories")
    assert "1. [WORLD] (occurred: 2024-05-01 to 2024-05-31) Dana lives in Lyon" in sent[0]["content"]
    assert messages[0]["content"] == "Be brief."  # the caller's list is not mutated

    item = stub.find("POST", "/api/banks/u/memories")[-1]["body"]["items"][0]
    assert item["content"] == "USER: Where does Dana live?\n\nASSISTANT: Sure, Lyon it is."
    assert item["document_id"] == "chat-7"
    assert item["update_mode"] == "append"
    assert item["context"] == "conversation:openai:m1"
    assert item["metadata"] == {"source": "openai-wrapper", "model": "m1"}
    assert item["tags"] == ["chat"]
    assert item["timestamp"].endswith("Z")
    assert llm.models == "passthrough"


def test_no_system_message_gets_one_inserted(stub, g):
    stub.on("POST", "/api/banks/u/memories/recall", {"results": [FACT]})
    completions = FakeCompletions()
    with_memory(fake_client(completions), g.bank("u"), store=False).chat.completions.create(
        model="m", messages=[{"role": "user", "content": [{"type": "text", "text": "Dana?"}]}])
    sent = completions.calls[-1]["messages"]
    assert sent[0]["role"] == "system" and sent[0]["content"].startswith("# Relevant memories")
    assert stub.find("POST", "/api/banks/u/memories/recall")[-1]["body"]["query"] == "Dana?"
    assert not stub.find("POST", "/api/banks/u/memories")


def test_recall_failure_fails_open(stub, g):
    stub.on("POST", "/api/banks/u/memories/recall", {"detail": "boom"}, 500)
    stub.on("POST", "/api/banks/u/memories", {"success": True})
    completions = FakeCompletions()
    llm = with_memory(fake_client(completions), g.bank("u"))
    out = llm.chat.completions.create(model="m", messages=[{"role": "user", "content": "hi there"}])
    assert out.choices[0].message.content
    assert completions.calls[-1]["messages"] == [{"role": "user", "content": "hi there"}]


def test_unreachable_server_fails_open():
    completions = FakeCompletions()
    llm = with_memory(fake_client(completions), Grimoire("http://127.0.0.1:9", timeout=0.5).bank("u"))
    llm.chat.completions.create(model="m", messages=[{"role": "user", "content": "hi"}])
    assert len(completions.calls) == 1


def test_grimoire_kwargs_are_stripped_and_applied(stub, g):
    completions = FakeCompletions()
    llm = with_memory(fake_client(completions), g.bank("u"))
    llm.chat.completions.create(model="m", messages=[{"role": "user", "content": "hi"}],
                                grimoire_inject=False, grimoire_store=False, temperature=0)
    assert completions.calls[-1] == {"model": "m", "messages": [{"role": "user", "content": "hi"}],
                                     "temperature": 0}
    assert not stub.calls
    with pytest.raises(TypeError, match="grimoire_bogus"):
        llm.chat.completions.create(model="m", messages=[], grimoire_bogus=1)


def test_append_refused_falls_back_to_plain_retain(stub, g):
    stub.on("POST", "/api/banks/u/memories/recall", {"results": []})
    calls = []

    bank = g.bank("u")

    def fake(method, path, body=None):
        if path == "/memories":
            calls.append(body)
            if body["items"][0].get("update_mode") == "append":
                raise GrimoireError(400, "unknown update_mode")
            return {"success": True}
        return {"results": []}

    bank._req = fake  # type: ignore[method-assign]
    with_memory(fake_client(FakeCompletions()), bank, session_id="s1").chat.completions.create(
        model="m", messages=[{"role": "user", "content": "hello"}])
    assert [c["items"][0].get("update_mode") for c in calls] == ["append", None]


def test_background_retain_collects_errors(stub, g):
    stub.on("POST", "/api/banks/u/memories/recall", {"results": []})
    stub.on("POST", "/api/banks/u/memories", {"detail": "disk full"}, 500)
    llm = with_memory(fake_client(FakeCompletions()), g.bank("u"), background=True)
    llm.chat.completions.create(model="m", messages=[{"role": "user", "content": "hello"}])
    llm.flush(5)
    errors = llm.pending_errors()
    assert len(errors) == 1 and "disk full" in str(errors[0])
    assert llm.pending_errors() == []


def test_stream_is_retained_when_exhausted(stub, g):
    stub.on("POST", "/api/banks/u/memories/recall", {"results": []})
    stub.on("POST", "/api/banks/u/memories", {"success": True})
    llm = with_memory(fake_client(FakeCompletions()), g.bank("u"))
    stream = llm.chat.completions.create(model="m", stream=True,
                                         messages=[{"role": "user", "content": "where?"}])
    assert not stub.find("POST", "/api/banks/u/memories")
    assert len(list(stream)) == 2
    item = stub.find("POST", "/api/banks/u/memories")[-1]["body"]["items"][0]
    assert item["content"] == "USER: where?\n\nASSISTANT: Lyon"


def test_async_client(stub, g):
    stub.on("POST", "/api/banks/u/memories/recall", {"results": [FACT]})
    stub.on("POST", "/api/banks/u/memories", {"success": True})
    completions = AsyncFakeCompletions()
    llm = with_memory(fake_client(completions), g.async_bank("u"))
    assert llm.is_async

    async def go():
        return await llm.chat.completions.create(
            model="m", messages=[{"role": "user", "content": "Dana?"}])

    out = asyncio.run(go())
    assert out.choices[0].message.content == "Sure, Lyon it is."
    assert "Dana lives in Lyon" in completions.calls[-1]["messages"][0]["content"]
    assert stub.find("POST", "/api/banks/u/memories")


def test_format_marks_human_and_disputed():
    text = format_memories([{"text": "a", "authority": "human"},
                            {"text": "b", "type": "experience", "disputed_by": "h1"},
                            {"text": "  "}])
    assert "1. [WORLD] a (written by a person)" in text
    assert "2. [EXPERIENCE] b (disputed by a person's correction)" in text
    assert "3." not in text
    assert format_memories([]) == ""
