"""The vault watcher: external .md edits (Obsidian/vim/sync) reconcile the index."""
import time

from server import db, index
from server.watcher import VaultWatcher


def _wait(cond, timeout=6.0):
    end = time.time() + timeout
    while time.time() < end:
        if cond():
            return True
        time.sleep(0.1)
    return False


def test_external_create_is_indexed(vaultdir):
    w = VaultWatcher(debounce=0.2)
    w.start()
    time.sleep(0.3)   # let the observer establish its watch before writing
    try:
        # write a note the way an external editor / sync client would
        (vaultdir / "external.md").write_text(
            "---\ntitle: External Note\n---\nwritten outside mnemo [[Target]] #ext")
        assert _wait(lambda: db.one("SELECT 1 FROM notes WHERE path='external.md'")), \
            "watcher did not index an externally-created note"
        n = db.one("SELECT title FROM notes WHERE path='external.md'")
        assert n["title"] == "External Note"
        assert db.query("SELECT 1 FROM tags WHERE note='external.md' AND tag='ext'")
    finally:
        w.stop()


def test_external_edit_updates_index(vaultdir):
    (vaultdir / "edit.md").write_text("# Edit\n\noriginal content")
    index.reindex()
    w = VaultWatcher(debounce=0.2)
    w.start()
    try:
        (vaultdir / "edit.md").write_text("# Edit\n\nUPDATED externally with #newtag")
        assert _wait(lambda: db.query(
            "SELECT 1 FROM tags WHERE note='edit.md' AND tag='newtag'")), \
            "watcher did not pick up an external edit"
    finally:
        w.stop()


def test_external_delete_removes_from_index(vaultdir):
    p = vaultdir / "doomed.md"
    p.write_text("# Doomed")
    index.reindex()
    assert db.one("SELECT 1 FROM notes WHERE path='doomed.md'")
    w = VaultWatcher(debounce=0.2)
    w.start()
    try:
        p.unlink()
        assert _wait(lambda: not db.one("SELECT 1 FROM notes WHERE path='doomed.md'")), \
            "watcher did not remove a deleted note from the index"
    finally:
        w.stop()
