"""The bank client against a real server with no language model.

test_banks.py pins the requests the client sends; this checks the replies it
gets back from the server it is written for — the shapes reflect, mental
models, directives, operations and templates actually have — on the paths
that need no model: chunk retain, recall, extractive reflect, a mental model a
person wrote, and model_required where a model is needed.
"""

from __future__ import annotations

import uuid

import pytest
from grimoire_client import Grimoire, ModelRequired


@pytest.fixture
def bank(live_server):
    g = Grimoire(live_server, agent="pytest-live")
    bank_id = "live-" + uuid.uuid4().hex[:8]
    return g, g.bank(bank_id)


def test_offline_bank_round_trip(bank):
    g, b = bank
    templates = {t["id"] for t in g.banks.templates()}
    assert {"assistant", "coding-agent", "support", "research", "plain-retrieval"} <= templates
    assert g.banks.template("plain-retrieval")["manifest"]["bank"]["config"]["retain_extraction_mode"] == "chunks"

    imported = b.import_template(template="plain-retrieval")
    assert imported["bank_created"] is True

    out = b.retain("The kestrel gateway uses copper certificates.", document_id="d1", mode="chunks")
    assert out["documents"][0]["document_id"] == "d1"
    recall = b.recall("copper certificates", trace=True)
    assert "copper" in recall["results"][0]["text"] and recall["trace"]

    reflect = b.reflect("What does the kestrel gateway use?")
    assert reflect["mode"] == "extractive"
    assert reflect["based_on"]["memories"]
    assert reflect.get("trace") is None
    assert b.reflect("What does the gateway use?", trace=True)["trace"]

    d = b.create_directive("Answer in one sentence.", name="Brief")
    assert b.directives()["items"][0]["id"] == d["id"]
    b.update_directive(d["id"], text="Answer briefly.")
    assert b.directives()["items"][0]["text"] == "Answer briefly."
    b.delete_directive(d["id"])
    assert b.directives()["items"] == []

    created = b.create_mental_model("Dana", "Who is Dana?", id="people/dana",
                                    body="Dana runs the gateway team.")
    assert created["mental_model_id"] == "people/dana" and created["operation_id"] is None
    assert created["mental_model"]["authority"] == "human"
    assert b.mental_model("people/dana")["body"].strip() == "Dana runs the gateway team."
    assert b.mental_models()["items"][0]["id"] == "people/dana"
    assert b.mental_model_tree()[0]["name"] == "people"
    assert any(f["path"] == "index.md" for f in b.export_mental_models())
    assert "Dana runs the gateway team." in b.export_mental_models(markdown=True)
    with pytest.raises(ModelRequired):
        b.refresh_mental_model("people/dana")
    moved = b.move_mental_model("people/dana", "team")
    assert moved["id"] == "team/dana"
    b.delete_mental_model("team/dana")

    with pytest.raises(ModelRequired):
        b.consolidate()
    assert b.observations()["items"] == []
    assert b.stats()["model_available"] is False


def test_async_retain_is_an_operation(bank):
    _, b = bank
    out = b.retain("Queued facts about the heron bridge.", document_id="q1", mode="chunks", async_=True)
    assert out["async"] is True and out["operation_ids"] == [out["operation_id"]]
    op = b.wait_operation(out["operation_id"], timeout=30, interval=0.2)
    assert op["status"] == "completed" and op["kind"] == op["type"] == "retain"
    listed = b.operations(type="retain")
    assert listed["bank_id"] == b.id and listed["total"] >= 1
    assert any(o["id"] == op["id"] for o in listed["operations"])
