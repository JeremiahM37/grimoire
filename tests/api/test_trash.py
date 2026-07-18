"""Soft-delete: deleting a note is recoverable via trash."""


def test_delete_moves_to_trash_and_is_restorable(client, vaultdir):
    client.post("/api/notes", json={"title": "Deleteme", "body": "precious content here"})
    assert client.get("/api/search?q=precious").json()
    r = client.delete("/api/notes/deleteme.md")
    assert r.status_code == 200
    tid = r.json()["trashed"]
    # gone from vault + index, file no longer at original path
    assert not (vaultdir / "deleteme.md").exists()
    assert client.get("/api/notes/deleteme.md").status_code == 404
    assert client.get("/api/search?q=precious").json() == []
    # ...but present in trash
    trash = client.get("/api/trash").json()
    assert any(t["id"] == tid and t["title"] == "Deleteme" for t in trash)
    # restore brings it back, searchable again
    back = client.post(f"/api/trash/{tid}/restore")
    assert back.status_code == 200 and back.json()["path"] == "deleteme.md"
    assert (vaultdir / "deleteme.md").exists()
    assert client.get("/api/search?q=precious").json()
    assert client.get("/api/trash").json() == []


def test_restore_auto_suffixes_when_path_reused(client, vaultdir):
    client.post("/api/notes", json={"title": "Recur", "body": "one"})
    tid = client.delete("/api/notes/recur.md").json()["trashed"]
    # a new note takes the original path
    client.post("/api/notes", json={"title": "Recur", "body": "two"})
    rel = client.post(f"/api/trash/{tid}/restore").json()["path"]
    assert rel == "recur-2.md"                      # didn't clobber the new note
    assert (vaultdir / "recur.md").read_text().find("two") != -1


def test_purge_is_permanent(client):
    tid = (client.post("/api/notes", json={"title": "Gone", "body": "x"}),
           client.delete("/api/notes/gone.md").json()["trashed"])[1]
    assert client.delete(f"/api/trash/{tid}").status_code == 204
    assert client.get("/api/trash").json() == []
    assert client.post(f"/api/trash/{tid}/restore").status_code == 404


def test_encrypted_note_stays_encrypted_in_trash(client, vaultdir):
    client.post("/api/vault/init", json={"passphrase": "trashpass12345"})
    client.post("/api/notes", json={"title": "Enc Trash", "body": "sealed secret"})
    client.post("/api/notes/enc-trash.md/encrypt")
    client.delete("/api/notes/enc-trash.md")
    # the trashed file is still ciphertext — plaintext never exposed in trash
    trashfiles = list((vaultdir / ".grimoire" / "trash").glob("*.md"))
    assert trashfiles and "sealed secret" not in trashfiles[0].read_text()
    assert "grimoire:enc:v1:" in trashfiles[0].read_text()
