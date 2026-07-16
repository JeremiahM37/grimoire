"""v0.5 sync: manifest/pull/push + conflict copies that never lose data."""


def test_manifest_and_pull(client):
    client.post("/api/notes", json={"title": "Note A", "body": "content a"})
    m = client.get("/api/sync/manifest").json()
    assert "note-a.md" in m and "hash" in m["note-a.md"]
    pulled = client.post("/api/sync/pull", json={"paths": ["note-a.md"]}).json()["contents"]
    assert "content a" in pulled["note-a.md"]


def test_push_create_and_update(client):
    r = client.post("/api/sync/push", json={"changes": [
        {"path": "pushed.md", "content": "---\ntitle: Pushed\n---\nhello"}]}).json()
    assert r["results"][0]["status"] == "created"
    assert client.get("/api/notes/pushed.md").json()["title"] == "Pushed"
    # update with correct base_hash → ok
    h = client.get("/api/sync/manifest").json()["pushed.md"]["hash"]
    r2 = client.post("/api/sync/push", json={"changes": [
        {"path": "pushed.md", "content": "---\ntitle: Pushed\n---\nedited", "base_hash": h}]}).json()
    assert r2["results"][0]["status"] == "ok"


def test_conflict_creates_copy_never_loses_data(client, vaultdir):
    # server has a note; two clients edited from the same base
    client.post("/api/notes", json={"title": "Shared", "body": "original"})
    # client edits from a STALE base_hash (server changed since)
    client.post("/api/sync/push", json={"changes": [
        {"path": "shared.md", "content": "server version", "base_hash": None}]})  # server write
    server_hash = client.get("/api/sync/manifest").json()["shared.md"]["hash"]
    # now a second client pushes with a DIFFERENT stale base
    r = client.post("/api/sync/push", json={"changes": [
        {"path": "shared.md", "content": "client's divergent version",
         "base_hash": "staaaale"}]}).json()["results"][0]
    assert r["status"] == "conflict"
    conflict = r["conflict_copy"]
    # server version preserved
    assert "server version" in client.get("/api/notes/shared.md").json()["body"]
    # divergent version preserved in a conflict copy — NOTHING lost
    assert "divergent version" in client.get(f"/api/notes/{conflict}").json()["body"]
    assert "(conflict" in conflict


def test_push_delete(client):
    client.post("/api/notes", json={"title": "Doomed", "body": "x"})
    h = client.get("/api/sync/manifest").json()["doomed.md"]["hash"]
    r = client.post("/api/sync/push", json={"changes": [
        {"path": "doomed.md", "content": None, "base_hash": h}]}).json()
    assert r["results"][0]["status"] == "deleted"
    assert client.get("/api/notes/doomed.md").status_code == 404


def test_delete_conflict_keeps_changed_note(client):
    client.post("/api/notes", json={"title": "Keep", "body": "important"})
    # delete request with a stale base → server keeps it (don't delete changed data)
    r = client.post("/api/sync/push", json={"changes": [
        {"path": "keep.md", "content": None, "base_hash": "old"}]}).json()
    assert r["results"][0]["status"] == "conflict-keep"
    assert client.get("/api/notes/keep.md").status_code == 200
