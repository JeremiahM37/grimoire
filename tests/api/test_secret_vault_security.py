"""Negative / adversarial: the secret vault's security invariants.

If any of these fail, the vault's entire premise is broken — so they're the most
important tests in mnemo.
"""


def _armed(client, pw="passphrase-strong"):
    client.post("/api/vault/init", json={"passphrase": pw})
    client.post("/api/secrets", json={"name": "apikey", "value": "sk-DEADBEEF-secret"})
    return pw


def test_secret_value_never_returned_by_any_endpoint(client):
    _armed(client)
    # exhaustively: no API surface returns the raw value
    for path in ["/api/secrets", "/api/vault/status", "/api/audit"]:
        assert "sk-DEADBEEF-secret" not in client.get(path).text


def test_secret_never_in_notes_search_or_rag(client):
    _armed(client)
    # a note references the secret by HANDLE only — the value must never be indexed
    client.post("/api/notes", json={"title": "Deploy", "body":
        "Use {{secret:apikey}} to call the service. The apikey deploys things."})
    # search for the token → nothing; search for surrounding words → the note, but no value
    assert client.get("/api/search", params={"q": "DEADBEEF"}).json() == []
    res = client.get("/api/search", params={"q": "deploy service"}).json()
    assert all("sk-DEADBEEF-secret" not in (r.get("snippet") or "") for r in res)
    ask = client.post("/api/ask", json={"q": "how do I deploy"}).json()
    assert "sk-DEADBEEF-secret" not in ask["answer"]


def test_locked_vault_denies_everything(client):
    _armed(client)
    client.post("/api/vault/lock")
    assert client.get("/api/secrets").status_code == 423
    assert client.post("/api/secrets", json={"name": "x", "value": "y"}).status_code == 423
    assert client.post("/api/secrets/apikey/grant",
                       json={"grantee": "a", "scope": "http://x/"}).status_code == 423


def test_wrong_passphrase_rejected(client):
    _armed(client, pw="the-real-one")
    client.post("/api/vault/lock")
    assert client.post("/api/vault/unlock", json={"passphrase": "guess"}).status_code == 401
    assert client.post("/api/vault/unlock", json={"passphrase": "the-real-one"}).status_code == 200


def test_broker_denies_out_of_scope_url(client):
    _armed(client)
    g = client.post("/api/secrets/apikey/grant", json={
        "grantee": "a", "scope": "https://allowed.test/", "ttl_seconds": 60}).json()["grant"]
    r = client.post("/api/secrets/broker", json={
        "grant": g, "url": "https://evil.test/steal", "header": "Authorization"})
    assert r.status_code == 403 and "scope" in r.json()["detail"]


def test_broker_denies_expired_grant(client):
    _armed(client)
    g = client.post("/api/secrets/apikey/grant", json={
        "grantee": "a", "scope": "http://ok.test/", "ttl_seconds": 0}).json()["grant"]
    import time
    time.sleep(0.05)
    r = client.post("/api/secrets/broker", json={
        "grant": g, "url": "http://ok.test/x", "header": "Authorization"})
    assert r.status_code == 403 and "expired" in r.json()["detail"]


def test_broker_denies_invalid_grant(client):
    _armed(client)
    r = client.post("/api/secrets/broker", json={
        "grant": "forged-token", "url": "http://x/", "header": "Authorization"})
    assert r.status_code == 403


def test_grant_for_missing_secret_rejected(client):
    _armed(client)
    assert client.post("/api/secrets/ghost/grant",
                       json={"grantee": "a", "scope": "http://x/"}).status_code == 400


def test_secrets_file_on_disk_is_ciphertext(client, vaultdir):
    _armed(client)
    blob = (vaultdir / ".mnemo" / "secrets.enc").read_text()
    assert "sk-DEADBEEF-secret" not in blob and "apikey" not in blob   # sealed
