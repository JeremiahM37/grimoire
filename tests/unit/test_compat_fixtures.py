"""The cross-language contract: Python must still reproduce compat/fixtures/.

These fixtures are what the Go port is written against. Two jobs here:

1. **Self-consistency** — prove the fixtures are reproducible and not accidental
   snapshots of a particular machine/run. A fixture that Python itself can't
   regenerate is worthless as a target for Go.
2. **Drift guard** — if someone changes rendering, parsing, crypto or the CRDT
   format in Python, this suite fails loudly instead of letting the Go port
   quietly encode last month's behaviour.

When a change to Python behaviour is intentional, regenerate:
    .venv/bin/python compat/generate.py
and review the fixture diff as part of the change — that diff IS the list of
things the Go port must be updated to match.
"""
from __future__ import annotations

import base64
import json
from pathlib import Path

import pytest

FIXTURES = Path(__file__).resolve().parents[2] / "compat" / "fixtures"


def load(name: str) -> dict:
    p = FIXTURES / name
    if not p.exists():
        pytest.skip(f"{name} not generated (run compat/generate.py)")
    return json.loads(p.read_text())


# --------------------------------------------------------------------- crypto

def test_crypto_key_derivation_matches_fixture():
    from server import crypto
    fx = load("crypto.json")
    assert fx["params"]["argon2id"]["memory_kib"] == crypto.ARGON_MEMORY_KIB
    assert fx["params"]["pbkdf2"]["iterations"] == crypto.ITERATIONS
    for c in fx["cases"]:
        key = crypto.derive_key(c["passphrase"], base64.b64decode(c["salt_b64"]), c["kdf"])
        assert key.decode("ascii") == c["derived_key"], f"{c['kdf']} drifted"


def test_crypto_sealed_tokens_still_open():
    """Tokens frozen in the fixture must keep decrypting — this is exactly what
    an existing user's vault looks like after an upgrade."""
    from server import crypto
    for c in load("crypto.json")["seal_cases"]:
        key = crypto.derive_key(c["passphrase"], base64.b64decode(c["salt_b64"]), c["kdf"])
        assert crypto.unseal(key, c["token"].encode("ascii")).decode("utf-8") == c["plaintext"]


# ---------------------------------------------------------------- note parsing

def test_note_parsing_matches_fixture():
    from server import index, vault
    fx = load("notes.json")
    for c in fx["cases"]:
        note = vault.note_from_text(c["path"], c["text"], mtime=fx["mtime"])
        assert note["title"] == c["title"], c["path"]
        assert note["body"] == c["body"], c["path"]
        assert note["tags"] == c["tags"], c["path"]
        assert note["links"] == c["links"], c["path"]
        assert bool(note["private"]) == c["private"], c["path"]
        assert note["frontmatter"] == c["frontmatter"], c["path"]
        assert note["hash"] == c["hash"], c["path"]
        assert [list(f) for f in index.extract_facts(note["body"])] == c["facts"], c["path"]


# ------------------------------------------------------------- frontmatter I/O

def test_frontmatter_patching_matches_fixture():
    from server import vault
    fx = load("frontmatter.json")
    for c in fx["cases"]:
        assert vault._patch_frontmatter(c["raw_inner"], c["frontmatter"]) == c["patched"], c["name"]
    for c in fx["serialize_cases"]:
        assert vault._serialize(c["frontmatter"], c["body"]) == c["serialized"]


# ------------------------------------------------------------------- rendering

def test_render_output_matches_fixture():
    from server import render
    fx = load("render.json")
    for c in fx["cases"]:
        got = render.render(c["markdown"], link_map=dict(c["link_map"]))
        assert got == c["html"], f"render drifted for case {c['name']!r}"
    for c in fx["heading_id_cases"]:
        assert render.heading_id(c["text"]) == c["id"]


# ------------------------------------------------------------------------ crdt

def test_crdt_format_matches_fixture():
    from server import crdt
    fx = load("crdt.json")
    assert crdt.BASE == fx["base"]
    for c in fx["cases"]:
        doc = crdt.Doc(site=c["site"])
        for t in c["edits"]:
            doc.local_edit(t)
        assert doc.text() == c["final_text"], c["name"]
        assert json.loads(doc.to_json()) == c["json"], f"crdt json format drifted: {c['name']}"
    for c in fx["key_between_cases"]:
        got = crdt.key_between(tuple(c["left"]), tuple(c["right"]))
        assert list(got) == c["between"]


def test_crdt_fixture_docs_are_loadable_and_converge():
    """The stored JSON must load back into a working doc — that's the on-disk
    compatibility promise, not just a serialization detail."""
    from server import crdt
    for c in load("crdt.json")["merge_cases"]:
        a = crdt.Doc.from_json(json.dumps(c["a_json"]), site="alpha")
        b = crdt.Doc.from_json(json.dumps(c["b_json"]), site="beta")
        a.merge(b)
        assert a.text() == c["merged_text"], c["name"]


# ------------------------------------------------------------------ embeddings

def test_embeddings_match_fixture():
    from server import ai
    fx = load("embed.json")
    tol = fx["tolerance"]
    sig = ai.embed_signature()
    if sig not in fx["backends"]:
        pytest.skip(f"active backend {sig} not in fixture (generated on another host)")
    for c in fx["backends"][sig]:
        got = ai.embed([c["text"]])[0]
        assert len(got) == len(c["vector"]), f"dimension changed for {sig}"
        worst = max((abs(float(g) - e) for g, e in zip(got, c["vector"], strict=True)),
                    default=0.0)
        assert worst <= tol, f"{sig} vectors drifted by {worst:g} (tol {tol:g})"


def test_chunking_matches_fixture():
    from server import ai
    for c in load("embed.json")["chunk_cases"]:
        assert ai.chunk_text(c["text"]) == c["chunks"]


# ------------------------------------------------------------------ path safety

def test_path_confinement_matches_fixture():
    from server import vault
    for c in load("paths.json")["cases"]:
        try:
            p = vault.safe_path(c["rel"])
            ok, resolved = True, vault.rel_of(p)
        except Exception as exc:
            ok, resolved = False, type(exc).__name__
        assert ok == c["ok"], f"confinement changed for {c['rel']!r}"
        if ok:
            assert resolved == c["resolved_rel"], c["rel"]
        else:
            assert resolved == c["error"], c["rel"]
        assert vault.is_reserved(c["rel"]) == c["is_reserved"], c["rel"]


def test_slugify_matches_fixture():
    from server import vault
    for c in load("paths.json")["slugify_cases"]:
        assert vault.slugify(c["title"]) == c["slug"]
