"""The secret vault — mnemo's unique differentiator.

A sealed store for AI agent tokens, API keys, and MCP credentials that your AI
can USE but never READ:

- Secrets are encrypted at rest in `.mnemo/secrets.enc` (never in the notes, never
  indexed, never in search/RAG). The passphrase is never stored.
- The vault is LOCKED by default. `unlock(passphrase)` holds the derived key in
  memory only; `lock()` / panic-lock drops it.
- An agent doesn't get raw secrets. It gets a **grant** (scoped + time-boxed) and
  mnemo BROKERS the call — injecting the secret into an outbound request — so the
  value never crosses to the client. Every use is written to an audit log.
"""
import json
import os
import time

from . import config, crypto, db

# in-memory session key (never persisted); None when locked
_key: bytes | None = None

# --- brute-force + idle protection (in-memory, per process) -------------------
_failures = 0
_lock_until = 0.0
_last_activity = 0.0
MAX_FAILURES = 5
# auto-lock the vault after this many idle seconds (0 disables)
IDLE_LOCK_SECONDS = int(os.environ.get("MNEMO_VAULT_IDLE_LOCK", "900"))


def _touch() -> None:
    global _last_activity
    _last_activity = time.time()


def _check_lockout() -> None:
    remaining = _lock_until - time.time()
    if remaining > 0:
        raise VaultError(f"too many attempts — locked for {int(remaining) + 1}s")


def _record_failure() -> None:
    global _failures, _lock_until
    _failures += 1
    if _failures >= MAX_FAILURES:
        # exponential backoff: 30s, 60s, 120s … capped at 1h
        _lock_until = time.time() + min(3600, 30 * (2 ** (_failures - MAX_FAILURES)))


def _reset_failures() -> None:
    global _failures, _lock_until
    _failures = 0
    _lock_until = 0.0

# marker for an encrypted note body on disk
ENC_PREFIX = "mnemo:enc:v1:"


def is_encrypted(body: str) -> bool:
    return body.lstrip().startswith(ENC_PREFIX)


def seal_text(plaintext: str) -> str:
    """Seal a note body with the (unlocked) vault key. Requires unlock."""
    _require_unlocked()
    return ENC_PREFIX + crypto.seal(_key, plaintext.encode("utf-8")).decode()


def unseal_text(body: str) -> str:
    """Decrypt an encrypted note body. Requires unlock; raises on wrong key."""
    _require_unlocked()
    b = body.lstrip()
    if not b.startswith(ENC_PREFIX):
        return body
    return crypto.unseal(_key, b[len(ENC_PREFIX):].encode("utf-8")).decode("utf-8")


def store_path():
    return config.mnemo_dir() / "secrets.enc"


def _load_blob() -> dict:
    p = store_path()
    if not p.exists():
        return {}
    return json.loads(p.read_text())


def _save_blob(blob: dict) -> None:
    config.mnemo_dir().mkdir(parents=True, exist_ok=True)
    store_path().write_text(json.dumps(blob))


def is_initialized() -> bool:
    return bool(_load_blob().get("salt"))


def is_unlocked() -> bool:
    global _key
    if _key is not None and IDLE_LOCK_SECONDS > 0 and (time.time() - _last_activity) > IDLE_LOCK_SECONDS:
        _key = None   # auto-lock after idle — shrink the key's exposure window
    return _key is not None


def status() -> dict:
    return {"initialized": is_initialized(), "unlocked": is_unlocked(),
            "count": len(list_names()) if is_unlocked() else None}


class VaultLocked(Exception):
    pass


class VaultError(Exception):
    pass


def _require_unlocked():
    if not is_unlocked():
        raise VaultLocked("vault is locked")
    _touch()   # refresh the idle timer on every sensitive operation


def _payload() -> dict:
    """Decrypt the secrets payload {name: {value, meta}}. Requires unlock."""
    _require_unlocked()
    blob = _load_blob()
    if not blob.get("secrets"):
        return {}
    import base64
    return json.loads(crypto.unseal(_key, base64.b64decode(blob["secrets"])))


def _write_payload(payload: dict) -> None:
    import base64
    blob = _load_blob()
    blob["secrets"] = base64.b64encode(crypto.seal(_key, json.dumps(payload).encode())).decode()
    _save_blob(blob)


def initialize(passphrase: str) -> None:
    if is_initialized():
        raise VaultError("vault already initialized")
    if len(passphrase) < 8:
        raise VaultError("passphrase must be at least 8 characters")
    salt = crypto.new_salt()
    import base64
    key = crypto.derive_key(passphrase, salt, crypto.DEFAULT_KDF)
    # a verifier: seal a known token so unlock can validate the passphrase
    blob = {"salt": base64.b64encode(salt).decode(),
            "verifier": base64.b64encode(crypto.seal(key, b"mnemo-vault-v1")).decode(),
            "kdf": crypto.DEFAULT_KDF}
    _save_blob(blob)
    global _key
    _key = key
    _touch()
    _write_payload({})


def unlock(passphrase: str) -> None:
    global _key
    _check_lockout()   # brute-force backoff
    blob = _load_blob()
    if not blob.get("salt"):
        raise VaultError("vault not initialized")
    import base64
    kdf = blob.get("kdf", "pbkdf2")   # legacy vaults predate the kdf field
    key = crypto.derive_key(passphrase, base64.b64decode(blob["salt"]), kdf)
    try:
        # validate against the verifier — raises ValueError on wrong passphrase
        crypto.unseal(key, base64.b64decode(blob["verifier"]))
    except ValueError:
        _record_failure()
        raise
    _reset_failures()
    _key = key
    _touch()


def lock() -> None:
    global _key
    _key = None


def list_names() -> list[dict]:
    p = _payload()
    return [{"name": n, "meta": v.get("meta", {})} for n, v in sorted(p.items())]


def set_secret(name: str, value: str, meta: dict | None = None) -> None:
    _require_unlocked()
    p = _payload()
    p[name] = {"value": value, "meta": meta or {}}
    _write_payload(p)
    _audit("set", secret=name)


def delete_secret(name: str) -> None:
    p = _payload()
    if name in p:
        del p[name]
        _write_payload(p)
        _audit("delete", secret=name)


def _get_value(name: str) -> str:
    """INTERNAL ONLY — never exposed via API. Used by the broker."""
    p = _payload()
    if name not in p:
        raise VaultError(f"no such secret: {name}")
    return p[name]["value"]


# ---- grants (scoped, time-boxed authorization to USE a secret) ---------------

def grant(secret: str, grantee: str, scope: str, ttl_seconds: int) -> str:
    """Issue a grant token allowing `grantee` to have the secret brokered against
    `scope` (an allowed URL prefix) until it expires."""
    _require_unlocked()
    if secret not in {s["name"] for s in list_names()}:
        raise VaultError(f"no such secret: {secret}")
    from urllib.parse import urlparse
    sp = urlparse(scope or "")
    if sp.scheme not in ("http", "https") or not sp.hostname:
        raise VaultError("grant scope must be an absolute http(s) URL (e.g. https://api.github.com/)")
    import secrets as pysecrets
    token = pysecrets.token_urlsafe(24)
    db.execute(
        "INSERT INTO grants(token,secret,grantee,scope,expires_at,created) VALUES(?,?,?,?,?,?)",
        (token, secret, grantee, scope, _now() + ttl_seconds, _iso()))
    _audit("grant", secret=secret, detail=f"grantee={grantee} scope={scope} ttl={ttl_seconds}")
    return token


def _scope_permits(scope: str, url: str) -> bool:
    """Origin-exact + path-prefix match. Prevents the classic prefix bypass where
    `https://api.github.com` would otherwise 'match' `https://api.github.com.evil.com`."""
    from urllib.parse import urlparse
    s, u = urlparse(scope), urlparse(url)
    if (s.scheme.lower(), s.hostname, s.port) != (u.scheme.lower(), u.hostname, u.port):
        return False
    sp = s.path if s.path.endswith("/") or not s.path else s.path
    return u.path.startswith(s.path) or (u.path + "/").startswith(sp)


def _assert_url_safe(url: str) -> None:
    """SSRF guard: only http/https, and (by default) refuse to broker a secret to
    a private / loopback / link-local / reserved address. Cloud-metadata and
    link-local are ALWAYS blocked. Set MNEMO_BROKER_ALLOW_PRIVATE=1 to reach
    internal hosts (e.g. a self-hosted homelab)."""
    import ipaddress
    import socket
    from urllib.parse import urlparse
    p = urlparse(url)
    if p.scheme not in ("http", "https"):
        raise VaultError("broker: only http(s) URLs are allowed")
    host = p.hostname
    if not host:
        raise VaultError("broker: URL has no host")
    allow_private = os.environ.get("MNEMO_BROKER_ALLOW_PRIVATE") == "1"
    try:
        infos = socket.getaddrinfo(host, p.port or (443 if p.scheme == "https" else 80),
                                   proto=socket.IPPROTO_TCP)
    except socket.gaierror:
        raise VaultError("broker: could not resolve host")
    for info in infos:
        ip = ipaddress.ip_address(info[4][0])
        if ip.is_link_local or str(ip) in ("169.254.169.254", "::ffff:169.254.169.254"):
            raise VaultError("broker: blocked link-local / cloud-metadata address")
        if not allow_private and (ip.is_private or ip.is_loopback or ip.is_reserved
                                  or ip.is_multicast or ip.is_unspecified):
            raise VaultError(f"broker: refusing non-public address {ip} "
                             "(set MNEMO_BROKER_ALLOW_PRIVATE=1 for internal hosts)")


def _valid_grant(token: str, url: str) -> dict:
    g = db.one("SELECT * FROM grants WHERE token=?", (token,))
    if not g:
        raise VaultError("invalid grant")
    if g["expires_at"] < _now():
        raise VaultError("grant expired")
    if not g["scope"] or not _scope_permits(g["scope"], url):
        raise VaultError(f"grant scope does not permit {url}")
    return g


def broker(token: str, method: str, url: str, header: str,
           body: str | None = None) -> dict:
    """USE-not-READ: make an outbound request injecting the secret into `header`,
    authorized by a grant. The secret value never returns to the caller."""
    _require_unlocked()
    g = _valid_grant(token, url)
    _assert_url_safe(url)
    value = _get_value(g["secret"])
    import urllib.request
    req = urllib.request.Request(url, method=method.upper(),
                                 data=body.encode() if body else None,
                                 headers={header: value, "Content-Type": "application/json"})
    _audit("broker", secret=g["secret"], detail=f"{method} {url}")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return {"status": r.status, "body": r.read(65536).decode(errors="replace")}
    except VaultError:
        raise
    except Exception as e:  # noqa: BLE001
        return {"status": 0, "error": str(e)}


def audit_log(limit: int = 200) -> list[dict]:
    return db.query("SELECT * FROM audit ORDER BY id DESC LIMIT ?", (limit,))


def _audit(action: str, secret: str | None = None, detail: str = "") -> None:
    db.execute("INSERT INTO audit(ts,action,secret,detail) VALUES(?,?,?,?)",
               (_iso(), action, secret, detail))


def _now() -> float:
    return time.time()


def _iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%S")


def reset_for_tests() -> None:
    """Drop the in-memory key and protection state between tests."""
    global _key, _failures, _lock_until, _last_activity
    _key = None
    _failures = 0
    _lock_until = 0.0
    _last_activity = 0.0
