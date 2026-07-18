"""Passphrase rotation + grant revocation."""
from server import secrets

OLD = "old passphrase here"
NEW = "new passphrase stronger"


def test_change_passphrase_reseals_secrets_and_notes(client, vaultdir):
    client.post("/api/vault/init", json={"passphrase": OLD})
    client.post("/api/secrets", json={"name": "gh", "value": "ghp_rotate_me"})
    client.post("/api/notes", json={"title": "Sealed", "body": "classified body"})
    client.post("/api/notes/sealed.md/encrypt")
    # rotate
    r = client.post("/api/vault/change-passphrase", json={"old": OLD, "new": NEW})
    assert r.status_code == 200 and r.json()["reencrypted_notes"] == 1
    # old passphrase no longer unlocks; new one does
    client.post("/api/vault/lock")
    assert client.post("/api/vault/unlock", json={"passphrase": OLD}).status_code == 401
    assert client.post("/api/vault/unlock", json={"passphrase": NEW}).status_code == 200
    # secret still usable, note still decryptable under the new key
    assert secrets._get_value("gh") == "ghp_rotate_me"
    note = client.get("/api/notes/sealed.md").json()
    assert note["encrypted"] and note["body"].strip() == "classified body"
    # disk still ciphertext, no plaintext leak
    disk = (vaultdir / "sealed.md").read_text()
    assert "classified body" not in disk and "grimoire:enc:v1:" in disk


def test_change_passphrase_wrong_old_rejected(client):
    client.post("/api/vault/init", json={"passphrase": OLD})
    r = client.post("/api/vault/change-passphrase", json={"old": "nope", "new": NEW})
    assert r.status_code == 400


def test_grant_list_and_revoke(client):
    client.post("/api/vault/init", json={"passphrase": OLD})
    client.post("/api/secrets", json={"name": "svc", "value": "v"})
    tok = client.post("/api/secrets/svc/grant",
                      json={"grantee": "ai", "scope": "https://api.x.com/", "ttl_seconds": 300}).json()["grant"]
    grants = client.get("/api/grants").json()
    assert any(g["token"] == tok and g["secret"] == "svc" for g in grants)
    # revoke it → broker with it now fails
    assert client.delete(f"/api/grants/{tok}").json()["revoked"] is True
    assert client.get("/api/grants").json() == []
    r = client.post("/api/secrets/broker", json={"grant": tok, "url": "https://api.x.com/", "method": "GET"})
    assert r.status_code >= 400 or "invalid grant" in r.text.lower()


def test_grants_require_unlock(client):
    client.post("/api/vault/init", json={"passphrase": OLD})
    client.post("/api/vault/lock")
    assert client.get("/api/grants").status_code == 423
