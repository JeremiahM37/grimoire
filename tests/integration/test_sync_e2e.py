"""Real bidirectional sync between two grimoire instances over HTTP."""
import socket
import subprocess
import sys
import time
import urllib.request
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def _free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _get(url):
    with urllib.request.urlopen(url, timeout=5) as r:
        import json
        return json.load(r)


@pytest.fixture()
def peer(tmp_path):
    """A second grimoire server (the sync peer) with its own vault."""
    port = _free_port()
    vault = tmp_path / "peer-vault"
    vault.mkdir()
    env = {"GRIMOIRE_VAULT": str(vault), "GRIMOIRE_PORT": str(port),
           "GRIMOIRE_NO_WATCHER": "1", "PATH": __import__("os").environ["PATH"]}
    proc = subprocess.Popen([sys.executable, "-m", "server"], cwd=ROOT, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = f"http://127.0.0.1:{port}"
    for _ in range(100):
        try:
            _get(base + "/api/health"); break
        except Exception:
            time.sleep(0.1)
    else:
        proc.kill(); raise RuntimeError("peer did not start")
    yield base, vault
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def _post(base, path, body):
    import json
    req = urllib.request.Request(base + path, method="POST",
                                 data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=5) as r:
        return json.load(r)


def _peer_body(base, rel):
    return _get(base + "/api/notes/" + rel)["body"]


def test_crdt_concurrent_edits_auto_merge(vaultdir, peer):
    """Both replicas edit the SAME note concurrently; sync converges them to the
    same text with BOTH edits — no conflict copy."""
    from server import index, syncclient, vault
    base, peer_vault = peer

    # create a note locally and sync it to the peer (peer adopts our CRDT ids)
    vault.write("collab.md", "line one\nline two\nline three", {"title": "Collab"})
    index.reindex()
    syncclient.sync_with_peer(base, "A")
    assert (peer_vault / "collab.md").exists()

    # concurrent edits: local prepends, peer appends (disjoint positions)
    ours = vault.read("collab.md")
    vault.write("collab.md", "ZERO\n" + ours["body"], ours["frontmatter"])
    index.upsert("collab.md")
    # peer edit via its notes API (a PUT)
    import json
    req = urllib.request.Request(base + "/api/notes/collab.md", method="PUT",
                                 data=json.dumps({"body": "line one\nline two\nline three\nFOUR"}).encode(),
                                 headers={"Content-Type": "application/json"})
    urllib.request.urlopen(req, timeout=5).read()

    # sync twice (one round each way settles the bidirectional merge)
    syncclient.sync_with_peer(base, "A")
    syncclient.sync_with_peer(base, "A")

    local_body = vault.read("collab.md")["body"]
    peer_body = _peer_body(base, "collab.md")
    assert local_body == peer_body, f"diverged:\nLOCAL:\n{local_body}\nPEER:\n{peer_body}"
    assert "ZERO" in local_body and "FOUR" in local_body   # both edits survived


def test_bidirectional_sync_between_two_instances(vaultdir, peer):
    from server import index, syncclient, vault
    base, peer_vault = peer
    # peer has a note we don't
    _post(base, "/api/notes", {"title": "From Peer", "body": "peer content xyz"})
    # we have a note the peer doesn't
    vault.write("from-local.md", "# From Local\n\nlocal content abc")
    index.reindex()

    stats = syncclient.sync_with_peer(base, "test-client")

    # the peer's note came down to us...
    assert (vaultdir / "from-peer.md").exists()
    assert "peer content xyz" in (vaultdir / "from-peer.md").read_text()
    # ...and our note went up to the peer
    assert (peer_vault / "from-local.md").exists()
    assert "local content abc" in (peer_vault / "from-local.md").read_text()
    assert stats["pulled"] >= 1 and stats["pushed"] >= 1
