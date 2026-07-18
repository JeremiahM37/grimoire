"""Adversarial security tests for the secret vault, broker, and HTTP surface."""
import time

import pytest

from server import crypto, secrets

PASS = "correct horse battery staple"


def _init(client):
    client.post("/api/vault/init", json={"passphrase": PASS})


# ---- key derivation ---------------------------------------------------------

def test_new_vaults_use_argon2id(client):
    _init(client)
    import json
    blob = json.loads(secrets.store_path().read_text())
    assert blob["kdf"] == "argon2id"


def test_kdf_backward_compatible_with_pbkdf2(vaultdir):
    # simulate a legacy pbkdf2 vault and confirm it still unlocks
    import base64
    import json
    salt = crypto.new_salt()
    key = crypto.derive_key(PASS, salt, "pbkdf2")
    blob = {"salt": base64.b64encode(salt).decode(),
            "verifier": base64.b64encode(crypto.seal(key, b"grimoire-vault-v1")).decode(),
            "kdf": "pbkdf2"}
    secrets.store_path().write_text(json.dumps(blob))
    secrets.unlock(PASS)          # must not raise
    assert secrets.is_unlocked()


def test_argon2id_and_pbkdf2_derive_differently_but_deterministically():
    salt = b"0123456789abcdef"
    a1 = crypto.derive_key("pw", salt, "argon2id")
    a2 = crypto.derive_key("pw", salt, "argon2id")
    p1 = crypto.derive_key("pw", salt, "pbkdf2")
    assert a1 == a2 and a1 != p1


# ---- brute-force + idle protection ------------------------------------------

def test_unlock_lockout_after_repeated_failures(client):
    _init(client)
    client.post("/api/vault/lock")
    for _ in range(secrets.MAX_FAILURES):
        r = client.post("/api/vault/unlock", json={"passphrase": "wrong"})
        assert r.status_code == 401
    # even the CORRECT passphrase is now refused during the lockout window
    r = client.post("/api/vault/unlock", json={"passphrase": PASS})
    assert r.status_code != 200


def test_idle_auto_lock(client, monkeypatch):
    _init(client)
    assert secrets.is_unlocked()
    monkeypatch.setattr(secrets, "IDLE_LOCK_SECONDS", 1)
    secrets._last_activity = time.time() - 5    # pretend 5s idle
    assert secrets.is_unlocked() is False       # auto-locked


# ---- broker SSRF + scope ----------------------------------------------------

def test_broker_scope_no_prefix_bypass():
    # the classic bypass: evil host that starts with the allowed host string
    assert secrets._scope_permits("https://api.github.com/", "https://api.github.com/x") is True
    assert secrets._scope_permits("https://api.github.com/", "https://api.github.com.evil.com/") is False
    assert secrets._scope_permits("https://api.github.com/", "http://api.github.com/") is False   # scheme
    assert secrets._scope_permits("https://api.github.com/v1/", "https://api.github.com/v2/") is False  # path


@pytest.mark.parametrize("url", [
    "http://127.0.0.1/", "http://10.0.0.5/", "http://192.168.1.1/",
    "http://169.254.169.254/latest/meta-data/", "http://[::1]/", "http://0.0.0.0/",
])
def test_broker_blocks_private_and_metadata(url, monkeypatch):
    monkeypatch.delenv("GRIMOIRE_BROKER_ALLOW_PRIVATE", raising=False)
    with pytest.raises(secrets.VaultError):
        secrets._assert_url_safe(url)


def test_metadata_blocked_even_when_private_allowed(monkeypatch):
    monkeypatch.setenv("GRIMOIRE_BROKER_ALLOW_PRIVATE", "1")
    # link-local / cloud metadata is ALWAYS refused
    with pytest.raises(secrets.VaultError):
        secrets._assert_url_safe("http://169.254.169.254/")
    # but an ordinary private host is now allowed through the guard
    secrets._assert_url_safe("http://10.0.0.5/")


def test_broker_allows_public_ip():
    secrets._assert_url_safe("https://8.8.8.8/")     # public — no raise


def test_broker_rejects_non_http_scheme():
    with pytest.raises(secrets.VaultError):
        secrets._assert_url_safe("file:///etc/passwd")
    with pytest.raises(secrets.VaultError):
        secrets._assert_url_safe("gopher://x/")


def test_grant_requires_absolute_url_scope(client):
    _init(client)
    client.post("/api/secrets", json={"name": "gh", "value": "ghp_x"})
    r = client.post("/api/secrets/gh/grant", json={"grantee": "ai", "scope": "notaurl", "ttl_seconds": 60})
    assert r.status_code >= 400


def test_broker_to_private_blocked_end_to_end(client):
    _init(client)
    client.post("/api/secrets", json={"name": "gh", "value": "ghp_secretvalue"})
    g = client.post("/api/secrets/gh/grant",
                    json={"grantee": "ai", "scope": "http://127.0.0.1:9/", "ttl_seconds": 60}).json()
    r = client.post("/api/secrets/broker",
                    json={"grant": g["grant"], "url": "http://127.0.0.1:9/", "method": "GET"})
    # blocked before any request is made; the secret is never transmitted or returned
    body = r.text
    assert "ghp_secretvalue" not in body
    assert r.status_code >= 400 or "refusing non-public" in body.lower() or "blocked" in body.lower()


# ---- HTTP surface -----------------------------------------------------------

def test_security_headers_present(client):
    h = client.get("/api/health").headers
    assert h["x-content-type-options"] == "nosniff"
    assert "content-security-policy" in h and "script-src 'self'" in h["content-security-policy"]
    assert h["referrer-policy"] == "no-referrer"


def test_secret_value_never_returned_by_api(client):
    _init(client)
    client.post("/api/secrets", json={"name": "api", "value": "sk-supersecret-123"})
    listing = client.get("/api/secrets").text
    assert "sk-supersecret-123" not in listing         # names/meta only
    # and it isn't retrievable through any documented note/search path
    assert "sk-supersecret-123" not in client.get("/api/search?q=supersecret").text


def test_audit_log_requires_unlock(client):
    _init(client)
    client.post("/api/secrets", json={"name": "gh", "value": "ghp_secret_audit"})
    client.post("/api/vault/lock")
    r = client.get("/api/audit")
    assert r.status_code == 423                    # no vault metadata while locked
    assert "gh" not in r.text and "ghp_secret_audit" not in r.text
