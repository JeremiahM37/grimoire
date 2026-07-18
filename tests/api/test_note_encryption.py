"""Encryption-at-rest for private notes. The body is sealed on disk with the
vault key; ciphertext is never indexed/searched/RAG'd; decrypt when unlocked."""

PASS = "correct horse battery staple"


def _unlock(client):
    client.post("/api/vault/init", json={"passphrase": PASS})   # init leaves it unlocked


def test_encrypt_roundtrip_and_search_exclusion(client):
    _unlock(client)
    client.post("/api/notes", json={"title": "Diary", "body": "met SECRETWORD at the cafe"})
    assert client.get("/api/search?q=SECRETWORD").json()          # searchable before
    r = client.post("/api/notes/diary.md/encrypt")
    assert r.status_code == 200
    v = r.json()
    assert v["encrypted"] is True and v["locked"] is False
    assert "met SECRETWORD" in v["body"]                          # decrypted for the editor
    # ciphertext is NOT indexed
    assert client.get("/api/search?q=SECRETWORD").json() == []
    g = client.get("/api/notes/diary.md").json()
    assert g["encrypted"] and "met SECRETWORD" in g["body"]


def test_ciphertext_on_disk_hides_plaintext(client, vaultdir):
    _unlock(client)
    client.post("/api/notes", json={"title": "Sensitive", "body": "my password is hunter2"})
    client.post("/api/notes/sensitive.md/encrypt")
    disk = (vaultdir / "sensitive.md").read_text()
    assert "hunter2" not in disk and "grimoire:enc:v1:" in disk


def test_encrypted_note_not_in_rag_or_read_surface(client):
    _unlock(client)
    client.post("/api/notes", json={"title": "Hidden", "body": "quantum widget blueprint"})
    client.post("/api/notes/hidden.md/encrypt")
    # not retrievable even with include_private (no vectors exist for it)
    hits = client.get("/api/retrieve?q=quantum widget&include_private=true").json()
    assert all(h["path"] != "hidden.md" for h in hits)
    # excluded from the e-ink surface
    assert "Hidden" not in client.get("/read").text


def test_locked_vault_hides_encrypted_body(client):
    _unlock(client)
    client.post("/api/notes", json={"title": "Locked Note", "body": "top secret content"})
    client.post("/api/notes/locked-note.md/encrypt")
    client.post("/api/vault/lock")
    g = client.get("/api/notes/locked-note.md").json()
    assert g["locked"] is True and g["body"] == "" and g["encrypted"] is True


def test_edit_encrypted_note_reseals(client, vaultdir):
    _unlock(client)
    client.post("/api/notes", json={"title": "Edit Enc", "body": "version one"})
    client.post("/api/notes/edit-enc.md/encrypt")
    client.put("/api/notes/edit-enc.md", json={"body": "version two updated"})
    disk = (vaultdir / "edit-enc.md").read_text()
    assert "version two" not in disk and "grimoire:enc" in disk      # still sealed
    assert client.get("/api/notes/edit-enc.md").json()["body"] == "version two updated"


def test_edit_encrypted_note_while_locked_is_rejected(client):
    _unlock(client)
    client.post("/api/notes", json={"title": "No Edit", "body": "x"})
    client.post("/api/notes/no-edit.md/encrypt")
    client.post("/api/vault/lock")
    assert client.put("/api/notes/no-edit.md", json={"body": "attempt"}).status_code == 423


def test_decrypt_restores_plaintext_and_search(client, vaultdir):
    _unlock(client)
    client.post("/api/notes", json={"title": "Restore", "body": "findme keyword here"})
    client.post("/api/notes/restore.md/encrypt")
    assert client.get("/api/search?q=findme").json() == []
    r = client.post("/api/notes/restore.md/decrypt")
    assert r.json()["encrypted"] is False
    assert "findme keyword" in (vaultdir / "restore.md").read_text()
    assert client.get("/api/search?q=findme").json()             # searchable again


def test_encrypt_requires_unlocked_vault(client):
    client.post("/api/notes", json={"title": "Needs Vault", "body": "x"})
    assert client.post("/api/notes/needs-vault.md/encrypt").status_code == 423


def test_encrypted_note_is_not_exportable(client):
    # export is an unauthenticated route — it must never render an encrypted note
    _unlock(client)
    client.post("/api/notes", json={"title": "No Export", "body": "leak me if you can"})
    client.post("/api/notes/no-export.md/encrypt")
    r = client.get("/notes/no-export/export.html")
    assert r.status_code == 404
    assert "leak me" not in r.text
