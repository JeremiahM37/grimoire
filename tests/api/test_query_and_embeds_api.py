"""End-to-end: live queries + transclusion + footnotes through the API and the
unauthenticated /read surface — including the privacy guarantees."""


def _mk(client, path, body, fm=None):
    r = client.post("/api/notes", json={"title": path.rsplit("/", 1)[-1][:-3],
                                        "body": body, "path": path,
                                        **({"frontmatter": fm} if fm else {})})
    # note creation API may not take path/frontmatter directly — fall back to PUT
    if r.status_code >= 400:
        client.post("/api/notes", json={"title": path[:-3], "body": body})
        r = client.put(f"/api/notes/{path}",
                       json={"body": body, "frontmatter": fm or {}})
    return r


def test_api_query_endpoint(client):
    client.post("/api/notes", json={"title": "Proj One", "body": "x #project"})
    client.post("/api/notes", json={"title": "Proj Two", "body": "y #project"})
    r = client.post("/api/query", json={"block": "tag: project\nsort: title asc"})
    assert r.status_code == 200
    titles = [row["title"] for row in r.json()["rows"]]
    assert titles == ["Proj One", "Proj Two"]


def test_api_query_reports_errors(client):
    r = client.post("/api/query", json={"block": "sort: bogus"})
    assert r.status_code == 200
    assert r.json()["errors"]


def test_read_surface_renders_query_block_excluding_private(client):
    client.post("/api/notes", json={"title": "Public Item", "body": "a #inbox"})
    client.post("/api/notes", json={"title": "Secret Item", "body": "b #inbox"})
    client.put("/api/notes/secret-item.md",
               json={"body": "b #inbox", "frontmatter": {"private": True}})
    client.post("/api/notes",
                json={"title": "Dashboard", "body": "```query\ntag: inbox\n```"})
    html = client.get("/read/dashboard").text
    assert "Public Item" in html
    assert "Secret Item" not in html      # private never leaks onto /read


def test_read_surface_transcludes_public_not_private(client):
    client.post("/api/notes", json={"title": "Pub", "body": "PUBLIC-EMBED-TEXT"})
    client.post("/api/notes", json={"title": "Priv", "body": "PRIVATE-EMBED-TEXT"})
    client.put("/api/notes/priv.md",
               json={"body": "PRIVATE-EMBED-TEXT", "frontmatter": {"private": True}})
    client.post("/api/notes", json={"title": "Host", "body": "![[Pub]]\n\n![[Priv]]"})
    html = client.get("/read/host").text
    assert "PUBLIC-EMBED-TEXT" in html
    assert "PRIVATE-EMBED-TEXT" not in html


def test_export_renders_footnotes_and_heading_anchors(client):
    client.post("/api/notes", json={
        "title": "Paper",
        "body": "## Findings\n\nA claim.[^a]\n\n[^a]: Source here."})
    html = client.get("/notes/paper.md/export.html").text
    assert 'id="h-findings"' in html
    assert 'id="fn-a"' in html and "Source here." in html


def test_heading_link_resolves_with_anchor(client):
    client.post("/api/notes", json={"title": "Target", "body": "## Deep Section\n\nx"})
    client.post("/api/notes", json={"title": "Src", "body": "[[Target#Deep Section]]"})
    html = client.get("/read/src").text
    assert 'href="/read/target#h-deep-section"' in html
