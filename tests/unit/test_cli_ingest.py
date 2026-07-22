"""CLI cold-start helpers: seed-demo (first-run sample vault) and ingest
(bulk-import an existing folder of markdown/text)."""
import cli.grimoire as g


def test_seed_demo_writes_sample_vault(vaultdir):
    from server import db
    g.cmd_seed_demo([])
    assert db.one("SELECT COUNT(*) c FROM notes")["c"] == len(g._DEMO)
    # includes a provenance-stamped memory and an extractable fact
    assert db.one("SELECT 1 FROM notes WHERE path='memory/deploy-quirks.md'")
    assert db.one("SELECT value FROM facts WHERE key='port'")["value"] == "8443"


def test_ingest_folder_normalizes_and_indexes(vaultdir, tmp_path):
    from server import db
    src = tmp_path / "src" / "deep"
    src.mkdir(parents=True)
    (src / "Imported Doc.md").write_text("---\ntitle: Kept Title\n---\nabout zeppelins\n")
    (tmp_path / "src" / "loose note.txt").write_text("plain text about zeppelins too\n")
    (tmp_path / "src" / "photo.png").write_bytes(b"\x89PNG binary")
    g.cmd_ingest([str(tmp_path / "src")])
    paths = {r["path"] for r in db.query("SELECT path FROM notes")}
    assert "deep/imported-doc.md" in paths          # slugified, nested path kept
    assert "loose-note.md" in paths                 # .txt normalized to .md
    assert not any(p.endswith(".png") for p in paths)   # binary skipped
    # frontmatter title preserved; content searchable
    assert db.one("SELECT title FROM notes WHERE path='deep/imported-doc.md'")["title"] == "Kept Title"
    assert any(h["title"] == "Kept Title"
               for h in __import__("server.index", fromlist=["x"]).retrieve("zeppelins"))
