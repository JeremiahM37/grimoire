"""LangGraph store adapter, against a real server and the real langgraph.

Unlike the client tests, these do NOT stub anything: an adapter that satisfies
an interface I imagined is worth nothing. The store is driven through
langgraph's own ``BaseStore`` methods — which route through ``batch`` and the
real op types — against the binary the ``live_server`` fixture launches (see
conftest.py). If either side changes its contract, this fails.

Skipped when langgraph is not installed or the binary has not been built.
"""

from __future__ import annotations

import asyncio

import pytest

pytest.importorskip("langgraph", reason="langgraph is not installed")

from grimoire_client import Grimoire  # noqa: E402
from grimoire_client.langgraph import (  # noqa: E402
    GrimoireStore,
    decode_value,
    encode_value,
    namespace_for,
    note_for,
    slugify,
)

@pytest.fixture
def store(live_server, request):
    # A namespace per test, so tests cannot see each other's facts.
    client = Grimoire(live_server, agent="langgraph-test")
    store = GrimoireStore(client)
    store._namespace = ("t", slugify(request.node.name)[:40])
    return store


def ns(store, *extra):
    return store._namespace + extra


# ---- the store contract ------------------------------------------------


def test_put_then_get_round_trips(store):
    store.put(ns(store), "pref", {"text": "the user prefers tabs"})
    item = store.get(ns(store), "pref")
    assert item is not None
    assert item.value == {"text": "the user prefers tabs"}
    assert item.key == "pref"
    assert item.namespace == ns(store)


def test_get_of_an_unknown_key_is_none(store):
    assert store.get(ns(store), "never-written") is None


def test_put_replaces_rather_than_accumulating(store):
    # A key-value store must return what was put last, not both.
    store.put(ns(store), "pref", {"text": "the user prefers spaces"})
    store.put(ns(store), "pref", {"text": "the user prefers tabs"})
    assert store.get(ns(store), "pref").value == {"text": "the user prefers tabs"}
    hits = store.search(ns(store), query="prefers")
    assert len([h for h in hits if h.key == "pref"]) == 1


def test_structured_values_survive_intact(store):
    value = {"count": 3, "nested": {"a": [1, 2]}, "flag": True}
    store.put(ns(store), "state", value)
    assert store.get(ns(store), "state").value == value


def test_a_plain_fact_is_stored_as_readable_text(store):
    # The point of the mapping: a store holding facts leaves a note a person
    # can read, not a line of JSON.
    store.put(ns(store), "pref", {"text": "the deploy needs a VPN reset"})
    note = store.client.read_note(note_for(ns(store)))
    assert "the deploy needs a VPN reset" in note["body"]
    assert "{" not in note["body"].split("the deploy")[0].splitlines()[-1]


def test_search_ranks_by_query(store):
    store.put(ns(store), "a", {"text": "the deploy script lives in /usr/local/bin"})
    store.put(ns(store), "b", {"text": "the cat is named marmalade"})
    hits = store.search(ns(store), query="deploy script")
    assert hits, "search returned nothing"
    assert hits[0].key == "a"
    assert hits[0].score is not None


def test_search_without_a_query_returns_the_namespace(store):
    store.put(ns(store), "a", {"text": "one distinct fact"})
    store.put(ns(store), "b", {"text": "another separate fact"})
    keys = {hit.key for hit in store.search(ns(store))}
    assert keys == {"a", "b"}


def test_search_respects_limit_and_offset(store):
    for i in range(4):
        store.put(ns(store), f"k{i}", {"text": f"distinct fact number {i}"})
    assert len(store.search(ns(store), limit=2)) == 2
    first = store.search(ns(store), limit=4)
    offset = store.search(ns(store), limit=4, offset=2)
    assert [h.key for h in offset] == [h.key for h in first[2:]]


def test_namespaces_are_isolated(store):
    store.put(ns(store, "alice"), "pref", {"text": "alice prefers tabs"})
    store.put(ns(store, "bob"), "pref", {"text": "bob prefers spaces"})
    assert store.get(ns(store, "alice"), "pref").value["text"] == "alice prefers tabs"
    assert store.get(ns(store, "bob"), "pref").value["text"] == "bob prefers spaces"


def test_delete_removes_the_key(store):
    store.put(ns(store), "pref", {"text": "a fact to remove"})
    store.delete(ns(store), "pref")
    assert store.get(ns(store), "pref") is None
    assert not [h for h in store.search(ns(store)) if h.key == "pref"]


def test_delete_of_an_unknown_key_is_not_an_error(store):
    store.delete(ns(store), "never-written")


def test_put_with_a_none_value_deletes(store):
    # langgraph's documented way to delete through a put.
    store.put(ns(store), "pref", {"text": "here"})
    store.batch([_put_op(ns(store), "pref", None)])
    assert store.get(ns(store), "pref") is None


def _put_op(namespace, key, value):
    from langgraph.store.base import PutOp

    return PutOp(namespace=namespace, key=key, value=value)


def test_list_namespaces_finds_what_was_written(store):
    store.put(ns(store, "alice"), "pref", {"text": "a fact in alice's namespace"})
    assert any(space[-1] == "alice" for space in store.list_namespaces())


def test_batch_runs_every_operation(store):
    from langgraph.store.base import GetOp, PutOp

    store.batch([
        PutOp(namespace=ns(store), key="a", value={"text": "first batched fact"}),
        PutOp(namespace=ns(store), key="b", value={"text": "second batched fact"}),
    ])
    results = store.batch([
        GetOp(namespace=ns(store), key="a"),
        GetOp(namespace=ns(store), key="b"),
    ])
    assert [r.value["text"] for r in results] == [
        "first batched fact", "second batched fact"]


def test_async_methods_work(store):
    # Driven with asyncio.run rather than an async test, so the suite needs no
    # async pytest plugin to cover the async half of the interface.
    async def exercise():
        await store.aput(ns(store), "pref", {"text": "written through the async api"})
        return await store.aget(ns(store), "pref")

    item = asyncio.run(exercise())
    assert item.value["text"] == "written through the async api"


def test_reconciling_store_supersedes_a_contradiction(live_server, request):
    # Opt-in mode: two different keys asserting contradictory facts. The second
    # supersedes the first, so the stale one stops being searchable — which is
    # the behaviour a plain key-value store cannot give you.
    client = Grimoire(live_server, agent="langgraph-test")
    store = GrimoireStore(client, reconcile=True)
    space = ("t", "reconcile", slugify(request.node.name)[:20])
    store.put(space, "old", {"text": "the user prefers spaces"})
    store.put(space, "new", {"text": "the user prefers tabs"})

    texts = [hit.value["text"] for hit in store.search(space)]
    assert "the user prefers tabs" in texts
    assert "the user prefers spaces" not in texts


# ---- the mapping itself ------------------------------------------------


def test_value_encoding_round_trips():
    for value in [
        {"text": "a plain fact"},
        {"count": 1},
        {"text": "with", "extra": "fields"},
        {},
    ]:
        assert decode_value(encode_value(value)) == value


def test_text_that_looks_like_json_is_not_mangled():
    assert decode_value("{not really json") == {"text": "{not really json"}


def test_note_and_namespace_are_inverses_for_slug_safe_names():
    for space in [("memories",), ("memories", "alice"), ("a", "b", "c")]:
        assert namespace_for(note_for(space)) == space


def test_a_namespace_is_slugified_into_a_real_filename():
    # The name the client filters on has to be the name the server wrote, or
    # the store looks empty.
    assert note_for(("Memories", "Alice_Smith")) == "memory/memories-alice-smith.md"
