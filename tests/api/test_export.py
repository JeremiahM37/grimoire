"""Per-note HTML export — standalone, self-contained (images inlined)."""

PNG = (b"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01"
       b"\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf"
       b"\xc0\xf0\x1f\x00\x05\x05\x02\x00\x84\xa6\xa6r\x00\x00\x00\x00IEND\xaeB`\x82")


def test_export_is_self_contained_html(client):
    up = client.post("/api/attach", files={"file": ("p.png", PNG, "image/png")}).json()
    client.post("/api/notes", json={
        "title": "Export Me",
        "body": f"# Export Me\n\n**bold** text\n\n![[{up['path']}]]"})
    r = client.get("/notes/export-me/export.html")
    assert r.status_code == 200
    assert r.headers["content-type"].startswith("text/html")
    assert "content-disposition" not in r.headers        # inline by default
    assert "attachment" in client.get(
        "/notes/export-me/export.html?download=1").headers.get("content-disposition", "")
    html = r.text
    assert "<h1>Export Me</h1>" in html and "<strong>bold</strong>" in html
    # image inlined as a data: URI — NOT a server URL (truly portable file)
    assert "data:image/png;base64," in html
    assert "/api/file/" not in html


def test_export_404_for_missing_note(client):
    assert client.get("/notes/nope/export.html").status_code == 404
