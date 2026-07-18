"""BYO-vault frontmatter fidelity: editing a note through Grimoire must never
degrade frontmatter written by other markdown tools (nested maps, multiline
strings, object lists). Managed flat keys are patched; foreign structure passes
through byte-for-byte."""
from server import vault

RICH = """---
title: Imported Note
tags: [alpha, beta]
nested:
  level: deep
  items:
    - one
    - two
authors:
  - name: Ada
    role: writer
description: |
  A multiline
  description block.
custom_flag: true
---

original body
"""


def _write_raw(vaultdir, rel, text):
    (vaultdir / rel).write_text(text, encoding="utf-8")


def test_body_edit_preserves_nested_frontmatter(vaultdir):
    _write_raw(vaultdir, "rich.md", RICH)
    note = vault.read("rich.md")
    vault.write("rich.md", "edited body", note["frontmatter"])
    text = (vaultdir / "rich.md").read_text()
    # foreign structure survives verbatim
    assert "nested:\n  level: deep" in text
    assert "  items:\n    - one\n    - two" in text
    assert "- name: Ada\n    role: writer" in text
    assert "description: |\n  A multiline\n  description block." in text
    assert "edited body" in text


def test_managed_keys_still_update(vaultdir):
    _write_raw(vaultdir, "rich.md", RICH)
    note = vault.read("rich.md")
    fm = dict(note["frontmatter"])
    fm["title"] = "Renamed"
    fm["tags"] = ["gamma"]
    fm["pinned"] = True
    vault.write("rich.md", note["body"], fm)
    text = (vaultdir / "rich.md").read_text()
    assert "title: Renamed" in text
    assert "tags: [gamma]" in text
    assert "pinned: true" in text
    assert "updated:" in text                       # stamp applied
    assert "nested:\n  level: deep" in text        # structure untouched


def test_degraded_client_value_cannot_clobber_nested_key(vaultdir):
    """The UI's flattened copy of a nested key must not overwrite the real
    structure (our yamlish parser can only see a degraded version of it)."""
    _write_raw(vaultdir, "rich.md", RICH)
    note = vault.read("rich.md")
    fm = dict(note["frontmatter"])
    fm["nested"] = ""          # what a flattening round-trip would send back
    fm["authors"] = ""
    vault.write("rich.md", note["body"], fm)
    text = (vaultdir / "rich.md").read_text()
    assert "nested:\n  level: deep" in text
    assert "- name: Ada" in text
    assert "nested: \n" not in text                # degraded form NOT written


def test_deleting_a_flat_key_removes_it(vaultdir):
    _write_raw(vaultdir, "rich.md", RICH)
    note = vault.read("rich.md")
    fm = {k: v for k, v in note["frontmatter"].items() if k != "custom_flag"}
    vault.write("rich.md", note["body"], fm)
    text = (vaultdir / "rich.md").read_text()
    assert "custom_flag" not in text
    assert "nested:\n  level: deep" in text


def test_double_round_trip_is_stable(vaultdir):
    """Two consecutive body-only edits must converge — no churn, no drift."""
    _write_raw(vaultdir, "rich.md", RICH)
    n1 = vault.read("rich.md")
    vault.write("rich.md", "body v2", n1["frontmatter"])
    first = (vaultdir / "rich.md").read_text()
    n2 = vault.read("rich.md")
    vault.write("rich.md", "body v2", n2["frontmatter"])
    second = (vaultdir / "rich.md").read_text()
    # identical except the updated stamp
    def strip(t):
        return "\n".join(ln for ln in t.split("\n") if not ln.startswith("updated:"))
    assert strip(first) == strip(second)


def test_plain_grimoire_notes_unaffected(vaultdir):
    """Notes created by Grimoire itself keep the existing canonical format."""
    vault.write("plain.md", "hello", {"title": "Plain", "tags": ["x"]})
    n = vault.read("plain.md")
    vault.write("plain.md", "hello again", n["frontmatter"])
    text = (vaultdir / "plain.md").read_text()
    assert "title: Plain" in text and "tags: [x]" in text
    assert "hello again" in text
