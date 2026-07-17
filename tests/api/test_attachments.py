"""Image/file attachments: upload → stored in vault → served back for embeds."""

# a real 1×1 PNG
PNG = (b"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01"
       b"\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf"
       b"\xc0\xf0\x1f\x00\x05\x05\x02\x00\x84\xa6\xa6r\x00\x00\x00\x00IEND\xaeB`\x82")


def test_attach_image_stores_and_serves(client):
    r = client.post("/api/attach", files={"file": ("shot.png", PNG, "image/png")})
    assert r.status_code == 201
    j = r.json()
    assert j["is_image"] is True
    assert j["path"].startswith("attachments/") and j["path"].endswith(".png")
    assert j["bytes"] == len(PNG)
    # served back byte-for-byte
    g = client.get("/api/file/" + j["path"])
    assert g.status_code == 200 and g.content == PNG


def test_attachment_stored_with_real_extension_and_type(client, vaultdir):
    j = client.post("/api/attach", files={"file": ("photo.png", PNG, "image/png")}).json()
    # the on-disk file keeps its real extension (NOT coerced to .md)
    stored = vaultdir / j["path"]
    assert stored.exists() and stored.suffix == ".png"
    assert not (vaultdir / (j["path"] + ".md")).exists()
    # and it's served with an image content-type, not text/markdown
    g = client.get("/api/file/" + j["path"])
    assert g.headers["content-type"].startswith("image/")


def test_attach_non_image_flagged(client):
    r = client.post("/api/attach", files={"file": ("report.pdf", b"%PDF-1.4 x", "application/pdf")})
    assert r.status_code == 201
    assert r.json()["is_image"] is False


def test_attach_slugifies_hostile_filename(client):
    # a traversal-y filename must not escape the attachments dir
    r = client.post("/api/attach", files={"file": ("../../etc/evil.sh", b"x", "text/x-sh")})
    assert r.status_code == 201
    p = r.json()["path"]
    assert ".." not in p and p.startswith("attachments/")


def test_file_endpoint_404s_missing(client):
    assert client.get("/api/file/attachments/nope.png").status_code == 404


def test_image_embed_renders_on_read_surface(client):
    up = client.post("/api/attach", files={"file": ("pic.png", PNG, "image/png")}).json()
    client.post("/api/notes", json={"title": "Has Image", "body": f"look:\n\n![[{up['path']}]]"})
    html = client.get("/read/has-image").text
    assert f'<img src="/api/file/{up["path"]}"' in html
    # the ![[ ]] must NOT leak through as a broken wiki-link
    assert "![[" not in html
