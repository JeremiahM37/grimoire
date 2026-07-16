"""Negative / adversarial API tests — the class the user emphasized."""


def test_path_traversal_via_api_rejected(client, vaultdir):
    # a note path that tries to escape the vault must fail (4xx) and, above all,
    # never create a file outside the vault
    for bad in ("../../../etc/passwd", "..%2f..%2fpwn", "../escape"):
        r = client.put(f"/api/notes/{bad}", json={"body": "PWNED"})
        assert r.status_code >= 400, (bad, r.status_code)
    # the definitive check: nothing was written outside the vault
    escape = vaultdir.parent / "escape.md"
    assert not escape.exists()
    assert not (vaultdir.parent.parent / "pwn.md").exists()


def test_create_missing_title_and_path(client):
    assert client.post("/api/notes", json={"body": "x"}).status_code == 400


def test_explicit_path_collision_conflicts(client):
    client.post("/api/notes", json={"path": "fixed.md", "body": "a"})
    assert client.post("/api/notes", json={"path": "fixed.md", "body": "b"}).status_code == 409


def test_title_collision_auto_suffixes(client):
    # two notes with the same title must both succeed with distinct paths (no 409)
    a = client.post("/api/notes", json={"title": "Meeting Notes", "body": "a"})
    b = client.post("/api/notes", json={"title": "Meeting Notes", "body": "b"})
    assert a.status_code == 201 and b.status_code == 201
    assert a.json()["path"] == "meeting-notes.md"
    assert b.json()["path"] == "meeting-notes-2.md"
    c = client.post("/api/notes", json={"title": "Meeting Notes", "body": "c"})
    assert c.json()["path"] == "meeting-notes-3.md"


def test_get_missing_note_404(client):
    assert client.get("/api/notes/ghost.md").status_code == 404


def test_search_injection_is_safe(client):
    client.post("/api/notes", json={"title": "Safe", "body": "normal content"})
    # FTS syntax chars / SQL-ish input must not error or leak — just return []/results
    payloads = ["'; DROP TABLE notes; --", "NEAR (", "*", "AND OR NOT", "foo )("]
    for q in payloads:
        r = client.get("/api/search", params={"q": q})
        assert r.status_code == 200 and isinstance(r.json(), list)
    # table still there
    assert client.get("/api/health").json()["notes"] == 1


def test_empty_search_returns_empty(client):
    assert client.get("/api/search?q=").json() == []


def test_rename_onto_existing_conflicts(client):
    client.post("/api/notes", json={"title": "A", "body": "x"})
    client.post("/api/notes", json={"title": "B", "body": "y"})
    assert client.post("/api/notes/a.md/rename", json={"to": "b.md"}).status_code == 400


def test_malformed_frontmatter_does_not_crash(client, vaultdir):
    # a hand-written file with broken frontmatter must index without exploding
    (vaultdir / "broken.md").write_text("---\ntitle: [unclosed\nbroken\n---\nbody")
    assert client.post("/api/reindex").json()["indexed"] >= 1
    assert client.get("/api/notes/broken.md").status_code == 200


def test_auth_token_enforced_when_set(tmp_path, monkeypatch):
    from fastapi.testclient import TestClient

    from server import config, db
    from server.app import create_app
    vdir = tmp_path / "v"; vdir.mkdir()
    monkeypatch.setattr(config, "VAULT", vdir)
    monkeypatch.setattr(config, "AUTH_TOKEN", "sekret")
    with TestClient(create_app()) as c:
        assert c.get("/api/notes").status_code == 401
        assert c.get("/api/notes", headers={"Authorization": "Bearer sekret"}).status_code == 200
    db.close()
