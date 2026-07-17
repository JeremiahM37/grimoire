"""Rename a tag across the whole vault."""


def test_rename_tag_updates_body_and_frontmatter(client, vaultdir):
    client.post("/api/notes", json={"title": "N1", "body": "work #proj here"})
    client.post("/api/notes", json={"title": "N2", "body": "more #proj stuff",
                                    "frontmatter": {"tags": ["proj", "keep"]}})
    client.post("/api/notes", json={"title": "N3", "body": "unrelated #other"})
    r = client.post("/api/tags/rename", json={"old": "proj", "new": "project"})
    assert r.json() == {"renamed": "proj", "to": "project", "notes": 2}
    # body rewritten
    assert "#project" in (vaultdir / "n1.md").read_text() and "#proj " not in (vaultdir / "n1.md").read_text()
    # frontmatter tag rewritten, other tag kept
    n2 = client.get("/api/notes/n2.md").json()
    assert "project" in n2["tags"] and "keep" in n2["tags"] and "proj" not in n2["tags"]
    # old tag gone, new tag present in the index
    assert client.get("/api/notes?tag=proj").json() == []
    assert len(client.get("/api/notes?tag=project").json()) == 2
    # untouched note keeps its tag
    assert "#other" in (vaultdir / "n3.md").read_text()


def test_rename_does_not_touch_prefix_tags(client, vaultdir):
    client.post("/api/notes", json={"title": "P", "body": "#work and #workflow and #work/sub"})
    client.post("/api/tags/rename", json={"old": "work", "new": "job"})
    body = (vaultdir / "p.md").read_text()
    assert "#job " in body                       # exact tag renamed
    assert "#workflow" in body                   # prefix match NOT renamed
    assert "#work/sub" in body                   # nested tag NOT renamed


def test_rename_encrypted_note_frontmatter_tag_only(client, vaultdir):
    client.post("/api/vault/init", json={"passphrase": "tagrenpass123"})
    client.post("/api/notes", json={"title": "Enc", "body": "secret",
                                    "frontmatter": {"tags": ["conf"]}})
    client.post("/api/notes/enc.md/encrypt")
    r = client.post("/api/tags/rename", json={"old": "conf", "new": "classified"})
    assert r.json()["notes"] == 1
    # still sealed, frontmatter tag updated
    disk = (vaultdir / "enc.md").read_text()
    assert "secret" not in disk and "mnemo:enc:v1:" in disk
    assert "classified" in disk
