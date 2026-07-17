"""Bidirectional sync client — pairs with routers/sync.py on a peer mnemo.

Diffs the local manifest against a peer's, then pulls newer/missing notes down
and pushes newer/missing notes up. Direction is decided by mtime (last-writer),
but data is NEVER lost: pushing conflicts are turned into conflict copies by the
peer (base_hash check), and before a pull OVERWRITES a differing local note we
first preserve the local version as a conflict copy.

Runs on a timer from the app lifespan (MNEMO_SYNC_PEER + MNEMO_SYNC_INTERVAL),
on demand via POST /api/sync/now, or from the CLI (`mnemo sync`).
"""
import json
import logging
import urllib.request

from . import db, index, vault
from .routers.sync import _conflict_name, _write_raw

log = logging.getLogger("mnemo.sync")


def _req(url: str, method: str = "GET", body=None, token: str | None = None, timeout: int = 30):
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, method=method, data=data, headers=headers)
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def local_manifest() -> dict:
    return {n["path"]: {"hash": n["hash"], "mtime": n["mtime"]}
            for n in db.query("SELECT path, hash, mtime FROM notes")}


def _local_conflict_copy(path: str) -> None:
    try:
        raw = vault.read(path)["raw"]
        cp = _conflict_name(path)
        _write_raw(cp, raw)
        index.upsert(cp)
    except Exception:
        log.debug("could not conflict-copy %s", path)


def sync_with_peer(peer: str, device: str = "mnemo", token: str | None = None) -> dict:
    peer = peer.rstrip("/")
    remote = _req(f"{peer}/api/sync/manifest", token=token)
    local = local_manifest()

    to_pull: list[str] = []
    push_changes: list[dict] = []

    # decide direction per path
    for path, meta in remote.items():
        lm = local.get(path)
        if not lm:
            to_pull.append(path)                       # peer has it, we don't → pull
        elif lm["hash"] != meta["hash"]:
            if meta["mtime"] > lm["mtime"]:
                to_pull.append(path)                   # peer newer → pull
            else:
                push_changes.append({"path": path, "content": vault.read(path)["raw"],
                                     "base_hash": meta["hash"]})   # we're newer → push
    for path, lm in local.items():
        if path not in remote:
            push_changes.append({"path": path, "content": vault.read(path)["raw"],
                                 "base_hash": None})    # we have it, peer doesn't → push

    pulled = pushed = conflicts = 0

    if to_pull:
        contents = _req(f"{peer}/api/sync/pull", "POST", {"paths": to_pull}, token)["contents"]
        for path, raw in contents.items():
            if raw is None:
                continue
            if local.get(path):        # about to overwrite a differing local note — preserve it
                _local_conflict_copy(path)
                conflicts += 1
            _write_raw(path, raw)
            index.upsert(path)
            pulled += 1

    if push_changes:
        res = _req(f"{peer}/api/sync/push", "POST",
                   {"changes": push_changes, "device": device}, token)["results"]
        for r in res:
            if r.get("status") in ("ok", "created", "deleted"):
                pushed += 1
            elif str(r.get("status", "")).startswith("conflict"):
                conflicts += 1

    stats = {"pulled": pulled, "pushed": pushed, "conflicts": conflicts,
             "remote_notes": len(remote), "local_notes": len(local)}
    log.info("sync %s: %s", peer, stats)
    return stats
