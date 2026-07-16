"""API: full note lifecycle, links/backlinks, search, daily, capture."""


def test_health(client):
    h = client.get("/api/health").json()
    assert h["ok"] and h["notes"] == 0


def test_create_read_update_delete(client):
    r = client.post("/api/notes", json={"title": "First Note", "body": "# First\n\nhi"})
    assert r.status_code == 201
    path = r.json()["path"]
    assert path == "first-note.md"

    got = client.get(f"/api/notes/{path}")
    assert got.status_code == 200 and got.json()["title"] == "First Note"

    client.put(f"/api/notes/{path}", json={"body": "# First\n\nedited #tag"})
    assert "tag" in client.get(f"/api/notes/{path}").json()["tags"]

    assert client.delete(f"/api/notes/{path}").status_code == 204
    assert client.get(f"/api/notes/{path}").status_code == 404


def test_wikilinks_and_backlinks(client):
    client.post("/api/notes", json={"title": "Target", "body": "the target"})
    client.post("/api/notes", json={"title": "Source", "body": "links to [[Target]]"})
    bl = client.get("/api/notes/target.md").json()["backlinks"]
    assert [b["path"] for b in bl] == ["source.md"]
    # unresolved link surfaces in graph
    client.post("/api/notes", json={"title": "Dangling", "body": "see [[Nonexistent]]"})
    g = client.get("/api/graph").json()
    assert "Nonexistent" in g["unresolved"]


def test_search_ranked(client):
    client.post("/api/notes", json={"title": "Apples", "body": "apples are red fruit"})
    client.post("/api/notes", json={"title": "Bananas", "body": "bananas are yellow"})
    res = client.get("/api/search?q=fruit").json()
    assert [r["path"] for r in res] == ["apples.md"]
    assert "[" in res[0]["snippet"]   # highlighted


def test_daily_note_created(client):
    d = client.get("/api/daily").json()
    assert d["path"].startswith("journal/") and d["path"].endswith(".md")
    # idempotent
    assert client.get("/api/daily").json()["path"] == d["path"]


def test_capture_threads_into_daily(client):
    r = client.post("/api/capture", json={"text": "a clipped thought", "url": "https://x.test"})
    assert r.status_code == 201
    cap_path = r.json()["path"]
    assert cap_path.startswith("inbox/")
    # the daily note now links to the capture
    import time
    daily = client.get(f"/api/notes/journal/{time.strftime('%Y-%m-%d')}.md").json()
    assert "[[" in daily["body"]


def test_rename(client):
    client.post("/api/notes", json={"title": "Old", "body": "x"})
    r = client.post("/api/notes/old.md/rename", json={"to": "renamed.md"})
    assert r.status_code == 200 and r.json()["path"] == "renamed.md"
    assert client.get("/api/notes/old.md").status_code == 404
    assert client.get("/api/notes/renamed.md").status_code == 200


def test_reindex_rebuilds(client):
    client.post("/api/notes", json={"title": "N", "body": "x"})
    assert client.post("/api/reindex").json()["indexed"] == 1
