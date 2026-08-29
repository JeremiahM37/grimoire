"""Network identity, end to end, against a server that really has it enabled.

Grimoire is mounted by agents, and once those agents run on other devices the
name attached to a memory, an audit line or a ledger row stops being a detail:
the authority lattice, the read trail and the cost report are all keyed on who
said something, and until now that was a header the caller set about itself.

These run against a SECOND server with GRIMOIRE_IDENTITY configured, because
the property under test is what changes when it is on — and the session server
deliberately has it off, which is the default every existing deployment has.

The proxy backend is exercised for real: Playwright sets the header, the server
decides whether to believe it based on where the connection came from. The
overlay backends talk to stub daemons over their configured endpoints, which is
ordinary configuration rather than test-only code in the server.
"""
import json
import os
import socket
import subprocess
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest
from conftest import ROOT
from playwright.sync_api import expect

PORT = 9123
BASE = f"http://127.0.0.1:{PORT}"

# What the stub tailscaled says about the one node it knows. The e2e server is
# reached over loopback, so 127.0.0.1 has to be inside the range the backend
# will look up — that is what GRIMOIRE_TAILSCALE_RANGES is for, and setting it
# is also how a real operator scopes the backend to their own tailnet.
TAILNET_NODE = {
    "Node": {"Name": "e2e-laptop.tail878d9e.ts.net.",
             "Hostinfo": {"Hostname": "e2e-laptop", "OS": "linux"}},
    "UserProfile": {"LoginName": "e2e@example.com", "DisplayName": "E2E"},
}


class _Stub(BaseHTTPRequestHandler):
    """Stands in for tailscaled's LocalAPI and a ZeroTier controller."""

    def do_GET(self):  # noqa: N802
        if self.path.startswith("/localapi/v0/whois"):
            return self._json(TAILNET_NODE)
        if "/member" in self.path:
            return self._json([{
                "nodeId": "e2ezt000a", "name": "e2e-zt-node",
                "config": {"ipAssignments": ["127.0.0.1"], "authorized": True},
            }])
        self.send_response(404)
        self.end_headers()

    def _json(self, obj):
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


@pytest.fixture(scope="module")
def stub():
    srv = HTTPServer(("127.0.0.1", 0), _Stub)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


def _start(env_extra, tmp_path_factory, port):
    vault = tmp_path_factory.mktemp("e2e-identity-vault")
    binary = os.environ.get("GRIMOIRE_E2E_SERVER") or str(ROOT / "go" / "grimoire")
    if not Path(binary).exists():
        pytest.skip(f"no grimoire binary at {binary}", allow_module_level=True)
    env = {**os.environ, "GRIMOIRE_VAULT": str(vault), "GRIMOIRE_PORT": str(port),
           "GRIMOIRE_NO_WATCHER": "1", "GRIMOIRE_WEB_DIR": str(ROOT / "web")}
    for var in ("GRIMOIRE_OLLAMA_URL", "GRIMOIRE_LLM", "GRIMOIRE_LLM_MODEL"):
        env.pop(var, None)
    env.update(env_extra)
    proc = subprocess.Popen([binary], cwd=ROOT, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(100):
        with socket.socket() as s:
            if s.connect_ex(("127.0.0.1", port)) == 0:
                break
        time.sleep(0.1)
    else:
        proc.kill()
        raise RuntimeError("identity server did not start")
    return proc


@pytest.fixture(scope="module")
def id_server(stub, tmp_path_factory):
    """A server with all three network backends live at once.

    Ordered tailscale, zerotier, proxy — which also exercises that the first
    backend able to answer wins, since on loopback all three could.
    """
    proc = _start({
        "GRIMOIRE_IDENTITY": "tailscale,zerotier,proxy",
        "GRIMOIRE_TAILSCALE_ENDPOINT": stub,
        "GRIMOIRE_TAILSCALE_RANGES": "127.0.0.1/32",
        "GRIMOIRE_ZEROTIER_API": stub,
        "GRIMOIRE_ZEROTIER_NETWORK": "8bd5124fd6a1b7cc",
        "GRIMOIRE_IDENTITY_PROXY_FROM": "127.0.0.1",
    }, tmp_path_factory, PORT)
    yield BASE
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


@pytest.fixture(scope="module")
def proxy_server(tmp_path_factory):
    """Only the proxy backend, so the header path is tested on its own."""
    proc = _start({
        "GRIMOIRE_IDENTITY": "proxy",
        "GRIMOIRE_IDENTITY_PROXY_FROM": "127.0.0.1",
        "GRIMOIRE_IDENTITY_PROXY_DEVICE_HEADER": "X-Device",
    }, tmp_path_factory, PORT + 1)
    yield f"http://127.0.0.1:{PORT + 1}"
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def _get(base, path, headers=None):
    req = urllib.request.Request(base + path, headers=headers or {})
    with urllib.request.urlopen(req, timeout=15) as r:
        return json.loads(r.read())


# ----------------------------------------------------------------- API level

def test_the_overlay_identifies_the_caller(id_server):
    who = _get(id_server, "/api/identity")
    assert who["enabled"] is True
    assert who["backends"] == ["tailscale", "zerotier", "proxy"]
    assert who["verified"] is True, "a peer the stub daemon knows was not verified"
    # tailscale is configured first and can answer, so it wins.
    assert who["identity"]["backend"] == "tailscale"
    assert who["identity"]["device"] == "e2e-laptop"
    assert who["attributed_to"] == "e2e-laptop"


def test_a_verified_identity_beats_the_name_the_caller_sends(id_server):
    """This is the whole point: attribution stops taking the caller's word."""
    who = _get(id_server, "/api/identity",
               {"X-Grimoire-Agent": "i-am-somebody-else"})
    assert who["attributed_to"] == "e2e-laptop"
    assert who["claimed"] == "i-am-somebody-else", \
        "the claim must still be reported, or a disagreement is invisible"


def test_identity_is_off_unless_asked(server):
    """The session server has no identity configured — the default every
    existing deployment already has, which must be untouched."""
    who = _get(server, "/api/identity")
    assert who["enabled"] is False
    assert who["verified"] is False
    who = _get(server, "/api/identity", {"X-Grimoire-Agent": "claude-code"})
    assert who["attributed_to"] == "claude-code", \
        "with identity off the self-asserted name must work exactly as before"


def test_the_proxy_backend_believes_a_trusted_proxy(proxy_server):
    who = _get(proxy_server, "/api/identity",
               {"Remote-User": "jam", "X-Device": "work-laptop"})
    assert who["verified"] is True
    assert who["identity"]["user"] == "jam"
    assert who["identity"]["device"] == "work-laptop"
    assert who["attributed_to"] == "work-laptop"


def test_a_forwarded_address_cannot_mint_an_identity(proxy_server):
    """The forgery that would make this worse than having no identity at all.

    A caller that could name its own peer address would be able to claim any
    node on the overlay, so identity reads the address the TCP stack accepted
    and never a header — even where a proxy is otherwise trusted."""
    who = _get(proxy_server, "/api/identity", {
        "X-Forwarded-For": "10.0.0.1",
        "X-Real-IP": "10.0.0.1",
        "Remote-User": "jam",
    })
    assert who["peer"] == "127.0.0.1", \
        f"peer came back as {who['peer']}; identity must not read a forwarded header"


def test_an_unmapped_identity_is_attributed_but_grants_nothing(id_server):
    """Knowing truthfully who is calling is not a decision about what they may
    read. With no account mapped, a verified caller is named and otherwise
    treated exactly as before."""
    who = _get(id_server, "/api/identity")
    assert who["verified"] is True
    me = _get(id_server, "/api/me")
    assert me["multi_user"] is False
    assert me["anonymous"] is not True, \
        "a single-user deployment must be unaffected by enabling identity"


# ----------------------------------------------------------------- browser

def _open_usage(pg, base):
    pg.goto(base)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    pg.keyboard.press("Control+k")
    pg.fill("#palette-input", "AI usage")
    pg.keyboard.press("Enter")
    expect(pg.locator("#inspect-modal")).to_be_visible(timeout=5000)


def test_the_console_shows_who_the_caller_is(browser, id_server):
    pg = browser.new_context(viewport={"width": 1280, "height": 860}).new_page()
    try:
        _open_usage(pg, id_server)
        body = pg.locator("#inspect-body")
        expect(body).to_contain_text("How callers are identified", timeout=6000)
        expect(body).to_contain_text("verified")
        expect(body).to_contain_text("e2e-laptop")
        # Which mechanisms are live, so a configuration that never matches is
        # distinguishable from one that works.
        expect(body).to_contain_text("tailscale")
        expect(pg.locator(".id-ok")).to_be_visible()
    finally:
        pg.close()


def test_the_console_says_when_nothing_verifies_callers(browser, server):
    """With identity off the screen must say so, not imply the agent names on
    it were checked."""
    pg = browser.new_context(viewport={"width": 1280, "height": 860}).new_page()
    try:
        _open_usage(pg, server)
        body = pg.locator("#inspect-body")
        expect(body).to_contain_text("How callers are identified", timeout=6000)
        expect(body).to_contain_text("nothing verifies")
        expect(pg.locator(".id-ok")).to_have_count(0)
        expect(body).not_to_contain_text("undefined")
    finally:
        pg.close()


def test_the_console_flags_a_claim_that_disagrees_with_the_identity(browser, id_server):
    """An agent sending a name that is not its verified one is the signal an
    operator most needs, so the two are shown side by side and marked."""
    ctx = browser.new_context(viewport={"width": 1280, "height": 860},
                              extra_http_headers={"X-Grimoire-Agent": "pretending-to-be-ci"})
    pg = ctx.new_page()
    try:
        _open_usage(pg, id_server)
        body = pg.locator("#inspect-body")
        expect(body).to_contain_text("pretending-to-be-ci", timeout=6000)
        expect(body).to_contain_text("disputed")
        # And the name actually recorded is the verified one.
        expect(body).to_contain_text("e2e-laptop")
    finally:
        pg.close()
        ctx.close()


def test_the_identity_panel_reads_on_a_phone(browser, id_server):
    ctx = browser.new_context(viewport={"width": 390, "height": 844})
    pg = ctx.new_page()
    try:
        _open_usage(pg, id_server)
        expect(pg.locator("#inspect-body")).to_contain_text(
            "How callers are identified", timeout=6000)
        overflow = pg.evaluate(
            "() => document.documentElement.scrollWidth - document.documentElement.clientWidth")
        assert overflow <= 2, f"the page scrolls sideways by {overflow}px on a phone"
    finally:
        pg.close()
        ctx.close()
