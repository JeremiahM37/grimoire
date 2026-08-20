"""CrewAI storage backend, against the real protocol and a real server.

CrewAI's backend is vector-in: it hands the storage a query embedding, never
the query text. That only produces meaningful results if the vectors CrewAI
computes are in the same space as the ones the server stored — which is what
GrimoireEmbedder is for, and what these tests actually exercise, since they
embed through the server exactly as CrewAI would.

Skipped where crewai is not installed or the binary has not been built.
"""

from __future__ import annotations

import pytest

pytest.importorskip("crewai", reason="crewai is not installed")

from crewai.memory.types import MemoryRecord  # noqa: E402

from grimoire_client import Grimoire  # noqa: E402
from grimoire_client.crewai import (  # noqa: E402
    GrimoireEmbedder,
    GrimoireStorage,
    decode_record,
    encode_record,
    note_for,
    topic_for,
)


@pytest.fixture
def backend(live_server, request):  # noqa: F811
    client = Grimoire(live_server, agent="crewai-test")
    storage = GrimoireStorage(client)
    storage._embedder = GrimoireEmbedder(client)
    # A scope per test, so tests cannot see each other's records.
    storage._scope = f"t/{request.node.name.replace('_', '-')[:40]}"
    return storage


def record(backend, id, content, **kwargs):
    return MemoryRecord(id=id, content=content, scope=backend._scope, **kwargs)


def embed(backend, text):
    return backend._embedder([text])[0]


# ---- the embedder ------------------------------------------------------


def test_embedder_returns_vectors_from_the_server(backend):
    vectors = backend._embedder(["one", "two"])
    assert len(vectors) == 2
    assert len(vectors[0]) > 0
    assert all(isinstance(x, float) for x in vectors[0])


def test_embedder_survives_an_empty_string(backend):
    # CrewAI embeds whatever a record holds, and an empty content should not
    # take the whole batch down.
    assert len(backend._embedder(["", "real text"])) == 2


def test_crewai_embed_text_helper_accepts_it(backend):
    # The exact call CrewAI makes internally.
    from crewai.memory.types import embed_text

    assert len(embed_text(backend._embedder, "the user prefers tabs")) > 0


# ---- the backend contract ---------------------------------------------


def test_save_then_search_finds_the_record(backend):
    backend.save([record(backend, "r1", "the deploy script lives in /usr/local/bin")])
    hits = backend.search(embed(backend, "where does the deploy script live"),
                          scope_prefix=backend._scope)
    assert hits, "nothing came back"
    found, score = hits[0]
    assert found.id == "r1"
    assert found.content == "the deploy script lives in /usr/local/bin"
    assert score > 0


def test_search_ranks_the_relevant_record_first(backend):
    backend.save([
        record(backend, "deploy", "the deploy script lives in /usr/local/bin"),
        record(backend, "cat", "the cat is named marmalade"),
    ])
    hits = backend.search(embed(backend, "the deploy script lives in /usr/local/bin"),
                          scope_prefix=backend._scope)
    assert hits[0][0].id == "deploy", [h[0].id for h in hits]


def test_a_plain_record_is_stored_as_readable_text(backend):
    # The point of the mapping: a crew's memory leaves a note a person can
    # read, not a line of JSON.
    backend.save([record(backend, "r1", "the release is cut on Thursdays")])
    note = backend.client.read_note(note_for(backend._scope))
    assert "the release is cut on Thursdays" in note["body"]
    assert '"content"' not in note["body"]


def test_metadata_survives_a_round_trip(backend):
    backend.save([record(backend, "r1", "a fact with baggage",
                         metadata={"source": "ticket-4", "confidence": 0.9})])
    found = backend.get_record("r1")
    assert found.content == "a fact with baggage"
    assert found.metadata["source"] == "ticket-4"


def test_get_record_of_an_unknown_id_is_none(backend):
    assert backend.get_record("never-saved") is None


def test_save_replaces_a_record_with_the_same_id(backend):
    backend.save([record(backend, "r1", "the first version")])
    backend.save([record(backend, "r1", "the second version")])
    assert backend.get_record("r1").content == "the second version"
    assert len([r for r in backend.list_records(backend._scope) if r.id == "r1"]) == 1


def test_update_replaces_in_place(backend):
    backend.save([record(backend, "r1", "before")])
    backend.update(record(backend, "r1", "after"))
    assert backend.get_record("r1").content == "after"


def test_list_records_honours_scope_limit_and_offset(backend):
    backend.save([record(backend, f"r{i}", f"distinct fact number {i}") for i in range(4)])
    all_records = backend.list_records(backend._scope, limit=10)
    assert len(all_records) == 4
    assert len(backend.list_records(backend._scope, limit=2)) == 2
    offset = backend.list_records(backend._scope, limit=10, offset=2)
    assert [r.id for r in offset] == [r.id for r in all_records[2:]]


def test_scopes_are_isolated(backend):
    other = f"{backend._scope}-other"
    backend.save([
        record(backend, "mine", "a fact in my scope"),
        MemoryRecord(id="theirs", content="a fact in another scope", scope=other),
    ])
    ids = {r.id for r in backend.list_records(backend._scope)}
    assert ids == {"mine"}, ids


def test_search_filters_by_category(backend):
    backend.save([
        record(backend, "a", "an operational fact", categories=["ops"]),
        record(backend, "b", "a personal preference", categories=["prefs"]),
    ])
    hits = backend.search(embed(backend, "fact"), scope_prefix=backend._scope,
                          categories=["ops"])
    assert [h[0].id for h in hits] == ["a"]


def test_search_filters_by_metadata(backend):
    backend.save([
        record(backend, "a", "one fact", metadata={"source": "ticket-4"}),
        record(backend, "b", "another fact", metadata={"source": "ticket-9"}),
    ])
    hits = backend.search(embed(backend, "fact"), scope_prefix=backend._scope,
                          metadata_filter={"source": "ticket-9"})
    assert [h[0].id for h in hits] == ["b"]


def test_search_respects_min_score(backend):
    backend.save([record(backend, "r1", "a fact")])
    assert backend.search(embed(backend, "a fact"), scope_prefix=backend._scope,
                          min_score=0.0)
    assert not backend.search(embed(backend, "a fact"), scope_prefix=backend._scope,
                              min_score=1.1)


def test_search_respects_limit(backend):
    backend.save([record(backend, f"r{i}", f"distinct fact number {i}") for i in range(5)])
    assert len(backend.search(embed(backend, "fact"), scope_prefix=backend._scope,
                              limit=2)) == 2


def test_delete_by_id_and_by_scope(backend):
    backend.save([record(backend, "r1", "one"), record(backend, "r2", "two")])
    assert backend.delete(record_ids=["r1"]) == 1
    assert backend.get_record("r1") is None
    assert backend.get_record("r2") is not None

    assert backend.delete(scope_prefix=backend._scope) >= 1
    assert backend.list_records(backend._scope) == []


def test_delete_of_an_unknown_id_removes_nothing(backend):
    backend.save([record(backend, "r1", "keep me")])
    assert backend.delete(record_ids=["never-saved"]) == 0
    assert backend.get_record("r1") is not None


def test_delete_by_category(backend):
    backend.save([
        record(backend, "a", "an operational fact", categories=["ops"]),
        record(backend, "b", "a personal preference", categories=["prefs"]),
    ])
    backend.delete(scope_prefix=backend._scope, categories=["ops"])
    assert {r.id for r in backend.list_records(backend._scope)} == {"b"}


# ---- the honesty check -------------------------------------------------


def test_the_server_refuses_a_foreign_embedding_space(backend):
    # The reason this backend is correct rather than merely functional: a
    # vector from another model is refused, not scored.
    from grimoire_client import GrimoireError

    backend.save([record(backend, "r1", "a fact")])
    with pytest.raises(GrimoireError) as excinfo:
        backend.search([0.1, 0.2, 0.3], scope_prefix=backend._scope)
    assert "dimensions" in excinfo.value.message


# ---- the mapping -------------------------------------------------------


def test_record_encoding_round_trips():
    plain = MemoryRecord(id="a", content="just content", scope="s")
    assert decode_record(encode_record(plain))["content"] == "just content"

    rich = MemoryRecord(id="b", content="with baggage", scope="s",
                        metadata={"k": "v"}, categories=["x", "y"])
    decoded = decode_record(encode_record(rich))
    assert decoded["content"] == "with baggage"
    assert decoded["metadata"] == {"k": "v"}
    assert decoded["categories"] == ["x", "y"]


def test_content_that_looks_like_json_is_not_mangled():
    assert decode_record("{not really json")["content"] == "{not really json"
    assert decode_record('{"other":"shape"}')["content"] == '{"other":"shape"}'


def test_scope_becomes_a_real_filename():
    assert note_for("crew/researcher") == "memory/crew-researcher.md"
    assert topic_for("") == "crew"
