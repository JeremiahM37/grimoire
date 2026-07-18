"""Version history: snapshots on save, list/view/restore, ring buffer, and the
encrypted-note posture (ciphertext at rest, 423 when locked)."""
from server import history

VAULT_PASS = "hist-pass-12345"


def _save(client, path, body):
    return client.put(f"/api/notes/{path}", json={"body": body})


def test_save_snapshots_previous_version(client):
    client.post("/api/notes", json={"title": "Versioned", "body": "v1"})
    _save(client, "versioned.md", "v2")
    _save(client, "versioned.md", "v3")
    versions = client.get("/api/notes/versioned.md/history").json()
    assert len(versions) == 2                       # v1 and v2 preserved
    newest = client.get(
        f"/api/notes/versioned.md/history/{versions[0]['id']}").json()
    assert newest["body"].rstrip("\n") == "v2"


def test_identical_saves_do_not_pile_up(client):
    client.post("/api/notes", json={"title": "Same", "body": "x"})
    for _ in range(3):
        _save(client, "same.md", "x")               # no content change
    assert client.get("/api/notes/same.md/history").json() == []


def test_restore_rolls_back_and_is_undoable(client):
    client.post("/api/notes", json={"title": "Roll", "body": "good"})
    _save(client, "roll.md", "bad edit")
    versions = client.get("/api/notes/roll.md/history").json()
    r = client.post(f"/api/notes/roll.md/history/{versions[0]['id']}/restore")
    assert r.status_code == 200
    assert client.get("/api/notes/roll.md").json()["body"].rstrip("\n") == "good"
    # the restore snapshotted "bad edit" — so it's recoverable too
    bodies = {client.get(f"/api/notes/roll.md/history/{v['id']}").json()["body"].rstrip("\n")
              for v in client.get("/api/notes/roll.md/history").json()}
    assert "bad edit" in bodies


def test_ring_buffer_caps_versions(client, vaultdir):
    client.post("/api/notes", json={"title": "Cap", "body": "v0"})
    for i in range(history.KEEP + 10):
        _save(client, "cap.md", f"v{i + 1}")
    assert len(client.get("/api/notes/cap.md/history").json()) == history.KEEP


def test_bad_version_id_is_rejected(client):
    client.post("/api/notes", json={"title": "Ids", "body": "x"})
    _save(client, "ids.md", "y")
    for bad in ("../../../etc/passwd", "..%2fescape", "notanid"):
        assert client.get(f"/api/notes/ids.md/history/{bad}").status_code == 404


def test_encrypted_note_history_stays_ciphertext_and_gates_on_lock(client):
    client.post("/api/vault/init", json={"passphrase": VAULT_PASS})
    client.post("/api/notes", json={"title": "Sec", "body": "SECRET-V1"})
    client.post("/api/notes/sec.md/encrypt")
    _save(client, "sec.md", "SECRET-V2")            # re-sealed; snapshots ciphertext
    versions = client.get("/api/notes/sec.md/history").json()
    assert versions
    # on disk: ciphertext only
    raw = history.get_version("sec.md", versions[0]["id"])
    assert "SECRET-V1" not in raw and raw.startswith("grimoire:enc:")
    # unlocked API view decrypts
    body = client.get(f"/api/notes/sec.md/history/{versions[0]['id']}").json()["body"]
    assert body.rstrip("\n") == "SECRET-V1"
    # locked → 423, never ciphertext
    client.post("/api/vault/lock")
    r = client.get(f"/api/notes/sec.md/history/{versions[0]['id']}")
    assert r.status_code == 423
