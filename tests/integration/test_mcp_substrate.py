"""The MCP substrate tools against a REAL server: remember/recall round-trip
with provenance, and use_credential brokering a call whose secret value the
caller never sees."""
import importlib
import json
import os
import socket
import subprocess
import sys
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def _free_port():
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture()
def live(tmp_path, monkeypatch):
    """A real Grimoire server + the MCP module pointed at it."""
    port = _free_port()
    env = {**os.environ, "GRIMOIRE_VAULT": str(tmp_path / "vault"),
           "GRIMOIRE_PORT": str(port), "GRIMOIRE_NO_WATCHER": "1",
           "GRIMOIRE_BROKER_ALLOW_PRIVATE": "1"}   # broker target is 127.0.0.1
    proc = subprocess.Popen([sys.executable, "-m", "server"], cwd=ROOT, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(80):
        with socket.socket() as s:
            if s.connect_ex(("127.0.0.1", port)) == 0:
                break
        time.sleep(0.1)
    else:
        proc.kill()
        raise RuntimeError("server did not start")
    monkeypatch.setenv("GRIMOIRE_API", f"http://127.0.0.1:{port}")
    monkeypatch.setenv("GRIMOIRE_AGENT_NAME", "test-agent")
    import server.mcp_server as m
    importlib.reload(m)                 # rebind API/AGENT_NAME from env
    yield m, f"http://127.0.0.1:{port}"
    proc.terminate()
    proc.wait(timeout=10)


def _post(base, path, body):
    req = urllib.request.Request(base + path, method="POST",
                                 headers={"Content-Type": "application/json"},
                                 data=json.dumps(body).encode())
    with urllib.request.urlopen(req, timeout=10) as r:
        return json.load(r)


def test_remember_recall_round_trip_with_provenance(live):
    m, base = live
    out = m.remember("the CI cache lives on the NAS", topic="infra", task="t-9")
    assert out["path"] == "memory/infra.md"
    hits = m.recall("CI cache")
    assert hits and hits[0]["path"] == "memory/infra.md"
    assert "test-agent" in hits[0]["body"]          # provenance attributed
    # the memory is an ordinary, human-readable note on the server
    note = json.load(urllib.request.urlopen(base + "/api/notes/memory/infra.md"))
    assert note["frontmatter"]["memory"] is True


def test_use_credential_brokers_without_revealing_value(live):
    m, base = live
    # a tiny local service that records what auth header it receives
    seen = {}

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            seen["auth"] = self.headers.get("Authorization", "")
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"ok": true}')

        def log_message(self, *a):
            pass

    srv = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    target = f"http://127.0.0.1:{srv.server_port}/data"

    _post(base, "/api/vault/init", {"passphrase": "mcp-test-pass-1"})
    _post(base, "/api/secrets", {"name": "svc-key", "value": "TOP-SECRET-VALUE"})
    grant = _post(base, "/api/secrets/svc-key/grant",
                  {"grantee": "test-agent", "scope": f"http://127.0.0.1:{srv.server_port}/",
                   "ttl_seconds": 60})["grant"]

    result = m.use_credential(grant, target)
    srv.shutdown()
    assert seen["auth"] == "TOP-SECRET-VALUE"      # raw server-side injection
    # (raw, not "Bearer x" — the header NAME is caller-chosen so X-Api-Key works)
    assert "TOP-SECRET-VALUE" not in json.dumps(result)   # caller never sees it
    grants = m.list_grants()
    assert grants and grants[0]["grantee"] == "test-agent"
    assert all("TOP-SECRET-VALUE" not in json.dumps(g) for g in grants)


def test_use_credential_denied_outside_scope(live):
    m, base = live
    _post(base, "/api/vault/init", {"passphrase": "mcp-test-pass-2"})
    _post(base, "/api/secrets", {"name": "narrow", "value": "v"})
    grant = _post(base, "/api/secrets/narrow/grant",
                  {"grantee": "test-agent", "scope": "http://127.0.0.1:1/only-here/",
                   "ttl_seconds": 60})["grant"]
    with pytest.raises(urllib.error.HTTPError) as e:
        m.use_credential(grant, "http://127.0.0.1:1/elsewhere")
    assert e.value.code == 403
