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
import time

from . import config, crypto, db

# in-memory session key (never persisted); None when locked
_key: bytes | None = None


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
    return _key is not None


def status() -> dict:
    return {"initialized": is_initialized(), "unlocked": is_unlocked(),
            "count": len(list_names()) if is_unlocked() else None}


class VaultLocked(Exception):
    pass


class VaultError(Exception):
    pass


def _require_unlocked():
    if _key is None:
        raise VaultLocked("vault is locked")


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
    salt = crypto.new_salt()
    import base64
    key = crypto.derive_key(passphrase, salt)
    # a verifier: seal a known token so unlock can validate the passphrase
    blob = {"salt": base64.b64encode(salt).decode(),
            "verifier": base64.b64encode(crypto.seal(key, b"mnemo-vault-v1")).decode()}
    _save_blob(blob)
    global _key
    _key = key
    _write_payload({})


def unlock(passphrase: str) -> None:
    global _key
    blob = _load_blob()
    if not blob.get("salt"):
        raise VaultError("vault not initialized")
    import base64
    key = crypto.derive_key(passphrase, base64.b64decode(blob["salt"]))
    # validate against the verifier — raises ValueError on wrong passphrase
    crypto.unseal(key, base64.b64decode(blob["verifier"]))
    _key = key


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
    import secrets as pysecrets
    token = pysecrets.token_urlsafe(24)
    db.execute(
        "INSERT INTO grants(token,secret,grantee,scope,expires_at,created) VALUES(?,?,?,?,?,?)",
        (token, secret, grantee, scope, _now() + ttl_seconds, _iso()))
    _audit("grant", secret=secret, detail=f"grantee={grantee} scope={scope} ttl={ttl_seconds}")
    return token


def _valid_grant(token: str, url: str) -> dict:
    g = db.one("SELECT * FROM grants WHERE token=?", (token,))
    if not g:
        raise VaultError("invalid grant")
    if g["expires_at"] < _now():
        raise VaultError("grant expired")
    if g["scope"] and not url.startswith(g["scope"]):
        raise VaultError(f"grant scope does not permit {url}")
    return g


def broker(token: str, method: str, url: str, header: str,
           body: str | None = None) -> dict:
    """USE-not-READ: make an outbound request injecting the secret into `header`,
    authorized by a grant. The secret value never returns to the caller."""
    _require_unlocked()
    g = _valid_grant(token, url)
    value = _get_value(g["secret"])
    import urllib.request
    req = urllib.request.Request(url, method=method.upper(),
                                 data=body.encode() if body else None,
                                 headers={header: value, "Content-Type": "application/json"})
    _audit("broker", secret=g["secret"], detail=f"{method} {url}")
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return {"status": r.status, "body": r.read(65536).decode(errors="replace")}
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
    """Drop the in-memory key between tests."""
    global _key
    _key = None
