"""Real bidirectional sync between two mnemo instances over HTTP."""
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
    """A second mnemo server (the sync peer) with its own vault."""
    port = _free_port()
    vault = tmp_path / "peer-vault"
    vault.mkdir()
    env = {"MNEMO_VAULT": str(vault), "MNEMO_PORT": str(port),
           "MNEMO_NO_WATCHER": "1", "PATH": __import__("os").environ["PATH"]}
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
