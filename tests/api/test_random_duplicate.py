"""Random note + duplicate note."""


def test_random_returns_an_existing_note(client):
    for t in ("R1", "R2", "R3"):
        client.post("/api/notes", json={"title": t, "body": "x"})
    paths = {n["path"] for n in client.get("/api/notes").json()}
    got = client.get("/api/notes/random").json()["path"]
    assert got in paths


def test_random_404_when_empty(client):
    assert client.get("/api/notes/random").status_code == 404


def test_duplicate_copies_body_with_unique_path(client):
    client.post("/api/notes", json={"title": "Recipe", "body": "# Recipe\n\nflour, water"})
    r = client.post("/api/notes/recipe.md/duplicate")
    assert r.status_code == 201
    dup = r.json()
    assert dup["path"] == "recipe-copy.md"
    assert dup["title"] == "Recipe (copy)"
    assert "flour, water" in dup["body"]
    # both exist independently
    assert client.get("/api/notes/recipe.md").status_code == 200
    assert client.get("/api/notes/recipe-copy.md").status_code == 200


def test_duplicate_encrypted_stays_sealed(client, vaultdir):
    client.post("/api/vault/init", json={"passphrase": "duppass123456"})
    client.post("/api/notes", json={"title": "Sealed Dup", "body": "confidential"})
    client.post("/api/notes/sealed-dup.md/encrypt")
    r = client.post("/api/notes/sealed-dup.md/duplicate")
    assert r.status_code == 201 and r.json()["encrypted"] is True
    disk = (vaultdir / r.json()["path"]).read_text()
    assert "confidential" not in disk and "mnemo:enc:v1:" in disk
