#!/usr/bin/env python3
"""Verify that Python accepts what the GO implementation produces.

The fixtures prove Go reproduces Python. This proves the reverse, which matters
just as much for a drop-in replacement: a vault written by the Go build has to
stay readable if the user rolls back, and a Python-written vault has to stay
readable after upgrading. One direction alone lets a one-way door slip through.

Usage, from the repo root:

    cd go && go run ./cmd/xlangcheck | ../.venv/bin/python ../compat/verify_go.py
"""
from __future__ import annotations

import base64
import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))
os.environ.setdefault("GRIMOIRE_VAULT", "/tmp/grimoire-compat-vault")
os.environ.setdefault("GRIMOIRE_NO_WATCHER", "1")


def main() -> int:
    data = json.load(sys.stdin)
    failures: list[str] = []
    checked = 0

    from server import crdt, crypto, vault

    # --- Go-sealed tokens must open in Python, at both KDFs -----------------
    for c in data.get("crypto", []):
        checked += 1
        key = crypto.derive_key(c["passphrase"], base64.b64decode(c["salt_b64"]), c["kdf"])
        try:
            got = crypto.unseal(key, c["token"].encode()).decode("utf-8")
        except Exception as exc:
            failures.append(f"crypto[{c['kdf']}, {c['passphrase']!r}]: {exc}")
            continue
        if got != c["plaintext"]:
            failures.append(f"crypto[{c['passphrase']!r}]: plaintext mismatch")

    # --- Go CRDT documents must be byte-identical to Python's --------------
    for c in data.get("crdt", []):
        checked += 1
        doc = crdt.Doc(site=c["site"])
        for e in c["edits"]:
            doc.local_edit(e)
        if doc.text() != c["text"]:
            failures.append(f"crdt[{c['site']}]: text mismatch")
            continue
        if doc.to_json() != c["json"]:
            failures.append(
                f"crdt[{c['site']}]: serialized bytes differ\n"
                f"    py: {doc.to_json()[:120]}\n    go: {c['json'][:120]}")
        # and Python must be able to load what Go wrote
        try:
            loaded = crdt.Doc.from_json(c["json"], site="reader")
        except Exception as exc:
            failures.append(f"crdt[{c['site']}]: python cannot load go json: {exc}")
            continue
        if loaded.text() != c["text"]:
            failures.append(f"crdt[{c['site']}]: reloaded text mismatch")

    # --- note parsing agrees on titles and content hashes ------------------
    for c in data.get("notes", []):
        checked += 1
        note = vault.note_from_text(c["path"], c["text"], 1700000000.0)
        if note["title"] != c["title"]:
            failures.append(f"notes[{c['path']}]: title {note['title']!r} != {c['title']!r}")
        if note["hash"] != c["hash"]:
            failures.append(f"notes[{c['path']}]: hash {note['hash']} != {c['hash']}")

    for f in failures:
        print(f"FAIL {f}")
    if failures:
        print(f"\n{len(failures)} of {checked} cross-language checks FAILED")
        return 1
    print(f"PASS: python accepts all {checked} artifacts produced by go")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
