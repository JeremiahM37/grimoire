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
import urllib.parse
import urllib.request

from . import crdtstore, db, index, vault
from .routers.sync import _conflict_name, _write_raw
from .vault import _serialize

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


def sync_with_peer(peer: str, device: str = "grimoire", token: str | None = None) -> dict:
    peer = peer.rstrip("/")
    remote = _req(f"{peer}/api/sync/manifest", token=token)
    local = local_manifest()

    pulled = pushed = conflicts = merged = 0
    to_pull: list[str] = []
    push_changes: list[dict] = []

    # Every path that differs (or is only on one side) is handled CRDT-first so
    # both replicas end up sharing atom ids. Encrypted/oversized notes can't be
    # merged as text → fall back to last-writer pull/push.
    for path in set(local) | set(remote):
        lm, rm = local.get(path), remote.get(path)
        if lm and rm and lm["hash"] == rm["hash"]:
            continue                                   # already in sync

        note = None
        if lm:
            try:
                note = vault.read(path)
            except Exception:
                note = None
        our_body = note["body"] if note else ""
        our_fm = note["frontmatter"] if note else {}
        our_raw = note["raw"] if note else ""
        # not mergeable locally (encrypted) → last-writer
        if lm and not crdtstore.mergeable(path, our_body):
            if rm and rm["mtime"] > lm["mtime"]:
                to_pull.append(path)
            else:
                push_changes.append({"path": path, "content": our_raw,
                                     "base_hash": rm["hash"] if rm else None})
            continue

        try:
            # snapshot our own body doc BEFORE merging peer's in
            our_doc = crdtstore.body_doc_json(path, our_body) if lm else None
            if rm:   # peer has it → pull its doc + fm and merge
                enc = urllib.parse.quote(path, safe="/")
                pr = _req(f"{peer}/api/crdt/doc/{enc}", token=token)
                merged_body, win_fm, clean = crdtstore.merge_body(
                    path, our_body, our_fm, pr["doc"], pr.get("fm", {}))
                new_raw = _serialize(win_fm, merged_body)
                if new_raw != our_raw:
                    if not clean and our_raw.strip():   # independent histories → keep ours as a copy
                        _local_conflict_copy(path)
                        conflicts += 1
                    _write_raw(path, new_raw)
                    index.upsert(path)
                merged += 1 if lm else 0
                pulled += 0 if lm else 1
            if lm:   # push our (pre-merge) body doc + fm so the peer converges too
                _req(f"{peer}/api/crdt/merge", "POST",
                     {"path": path, "doc": our_doc, "fm": our_fm}, token)
                if not rm:
                    pushed += 1
        except Exception:   # peer 409 (encrypted) or transient → last-writer fallback
            log.debug("crdt path fell back for %s", path)
            if rm and (not lm or rm["mtime"] > (lm["mtime"] if lm else 0)):
                to_pull.append(path)
            elif lm:
                push_changes.append({"path": path, "content": our_raw,
                                     "base_hash": rm["hash"] if rm else None})

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

    stats = {"pulled": pulled, "pushed": pushed, "merged": merged, "conflicts": conflicts,
             "remote_notes": len(remote), "local_notes": len(local)}
    log.info("sync %s: %s", peer, stats)
    return stats
