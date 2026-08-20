"""Multi-user in a real browser, on its own server.

The shared session server is deliberately single-user — that is the deployment
every other test asserts is unchanged — so this module starts a second one,
creates accounts in it, and drives the console the way two colleagues would.
"""
from __future__ import annotations

import json
import os
import socket
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path

import pytest
from playwright.sync_api import expect

ROOT = Path(__file__).resolve().parents[2]
PASSWORD = "correct horse battery"


def _port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _api(base, path, body=None, key=None, method=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data,
                                 method=method or ("POST" if data else "GET"),
                                 headers={"Content-Type": "application/json"})
    if key:
        req.add_header("Authorization", "Bearer " + key)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            raw = r.read()
        return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as e:
        return {"_status": e.code, "_body": e.read().decode()}


@pytest.fixture(scope="module")
def team(tmp_path_factory):
    """A server with two accounts: alice (admin) and bob (member)."""
    vault = tmp_path_factory.mktemp("multiuser-vault")
    port = _port()
    binary = os.environ.get("GRIMOIRE_E2E_SERVER") or str(ROOT / "go" / "grimoire")
    if not Path(binary).exists():
        pytest.skip(f"no grimoire binary at {binary}")
    env = {**os.environ, "GRIMOIRE_VAULT": str(vault), "GRIMOIRE_PORT": str(port),
           "GRIMOIRE_NO_WATCHER": "1", "GRIMOIRE_WEB_DIR": str(ROOT / "web")}
    for var in ("GRIMOIRE_OLLAMA_URL", "GRIMOIRE_LLM", "GRIMOIRE_LLM_MODEL"):
        env.pop(var, None)
    proc = subprocess.Popen([binary], cwd=ROOT, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = f"http://127.0.0.1:{port}"
    for _ in range(150):
        with socket.socket() as s:
            if s.connect_ex(("127.0.0.1", port)) == 0:
                break
        time.sleep(0.1)
    else:
        proc.kill()
        raise RuntimeError("server did not start")

    # the first account needs no credentials; it is what turns multi-user on
    _api(base, "/api/users", {"name": "alice", "display": "Alice",
                              "password": PASSWORD, "role": "admin"})
    keys = {}
    alice = _api(base, "/api/auth/login", {"name": "alice", "password": PASSWORD})
    assert "user" in alice, alice
    # an admin key, to create bob and seed notes
    admin_key = _admin_key(base)
    _api(base, "/api/users", {"name": "bob", "display": "Bob",
                              "password": PASSWORD, "role": "member"}, key=admin_key)
    keys["alice"] = admin_key
    _api(base, "/api/notes", {"path": "users/alice/diary.md",
                              "body": "# Diary\n\nkestrel migration thoughts"}, key=admin_key)
    _api(base, "/api/notes", {"path": "shared-runbook.md",
                              "body": "# Runbook\n\nrollback with force-recreate"}, key=admin_key)
    yield base, keys
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()


def _admin_key(base):
    """Log in as alice over HTTP and mint an API key with the session."""
    import http.cookiejar
    jar = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))
    req = urllib.request.Request(base + "/api/auth/login",
                                 data=json.dumps({"name": "alice", "password": PASSWORD}).encode(),
                                 headers={"Content-Type": "application/json"})
    opener.open(req, timeout=30).read()
    req = urllib.request.Request(base + "/api/keys",
                                 data=json.dumps({"label": "e2e"}).encode(),
                                 headers={"Content-Type": "application/json"})
    out = json.loads(opener.open(req, timeout=30).read())
    return out["key"]


def test_console_asks_for_sign_in_and_then_works(browser, team):
    base, _ = team
    ctx = browser.new_context(viewport={"width": 1280, "height": 860})
    page = ctx.new_page()
    page.goto(base)
    page.wait_for_selector("body[data-ready]", timeout=15000)

    # An instance with accounts shows the sign-in overlay, not the vault.
    expect(page.locator("#signin-form")).to_be_visible(timeout=5000)
    assert "kestrel" not in page.content()

    page.fill("#signin-name", "alice")
    page.fill("#signin-pass", PASSWORD)
    page.click("#signin-go")

    # Signed in: the overlay is gone and alice's notes are there.
    expect(page.locator("#signin-form")).to_have_count(0, timeout=10000)
    expect(page.locator("#note-list")).to_contain_text("Diary", timeout=10000)
    expect(page.locator(".signin-who")).to_contain_text("alice")
    ctx.close()


def test_a_member_never_sees_another_members_notes(browser, team):
    base, _ = team
    ctx = browser.new_context(viewport={"width": 1280, "height": 860})
    page = ctx.new_page()
    page.goto(base)
    page.wait_for_selector("body[data-ready]", timeout=15000)
    page.fill("#signin-name", "bob")
    page.fill("#signin-pass", PASSWORD)
    page.click("#signin-go")
    expect(page.locator("#signin-form")).to_have_count(0, timeout=10000)

    # Bob sees the commons note and not alice's personal one — in the list,
    # in search, and in what an agent would retrieve.
    expect(page.locator("#note-list")).to_contain_text("Runbook", timeout=10000)
    assert "Diary" not in page.locator("#note-list").inner_text()

    page.fill("#search", "kestrel")
    page.wait_for_timeout(900)
    assert "Diary" not in page.locator("#note-list").inner_text()

    retrieved = page.evaluate(
        "async () => (await fetch('/api/retrieve?q=kestrel&k=10')).text()")
    assert "users/alice" not in retrieved

    # And the sign-out control returns to the overlay.
    page.click("#signout")
    page.wait_for_selector("#signin-form", timeout=15000)
    ctx.close()


def test_wrong_password_is_refused_in_the_browser(browser, team):
    base, _ = team
    ctx = browser.new_context(viewport={"width": 1280, "height": 860})
    page = ctx.new_page()
    page.goto(base)
    page.wait_for_selector("body[data-ready]", timeout=15000)
    page.fill("#signin-name", "alice")
    page.fill("#signin-pass", "not the password")
    page.click("#signin-go")
    expect(page.locator("#signin-error")).to_contain_text("wrong", timeout=8000)
    expect(page.locator("#signin-form")).to_be_visible()
    ctx.close()
