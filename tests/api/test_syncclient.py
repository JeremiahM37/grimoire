"""Bidirectional sync client — direction, pull/push, conflict-safety."""
from server import index, syncclient, vault


def test_sync_pulls_missing_and_pushes_local(vaultdir, monkeypatch):
    vault.write("local-only.md", "# Local Only\n\nmine")
    index.reindex()
    remote_manifest = {"peer-only.md": {"hash": "deadbeef", "mtime": 9_999_999_999.0}}
    captured = {}

    def fake_req(url, method="GET", body=None, token=None, timeout=30):
        if url.endswith("/sync/manifest"):
            return remote_manifest
        if url.endswith("/sync/pull"):
            return {"contents": {"peer-only.md": "# Peer Only\n\ntheirs"}}
        if url.endswith("/sync/push"):
            captured["push"] = body
            return {"results": [{"path": c["path"], "status": "created"} for c in body["changes"]]}
        return {}
    monkeypatch.setattr(syncclient, "_req", fake_req)

    stats = syncclient.sync_with_peer("http://peer", "test")
    assert stats["pulled"] == 1 and stats["pushed"] == 1
    # pulled peer note landed locally
    assert (vaultdir / "peer-only.md").exists() and "theirs" in (vaultdir / "peer-only.md").read_text()
    # local-only was pushed
    assert any(c["path"] == "local-only.md" for c in captured["push"]["changes"])


def test_sync_pull_preserves_local_as_conflict_copy(vaultdir, monkeypatch):
    # local + peer both have note.md with different content; peer is "newer"
    vault.write("note.md", "# Note\n\nlocal version")
    index.reindex()
    remote_manifest = {"note.md": {"hash": "differenthash", "mtime": 9_999_999_999.0}}

    def fake_req(url, method="GET", body=None, token=None, timeout=30):
        if url.endswith("/sync/manifest"):
            return remote_manifest
        if url.endswith("/sync/pull"):
            return {"contents": {"note.md": "# Note\n\npeer version"}}
        if url.endswith("/sync/push"):
            return {"results": []}
        return {}
    monkeypatch.setattr(syncclient, "_req", fake_req)

    stats = syncclient.sync_with_peer("http://peer", "test")
    # peer version won, but the local version was preserved as a conflict copy — no data loss
    assert "peer version" in (vaultdir / "note.md").read_text()
    assert stats["conflicts"] == 1
    conflicts = list(vaultdir.glob("note (conflict *).md"))
    assert conflicts and "local version" in conflicts[0].read_text()


def test_sync_status_endpoint(client):
    assert client.get("/api/sync/status").json() == {"peer": None, "interval": 0}


def test_sync_now_without_peer_400(client):
    assert client.post("/api/sync/now").status_code == 400
