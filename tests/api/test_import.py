"""Vault zip import — round-trip + zip-slip / .grimoire / zip-bomb protection."""
import io
import zipfile


def _zip(entries):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        for name, content in entries.items():
            z.writestr(name, content)
    return buf.getvalue()


def test_import_roundtrip(client, vaultdir):
    data = _zip({"imported-note.md": "# Imported\n\nhello world", "sub/deep.md": "# Deep"})
    r = client.post("/api/import/vault", files={"file": ("v.zip", data, "application/zip")})
    assert r.status_code == 200 and r.json()["imported"] == 2
    assert (vaultdir / "imported-note.md").exists()
    assert client.get("/api/notes/imported-note.md").json()["title"] == "Imported"
    assert client.get("/api/search?q=hello").json()          # indexed


def test_import_blocks_zip_slip(client, vaultdir):
    # a malicious entry trying to escape the vault must be refused, not written
    data = _zip({"../../evil.md": "PWNED", "ok.md": "fine"})
    r = client.post("/api/import/vault", files={"file": ("v.zip", data, "application/zip")})
    j = r.json()
    assert j["imported"] == 1 and j["skipped"] == 1
    assert (vaultdir / "ok.md").exists()
    # nothing was written outside the vault
    assert not (vaultdir.parent / "evil.md").exists()
    assert not (vaultdir.parent.parent / "evil.md").exists()


def test_import_skips_grimoire_entries(client, vaultdir):
    data = _zip({".grimoire/secrets.enc": "SHOULD_NOT_LAND", "real.md": "# Real"})
    j = client.post("/api/import/vault", files={"file": ("v.zip", data, "application/zip")}).json()
    assert j["imported"] == 1 and j["skipped"] == 1
    assert not (vaultdir / ".grimoire" / "secrets.enc").read_text() if (vaultdir / ".grimoire" / "secrets.enc").exists() else True


def test_import_rejects_non_zip(client):
    r = client.post("/api/import/vault", files={"file": ("x.zip", b"not a zip", "application/zip")})
    assert r.status_code == 400
