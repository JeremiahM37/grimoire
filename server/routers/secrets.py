"""Secret vault API — names + meta only; ciphertext and values never returned."""
from fastapi import APIRouter, HTTPException
from pydantic import BaseModel

from .. import secrets
from ..secrets import VaultError, VaultLocked

router = APIRouter(prefix="/api")


@router.get("/vault/status")
def vault_status():
    return secrets.status()


class Passphrase(BaseModel):
    passphrase: str


@router.post("/vault/init")
def vault_init(p: Passphrase):
    if len(p.passphrase) < 8:
        raise HTTPException(400, "passphrase must be at least 8 characters")
    try:
        secrets.initialize(p.passphrase)
    except VaultError as e:
        raise HTTPException(409, str(e))
    return {"ok": True}


@router.post("/vault/unlock")
def vault_unlock(p: Passphrase):
    try:
        secrets.unlock(p.passphrase)
    except ValueError:
        raise HTTPException(401, "wrong passphrase")
    except VaultError as e:
        raise HTTPException(400, str(e))
    return {"ok": True, "unlocked": True}


@router.post("/vault/lock")
def vault_lock():
    secrets.lock()          # panic-lock: drop the in-memory key immediately
    return {"ok": True, "unlocked": False}


class ChangePass(BaseModel):
    old: str
    new: str


@router.post("/vault/change-passphrase")
def change_pass(p: ChangePass):
    try:
        return {"ok": True, **secrets.change_passphrase(p.old, p.new)}
    except VaultError as e:
        raise HTTPException(400, str(e))


@router.get("/grants")
def get_grants():
    try:
        return secrets.list_grants()
    except VaultLocked:
        raise HTTPException(423, "vault locked")


@router.delete("/grants/{token}")
def revoke(token: str):
    return {"revoked": secrets.revoke_grant(token)}


@router.delete("/grants")
def revoke_all():
    return {"revoked": secrets.revoke_all_grants()}


@router.get("/secrets")
def list_secrets():
    try:
        return secrets.list_names()   # names + meta ONLY, never values
    except VaultLocked:
        raise HTTPException(423, "vault locked")


class SecretIn(BaseModel):
    name: str
    value: str
    meta: dict = {}


@router.post("/secrets", status_code=201)
def add_secret(s: SecretIn):
    try:
        secrets.set_secret(s.name, s.value, s.meta)
    except VaultLocked:
        raise HTTPException(423, "vault locked")
    return {"name": s.name}


@router.delete("/secrets/{name}", status_code=204)
def del_secret(name: str):
    try:
        secrets.delete_secret(name)
    except VaultLocked:
        raise HTTPException(423, "vault locked")


class GrantIn(BaseModel):
    grantee: str
    scope: str = ""          # allowed URL prefix; "" = any (discouraged)
    ttl_seconds: int = 900


@router.post("/secrets/{name}/grant")
def make_grant(name: str, g: GrantIn):
    try:
        token = secrets.grant(name, g.grantee, g.scope, g.ttl_seconds)
    except VaultLocked:
        raise HTTPException(423, "vault locked")
    except VaultError as e:
        raise HTTPException(400, str(e))
    return {"grant": token, "expires_in": g.ttl_seconds}


class BrokerIn(BaseModel):
    grant: str
    method: str = "GET"
    url: str
    header: str = "Authorization"
    body: str | None = None


@router.post("/secrets/broker")
def broker(b: BrokerIn):
    """USE-not-READ: mnemo makes the call with the secret injected; the caller
    never sees the value."""
    try:
        return secrets.broker(b.grant, b.method, b.url, b.header, b.body)
    except VaultLocked:
        raise HTTPException(423, "vault locked")
    except VaultError as e:
        raise HTTPException(403, str(e))


@router.get("/audit")
def audit():
    return secrets.audit_log()
