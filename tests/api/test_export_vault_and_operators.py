"""Whole-vault zip export + search operators."""
import io
import zipfile


def test_vault_export_is_a_zip_of_notes_without_grimoire(client, vaultdir):
    client.post("/api/notes", json={"title": "In Zip", "body": "hello"})
    # a secret store must NOT be included
    client.post("/api/vault/init", json={"passphrase": "zippass123456"})
    r = client.get("/api/export/vault")
    assert r.status_code == 200
    assert r.headers["content-type"] == "application/zip"
    z = zipfile.ZipFile(io.BytesIO(r.content))
    names = z.namelist()
    assert "in-zip.md" in names
    assert not any(".grimoire" in n for n in names)          # index + secrets excluded


def test_search_tag_operator(client):
    client.post("/api/notes", json={"title": "Tagged A", "body": "work #project stuff"})
    client.post("/api/notes", json={"title": "Tagged B", "body": "home #chore stuff"})
    hits = client.get("/api/search?q=stuff tag:project").json()
    titles = {h["title"] for h in hits}
    assert titles == {"Tagged A"}                          # tag: narrows FTS


def test_search_is_pinned_operator(client):
    client.post("/api/notes", json={"title": "PinnedOne", "body": "alpha"})
    client.post("/api/notes", json={"title": "PlainOne", "body": "alpha"})
    client.post("/api/notes/pinnedone.md/pin")
    hits = client.get("/api/search?q=alpha is:pinned").json()
    assert {h["title"] for h in hits} == {"PinnedOne"}


def test_search_path_operator_alone(client):
    client.get("/api/daily?date=2026-05-01")               # journal/2026-05-01.md
    client.post("/api/notes", json={"title": "Root Note", "body": "x"})
    hits = client.get("/api/search?q=path:journal").json()
    assert hits and all("journal/" in h["path"] for h in hits)
