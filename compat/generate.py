#!/usr/bin/env python3
"""Generate the cross-language compatibility fixtures.

These are the contract the Go port is written against: frozen output from the
Python implementation that already works, in a language-neutral form (JSON,
base64 for bytes). A Go implementation that reproduces every fixture is a
drop-in replacement for the surfaces the fixtures cover; one that doesn't is
telling you exactly where it diverges, in a unit-test-sized message rather than
as a corrupted vault six weeks later.

Run from the repo root:  .venv/bin/python compat/generate.py

Regenerating is safe and expected — the files are deterministic EXCEPT the
sealed-token cases (Fernet embeds a random IV and a timestamp). Those exist so
Go can prove it decrypts Python's output; the reverse direction (Python decrypting Go's
output) gets proven once there is Go code to produce any.

tests/unit/test_compat_fixtures.py replays every fixture against the current
Python implementation, so these files cannot silently go stale.
"""
from __future__ import annotations

import base64
import hashlib
import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

FIXTURES = Path(__file__).resolve().parent / "fixtures"

# A fixed vault so nothing depends on the developer's machine.
os.environ.setdefault("GRIMOIRE_VAULT", "/tmp/grimoire-compat-vault")
os.environ.setdefault("GRIMOIRE_NO_WATCHER", "1")


def b64(data: bytes) -> str:
    return base64.b64encode(data).decode("ascii")


def write(name: str, payload: dict) -> None:
    FIXTURES.mkdir(parents=True, exist_ok=True)
    p = FIXTURES / name
    p.write_text(json.dumps(payload, indent=2, ensure_ascii=False, sort_keys=False) + "\n")
    n = sum(len(v) for v in payload.values() if isinstance(v, list))
    n += sum(len(v) for b in payload.values() if isinstance(b, dict)
             for v in b.values() if isinstance(v, list))
    print(f"  {name:28s} {n:4d} cases")


# --------------------------------------------------------------------- crypto

def gen_crypto() -> None:
    """Key derivation is fully deterministic — pin exact bytes for both KDFs.

    Sealed tokens are not (random IV + timestamp), so we pin tokens Python
    produced and require Go to decrypt them.
    """
    from server import crypto

    kdf_cases = []
    for passphrase, salt_hex, kdf in [
        ("correct horse battery staple", "00112233445566778899aabbccddeeff", "argon2id"),
        ("correct horse battery staple", "00112233445566778899aabbccddeeff", "pbkdf2"),
        ("", "ffffffffffffffffffffffffffffffff", "argon2id"),
        ("ünïcode-påss-🔐", "0f0e0d0c0b0a09080706050403020100", "argon2id"),
        ("ünïcode-påss-🔐", "0f0e0d0c0b0a09080706050403020100", "pbkdf2"),
        ("x" * 200, "1234567890abcdef1234567890abcdef", "argon2id"),
    ]:
        salt = bytes.fromhex(salt_hex)
        key = crypto.derive_key(passphrase, salt, kdf)
        kdf_cases.append({
            "passphrase": passphrase,
            "salt_b64": b64(salt),
            "kdf": kdf,
            # the urlsafe-b64 Fernet key exactly as derive_key returns it
            "derived_key": key.decode("ascii"),
        })

    seal_cases = []
    for passphrase, plaintext in [
        ("correct horse battery staple", "a secret value"),
        ("correct horse battery staple", ""),
        ("ünïcode-påss-🔐", "ünïcode plaintext — with emoji 🔐 and\nnewlines\n"),
        ("correct horse battery staple", "x" * 5000),
    ]:
        salt = hashlib.sha256(passphrase.encode() + plaintext.encode()).digest()[:16]
        key = crypto.derive_key(passphrase, salt, "argon2id")
        token = crypto.seal(key, plaintext.encode("utf-8"))
        seal_cases.append({
            "passphrase": passphrase,
            "salt_b64": b64(salt),
            "kdf": "argon2id",
            "plaintext": plaintext,
            "token": token.decode("ascii"),
        })

    write("crypto.json", {
        "description": (
            "derive_key must produce these bytes exactly; the sealed tokens were "
            "produced by Python's Fernet and must decrypt to the given plaintext."
        ),
        "params": {
            "argon2id": {"time": crypto.ARGON_TIME, "memory_kib": crypto.ARGON_MEMORY_KIB,
                         "parallelism": crypto.ARGON_PARALLELISM, "hash_len": 32},
            "pbkdf2": {"iterations": crypto.ITERATIONS, "hash": "sha256", "length": 32},
            "default_kdf": crypto.DEFAULT_KDF,
        },
        "cases": kdf_cases,
        "seal_cases": seal_cases,
    })


# ---------------------------------------------------------------- note parsing

NOTE_SOURCES = [
    ("plain.md", "# Plain Note\n\nJust a body.\n"),
    ("frontmatter.md",
     "---\ntitle: From Frontmatter\ntags: [alpha, beta]\ncreated: 2026-01-02T03:04:05\n---\n"
     "Body under frontmatter with a #inline-tag.\n"),
    ("links.md",
     "# Links\n\nSee [[Other Note]], [[Folder/Deep Note]], [[Target|an alias]] and "
     "[[note#Some Heading]].\n"),
    ("facts.md",
     "# Facts\n\nport:: 8443\nowner:: platform team\n\n- role:: primary\n\n"
     "```\nnot-a-fact:: in a fence\n```\n"),
    ("tags.md", "---\ntags:\n  - from-fm\n---\n# Tags\n\n#body-tag and #nested/tag here.\n"),
    ("unicode.md", "# Ünïcodé — 日本語\n\nBody with emoji 🔐 and a [[Ünïcodé]] link.\n"),
    ("empty.md", ""),
    ("no-heading.md", "just text, no heading at all\n"),
    ("private.md", "---\nprivate: true\n---\n# Private\n\nsecret-ish body\n"),
]


def gen_notes() -> None:
    """note_from_text drives title/tags/links/facts/private extraction — every
    downstream surface (index, search, render) depends on it agreeing."""
    from server import index, vault

    cases = []
    for rel, text in NOTE_SOURCES:
        note = vault.note_from_text(rel, text, mtime=1700000000.0)
        cases.append({
            "path": rel,
            "text": text,
            "title": note["title"],
            "body": note["body"],
            "tags": note["tags"],
            "links": note["links"],
            "private": bool(note["private"]),
            "encrypted": bool(note.get("encrypted")),
            "frontmatter": note["frontmatter"],
            "hash": note["hash"],
            "facts": [list(f) for f in index.extract_facts(note["body"])],
        })

    write("notes.json", {
        "description": "note_from_text() + extract_facts(): parsing markdown into "
                       "the fields the whole index is built from. mtime is fixed at "
                       "1700000000.0 so `hash` is reproducible.",
        "mtime": 1700000000.0,
        "cases": cases,
    })


# ------------------------------------------------------------- frontmatter I/O

def gen_frontmatter() -> None:
    """The BYO-vault guarantee: writing a note PATCHES the existing frontmatter
    block — managed keys replaced, nested/multiline/foreign YAML preserved
    byte-for-byte. This is the fixture that stops the Go port silently
    degrading someone's Obsidian vault."""
    from server import vault

    cases = []
    raw_blocks = [
        ("simple", "title: Old Title\ntags: [a, b]\n", {"title": "New Title"}),
        ("nested-preserved",
         "title: Old\nobsidian:\n  cssclass: wide\n  nested:\n    deep: true\n"
         "aliases:\n  - one\n  - two\n",
         {"title": "New", "updated": "2026-08-13T00:00:00"}),
        ("foreign-multiline",
         "title: Old\ndescription: |\n  a multiline\n  block scalar\ncustom_key: kept\n",
         {"title": "New"}),
        ("adds-missing-key", "title: Only Title\n", {"private": True}),
        ("empty-block", "", {"title": "Added"}),
        ("list-value-replaced", "tags:\n  - old1\n  - old2\n", {"tags": ["new1", "new2"]}),
    ]
    for name, raw_inner, fm in raw_blocks:
        cases.append({
            "name": name,
            "raw_inner": raw_inner,
            "frontmatter": fm,
            "patched": vault._patch_frontmatter(raw_inner, fm),
        })

    serialize_cases = []
    for fm, body in [
        ({"title": "T"}, "body text"),
        ({}, "no frontmatter body"),
        ({"title": "T", "tags": ["x", "y"], "private": True}, "with tags"),
        ({"title": "Trailing"}, "body without trailing newline"),
    ]:
        serialize_cases.append({
            "frontmatter": fm, "body": body, "serialized": vault._serialize(fm, body),
        })

    write("frontmatter.json", {
        "description": "_patch_frontmatter() must preserve nested/multiline/foreign "
                       "YAML byte-for-byte while replacing managed flat keys; "
                       "_serialize() guarantees a trailing newline.",
        "cases": cases,
        "serialize_cases": serialize_cases,
    })


# ------------------------------------------------------------------- rendering

RENDER_SOURCES = [
    ("headings", "# H1\n\n## H2 with *emphasis*\n\n### H3 — dashes & ünïcode\n"),
    ("inline-code-is-literal",
     "use `[[wikilinks]]` and `#tags` and `**bold**` and `[x](https://e.com)` inline\n"),
    ("wikilinks",
     "see [[Known Note]] and [[Folder/Deep Note]] and [[Missing Note]] and "
     "[[Known Note|aliased]] and [[Known Note#A Heading]]\n"),
    ("emphasis", "**bold** and *italic* and ~~strike~~ and ==mark== mixed **in *one* line**\n"),
    ("lists", "- one\n- two\n  - nested\n\n1. first\n2. second\n"),
    ("tasks", "- [ ] todo item\n- [x] done item\n"),
    ("table", "| a | b |\n|---|---|\n| 1 | 2 |\n| [[Known Note]] | x |\n"),
    ("fenced-code", "```python\ndef f(x):\n    return [[not_a_link]]\n```\n"),
    ("fenced-no-lang", "```\nplain <script>alert(1)</script> block\n```\n"),
    ("blockquote", "> quoted **text** with [[Known Note]]\n"),
    ("hr", "text\n\n---\n\nmore\n"),
    ("footnotes", "a claim[^1] and another[^note]\n\n[^1]: first footnote\n[^note]: second\n"),
    ("html-escaping", "<script>alert('xss')</script> & <b>raw</b> \"quotes\"\n"),
    ("tags", "a #tag and #nested/tag and not#a-tag here\n"),
    ("links", "[text](https://example.com) and bare https://example.com\n"),
    ("image-embed", "![[picture.png]] inline and on its own line:\n\n![[diagram.jpg]]\n"),
    ("unicode", "Ünïcodé — 日本語 — emoji 🔐 in **bold**\n"),
    ("empty", ""),
]

RENDER_LINK_MAP = {
    "known note": "Known Note.md",
    "folder/deep note": "Folder/Deep Note.md",
    "a heading": "Known Note.md",
}


def gen_render() -> None:
    """The renderer's HTML is contract: the console's client-side renderer and
    the vision-checked /read pages both depend on this exact output."""
    from server import render

    cases = []
    for name, src in RENDER_SOURCES:
        cases.append({
            "name": name,
            "markdown": src,
            "link_map": RENDER_LINK_MAP,
            "html": render.render(src, link_map=dict(RENDER_LINK_MAP)),
        })

    heading_cases = [{"text": t, "id": render.heading_id(t)} for t in [
        "Simple Heading", "With — Dashes & Symbols!", "  leading and trailing  ",
        "Ünïcodé 日本語", "duplicate", "123 numeric start", "",
    ]]

    write("render.json", {
        "description": "render() output is byte-contract: the client renderer and "
                       "the vision-checked /read pages both depend on it.",
        "cases": cases,
        "heading_id_cases": heading_cases,
    })


# ------------------------------------------------------------------------ crdt

def gen_crdt() -> None:
    """On-disk CRDT JSON must stay readable across the port, or existing
    .grimoire/crdt state and any paired peer break."""
    from server import crdt

    cases = []
    edit_sequences = [
        ("append", "local", ["", "a", "ab", "abc"]),
        ("prepend", "local", ["abc", "zabc"]),
        ("middle-insert", "site-1", ["hello world", "hello brave world"]),
        ("delete", "site-1", ["hello world", "hello"]),
        ("replace-all", "s2", ["first text", "totally different"]),
        ("unicode", "s3", ["", "Ünïcodé 🔐", "Ünïcodé 🔐 日本語"]),
        ("empty-out", "s4", ["something", ""]),
    ]
    for name, site, texts in edit_sequences:
        doc = crdt.Doc(site=site)
        for t in texts:
            doc.local_edit(t)
        cases.append({
            "name": name, "site": site, "edits": texts,
            "final_text": doc.text(),
            "json": json.loads(doc.to_json()),
        })

    merge_cases = []
    a = crdt.Doc(site="alpha")
    a.local_edit("shared base")
    b = crdt.Doc.from_json(a.to_json(), site="beta")
    a.local_edit("shared base + alpha")
    b.local_edit("beta + shared base")
    merged = crdt.Doc.from_json(a.to_json(), site="alpha")
    merged.merge(crdt.Doc.from_json(b.to_json(), site="beta"))
    merge_cases.append({
        "name": "concurrent-edits-converge",
        "a_json": json.loads(a.to_json()),
        "b_json": json.loads(b.to_json()),
        "merged_text": merged.text(),
        "merged_json": json.loads(merged.to_json()),
    })

    key_cases = []
    for lo, hi in [((), (crdt.BASE,)), ((1,), (3,)), ((1,), (2,)), ((1, 5), (1, 6))]:
        key_cases.append({
            "left": list(lo), "right": list(hi),
            "between": list(crdt.key_between(tuple(lo), tuple(hi))),
        })

    write("crdt.json", {
        "description": "CRDT doc JSON is an on-disk format — the Go port must read "
                       "and write it unchanged. key_between() drives atom ordering.",
        "base": crdt.BASE,
        "cases": cases,
        "merge_cases": merge_cases,
        "key_between_cases": key_cases,
    })


# ------------------------------------------------------------------ embeddings

# Texts chosen to exercise the tokenizer's awkward corners: casing, accent
# stripping, CJK padding, punctuation splitting, subword continuation, and a
# word long enough to trip the max-input-chars rule.
TOKENIZER_TEXTS = [
    "The Quick Brown Fox",
    "café naïve résumé",
    "日本語のテキスト",
    "hello, world! (parens) [brackets] {braces}",
    "unbelievablyunsegmentableword",
    "a" * 150,
    "snake_case camelCase kebab-case",
    "url https://example.com/path?q=1",
    "emoji 🔐 mixed with text",
    "   leading and trailing   ",
    "tabs\tand\nnewlines",
]

EMBED_TEXTS = [
    "the api gateway listens on port 8443",
    "the deploy service is owned by the platform team",
    "Ünïcodé — 日本語 — emoji 🔐",
    "a" * 500,
    "",
    "Mixed CASE and Punctuation!! ... with numbers 12345",
]


def gen_embed() -> None:
    """The highest-risk surface. If Go's vectors drift, retrieval quality moves
    and the published LoCoMo/LongMemEval numbers stop describing the binary."""
    from server import ai

    backends = {}

    # hash:v1 — the dependency-free fallback, always available
    orig_local = ai._local_enabled
    ai._local_enabled = lambda: False
    try:
        if ai.embed_signature() == "hash:v1":
            backends["hash:v1"] = [
                {"text": t, "vector": [round(float(v), 8) for v in ai.embed([t])[0]]}
                for t in EMBED_TEXTS
            ]
    finally:
        ai._local_enabled = orig_local

    # model2vec — the production backend on this host
    sig = ai.embed_signature()
    if sig.startswith("model2vec:"):
        backends[sig] = [
            {"text": t, "vector": [round(float(v), 8) for v in ai.embed([t])[0]]}
            for t in EMBED_TEXTS
        ]

    # Token ids, so a tokenizer bug is debuggable on its own rather than
    # surfacing only as "the vectors are different".
    tokens = {}
    try:
        from model2vec import StaticModel
        sm = StaticModel.from_pretrained("minishlab/potion-base-8M")
        tokens["minishlab/potion-base-8M"] = [
            {"text": t, "ids": [int(i) for i in sm.tokenize([t], max_length=512)[0]]}
            for t in EMBED_TEXTS + TOKENIZER_TEXTS
        ]
    except Exception as exc:   # noqa: BLE001 — optional dependency
        print(f"    (tokenizer fixtures skipped: {exc})")

    # NOTE the non-ASCII cases: chunk sizes are counted in CHARACTERS, and a
    # byte-based implementation packs fewer of them per chunk — a divergence
    # that only shows up on notes containing emoji or accents.
    chunk_cases = [
        {"text": t, "chunks": ai.chunk_text(t)}
        for t in [
            "short text",
            "\n\n".join(f"paragraph {i} with enough words to matter here" for i in range(20)),
            "# Heading\n\nbody\n\n## Another\n\nmore body\n",
            "",
            # emoji + CJK + accents, sized to straddle the 800-char target
            "\n\n".join(f"🐛 paragraph {i} — ünïcodé 日本語 with enough words to matter here"
                        for i in range(20)),
            ("🗂️ " + "a very long single paragraph with no blank lines " * 40),
            ("日本語のテキスト。" * 200),
        ]
    ]

    write("embed.json", {
        "description": "Vectors per backend signature. Go must match within 1e-5 "
                       "per component, or retrieval quality and the published "
                       "benchmark numbers no longer describe the shipped binary.",
        "tolerance": 1e-5,
        "backends": backends,
        "tokenizer": tokens,
        "chunk_cases": chunk_cases,
    })


# ------------------------------------------------------------------ path safety

def gen_paths() -> None:
    """Path confinement is a security boundary — zip-slip and traversal must be
    rejected identically, not merely 'somehow'."""
    from server import vault

    cases = []
    for rel in [
        "ok.md", "Folder/ok.md", "Folder/Sub/ok.md", "with space.md", "Ünïcodé.md",
        "../escape.md", "/absolute.md", "Folder/../../escape.md", "..\\windows.md",
        "", ".", "..", "./ok.md", "Folder//double.md", ".grimoire/internal.md",
        "templates/t.md", "a/../b.md",
    ]:
        entry = {"rel": rel}
        try:
            p = vault.safe_path(rel)
            entry["ok"] = True
            entry["resolved_rel"] = vault.rel_of(p)
        except Exception as exc:
            entry["ok"] = False
            entry["error"] = type(exc).__name__
        entry["is_reserved"] = vault.is_reserved(rel)
        cases.append(entry)

    slug_cases = [{"title": t, "slug": vault.slugify(t)} for t in [
        "A Simple Title", "With — Dashes & Symbols!", "Ünïcodé 日本語",
        "  spaces  everywhere  ", "ALLCAPS", "123", "", "a/b/c",
    ]]

    write("paths.json", {
        "description": "safe_path() confinement (traversal + absolute + reserved "
                       "dirs) and slugify(). A security boundary, not a nicety.",
        "cases": cases,
        "slugify_cases": slug_cases,
    })


def main() -> None:
    print("generating compat fixtures from the Python implementation:")
    gen_crypto()
    gen_notes()
    gen_frontmatter()
    gen_render()
    gen_crdt()
    gen_embed()
    gen_paths()
    print(f"\nwrote {len(list(FIXTURES.glob('*.json')))} fixture files to {FIXTURES}")


if __name__ == "__main__":
    main()
