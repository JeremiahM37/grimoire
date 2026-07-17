"""Frontmatter aliases resolve as wiki-link targets."""


def test_alias_resolves_wikilink_and_backlink(client):
    client.post("/api/notes", json={
        "title": "United States", "body": "the country",
        "frontmatter": {"aliases": ["USA", "America"]}})
    client.post("/api/notes", json={"title": "Geo", "body": "I live in [[USA]]"})
    # the [[USA]] link resolves to the aliased note → backlink shows up
    note = client.get("/api/notes/united-states.md").json()
    assert any(b["path"] == "geo.md" for b in note["backlinks"])
    # and it's not counted as an unresolved link
    assert client.get("/api/health").json()["unresolved_links"] == 0


def test_alias_map_endpoint(client):
    client.post("/api/notes", json={
        "title": "Machine Learning", "body": "x", "frontmatter": {"aliases": ["ML", "AI/ML"]}})
    amap = client.get("/api/aliases").json()
    assert amap.get("ml") == "machine-learning.md"
    assert amap.get("ai/ml") == "machine-learning.md"


def test_single_string_alias_supported(client):
    client.post("/api/notes", json={
        "title": "New York City", "body": "x", "frontmatter": {"aliases": "NYC"}})
    client.post("/api/notes", json={"title": "Trip", "body": "off to [[NYC]]"})
    assert client.get("/api/health").json()["unresolved_links"] == 0
