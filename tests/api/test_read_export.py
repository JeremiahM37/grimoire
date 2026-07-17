"""v0.5 e-ink read surface — plain HTML, no JS, private excluded."""


def test_read_index_lists_public_notes(client):
    client.post("/api/notes", json={"title": "Readable", "body": "content"})
    client.post("/api/notes", json={"title": "Hidden", "frontmatter": {"private": True}, "body": "secret"})
    html = client.get("/read").text
    assert "Readable" in html and "Hidden" not in html
    assert "<script" not in html.lower()   # no JS — e-ink friendly


def test_read_note_renders_wikilinks_as_hyperlinks(client):
    client.post("/api/notes", json={"title": "Target", "body": "the target"})
    client.post("/api/notes", json={"title": "Source", "body": "see [[Target]] here"})
    html = client.get("/read/source").text
    assert 'href="/read/target">Target</a>' in html
    # backlink footer on the target
    tgt = client.get("/read/target").text
    assert "Linked from" in tgt and "Source" in tgt


def test_read_note_unresolved_link_not_hyperlinked(client):
    client.post("/api/notes", json={"title": "Lonely", "body": "see [[Nonexistent]]"})
    html = client.get("/read/lonely").text
    assert 'class="unresolved">Nonexistent' in html


def test_read_surface_escapes_xss(client):
    client.post("/api/notes", json={"title": "XSS", "body":
        'evil <script>alert(1)</script> and <img src=x onerror=alert(2)> and [[<b>x</b>]]'})
    html = client.get("/read/xss").text
    assert "<script>alert(1)</script>" not in html          # never raw
    assert "&lt;script&gt;" in html                          # escaped
    assert "onerror=alert" not in html or "&lt;img" in html  # attribute neutralized


def test_read_private_note_404(client):
    client.post("/api/notes", json={"title": "Priv", "frontmatter": {"private": True}, "body": "x"})
    assert client.get("/read/priv").status_code == 404
