"""Negative / adversarial: path safety is security-critical."""
import pytest

from server import vault
from server.vault import VaultError


@pytest.mark.parametrize("bad", [
    "../../etc/passwd", "../outside",
    "notes/../../escape", ".grimoire/index.db", "foo/../.grimoire/secrets",
])
def test_path_traversal_blocked(vaultdir, bad):
    with pytest.raises(VaultError):
        vault.safe_path(bad)


def test_absolute_path_confined_to_vault(vaultdir):
    # a leading-slash path is not an escape — it's confined inside the vault
    p = vault.safe_path("/etc/passwd")
    assert str(vaultdir.resolve()) in str(p) and "/etc/passwd" not in str(p.parent.parent)


def test_safe_paths_allowed(vaultdir):
    assert str(vault.safe_path("note")).endswith("/note.md")
    assert str(vault.safe_path("sub/deep/note.md")).endswith("/sub/deep/note.md")


def test_empty_path_rejected(vaultdir):
    with pytest.raises(VaultError):
        vault.safe_path("")


def test_write_read_roundtrip_and_atomic(vaultdir):
    vault.write("a.md", "# A\n\nbody #tag [[B]]", {"title": "A"})
    n = vault.read("a.md")
    assert n["title"] == "A" and "tag" in n["tags"]
    assert n["links"][0]["target"] == "B"
    assert not list(vaultdir.glob("*.tmp"))   # atomic write leaves no temp files
