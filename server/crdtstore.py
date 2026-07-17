"""Per-note CRDT documents, persisted in .mnemo/crdt/, keyed by note path.

We CRDT the note **body** (not the raw file) so the volatile frontmatter
timestamps don't garble a character merge. On merge the body is auto-merged
(sequence CRDT) and the frontmatter is chosen deterministically — the block with
the newer `updated` wins, tie-broken by its serialization — so BOTH replicas
converge to byte-identical files (no conflict copies, no sync churn). Encrypted
notes are skipped (ciphertext isn't mergeable) and fall back to last-writer sync.
"""
import hashlib
import json
import secrets as pysecrets

from . import config, crdt

MAX_CRDT_BYTES = 200_000


def site_id() -> str:
    p = config.mnemo_dir() / "site_id"
    if p.exists():
        return p.read_text().strip()
    config.mnemo_dir().mkdir(parents=True, exist_ok=True)
    sid = pysecrets.token_hex(4)
    p.write_text(sid)
    return sid


def _dir():
    d = config.mnemo_dir() / "crdt"
    d.mkdir(parents=True, exist_ok=True)
    return d


def _doc_file(rel: str):
    h = hashlib.sha256(rel.encode("utf-8")).hexdigest()[:16]
    return _dir() / f"{h}.json"


def mergeable(rel: str, body: str) -> bool:
    from . import secrets, vault
    return (not vault.is_reserved(rel) and not secrets.is_encrypted(body or "")
            and len((body or "").encode("utf-8")) <= MAX_CRDT_BYTES)


def load_doc(rel: str):
    p = _doc_file(rel)
    if p.exists():
        try:
            return crdt.Doc.from_json(p.read_text(encoding="utf-8"), site_id())
        except Exception:
            return None
    return None


def save_doc(rel: str, doc: "crdt.Doc") -> None:
    _doc_file(rel).write_text(doc.to_json(), encoding="utf-8")


def delete_doc(rel: str) -> None:
    p = _doc_file(rel)
    if p.exists():
        p.unlink()


def _reconciled_doc(rel: str, body: str) -> "crdt.Doc":
    doc = load_doc(rel)
    if doc is None:
        doc = crdt.Doc.from_text(body, site_id())
    else:
        doc.site = site_id()
        doc.local_edit(body)
    return doc


def update_from_body(rel: str, body: str) -> None:
    """Reconcile the current note body into its CRDT doc (no-op if unchanged)."""
    if not mergeable(rel, body):
        return
    save_doc(rel, _reconciled_doc(rel, body))


def body_doc_json(rel: str, body: str) -> str:
    """Serialized body CRDT doc, reconciled with the current body."""
    doc = _reconciled_doc(rel, body)
    save_doc(rel, doc)
    return doc.to_json()


def _fm_key(fm: dict) -> tuple:
    """Deterministic total order for choosing the winning frontmatter."""
    return (str(fm.get("updated", "")), json.dumps(fm, sort_keys=True))


def merge_body(rel: str, local_body: str, local_fm: dict,
               peer_body_doc: str, peer_fm: dict) -> tuple[str, dict, bool]:
    """Merge a peer's body doc into ours. Returns (body, frontmatter, clean).

    `clean` is False when the two docs share NO common atom ids and both are
    non-empty — i.e. they were created independently (same filename on two
    devices that never synced), so a character merge would interleave garbage.
    In that case we DON'T interleave: pick the newer version whole and let the
    caller keep the loser as a conflict copy. Otherwise we auto-merge and both
    replicas converge to identical text."""
    doc = _reconciled_doc(rel, local_body)
    other = crdt.Doc.from_json(peer_body_doc, "peer")
    win_fm = peer_fm if _fm_key(peer_fm or {}) > _fm_key(local_fm or {}) else local_fm

    ours = set(doc.atoms) | doc.tombs
    theirs = set(other.atoms) | other.tombs
    if local_body.strip() and other.text().strip() and not (ours & theirs):
        # independent histories — refuse to interleave; last-writer wins, conflict-copy the loser
        if _fm_key(peer_fm or {}) > _fm_key(local_fm or {}):
            other.site = site_id(); save_doc(rel, other)
            return other.text(), (peer_fm or {}), False
        return local_body, (local_fm or {}), False

    doc.merge(other)
    save_doc(rel, doc)
    return doc.text(), (win_fm or {}), True
