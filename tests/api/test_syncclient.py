"""Sync client — CRDT-first flow + last-writer fallback + endpoints.
(Content-correctness of merges is proven in tests/integration/test_sync_e2e.py.)"""
from server import crdt, index, syncclient, vault


def test_mergeable_notes_use_the_crdt_path(vaultdir, monkeypatch):
    vault.write("note.md", "local body", {"title": "N"})
    index.reindex()
    remote = {"note.md": {"hash": "different", "mtime": 1.0}}
    calls = {"doc": 0, "merge": 0}
    peer_doc = crdt.Doc.from_text("peer body", "peer").to_json()

    def fake_req(url, method="GET", body=None, token=None, timeout=30):
        if url.endswith("/sync/manifest"):
            return remote
        if "/crdt/doc/" in url:
            calls["doc"] += 1
            return {"doc": peer_doc, "fm": {"title": "N"}}
        if url.endswith("/crdt/merge"):
            calls["merge"] += 1
            return {"changed": True}
        return {"contents": {}, "results": []}
    monkeypatch.setattr(syncclient, "_req", fake_req)

    stats = syncclient.sync_with_peer("http://peer", "test")
    assert calls["doc"] >= 1 and calls["merge"] >= 1     # exchanged CRDT docs both ways
    assert stats["merged"] >= 1


def test_local_only_note_pushed_via_crdt(vaultdir, monkeypatch):
    vault.write("local-only.md", "mine", {"title": "L"})
    index.reindex()
    pushed = []

    def fake_req(url, method="GET", body=None, token=None, timeout=30):
        if url.endswith("/sync/manifest"):
            return {}                                    # peer is empty
        if url.endswith("/crdt/merge"):
            pushed.append(body["path"])
            return {"changed": True}
        return {"contents": {}, "results": []}
    monkeypatch.setattr(syncclient, "_req", fake_req)

    stats = syncclient.sync_with_peer("http://peer", "test")
    assert stats["pushed"] == 1 and "local-only.md" in pushed


def test_oversized_note_falls_back_to_last_writer(vaultdir, monkeypatch):
    big = "x" * (syncclient.crdtstore.MAX_CRDT_BYTES + 10)
    vault.write("huge.md", big, {"title": "H"})
    index.reindex()
    remote = {"huge.md": {"hash": "different", "mtime": 9_999_999_999.0}}   # peer newer
    pulled = []

    def fake_req(url, method="GET", body=None, token=None, timeout=30):
        if url.endswith("/sync/manifest"):
            return remote
        if url.endswith("/sync/pull"):
            pulled.extend(body["paths"])
            return {"contents": {"huge.md": None}}
        return {"contents": {}, "results": []}
    monkeypatch.setattr(syncclient, "_req", fake_req)

    syncclient.sync_with_peer("http://peer", "test")
    assert "huge.md" in pulled              # non-mergeable → last-writer pull, not CRDT


def test_sync_status_endpoint(client):
    assert client.get("/api/sync/status").json() == {"peer": None, "interval": 0}


def test_sync_now_without_peer_400(client):
    assert client.post("/api/sync/now").status_code == 400
