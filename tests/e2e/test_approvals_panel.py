"""The Approvals panel, in a real browser, against a real action queue.

An agent asks for an action on a connector; nothing runs until a person decides.
These tests seed the queue through the server's own endpoints (a GitHub connector
pointed at a local stand-in for api.github.com, with one action class enabled),
then drive the panel: approve one (it really executes, against the stand-in) and
deny another, and check the states the owner sees.

The session vault is shared, so everything is namespaced and removed after.
"""
import json
import os
import re
import threading
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest
from conftest import PHONE
from playwright.sync_api import expect

PREFIX = "e2e-approve"


def _api(server, path, method="GET", body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        f"{server}/api{path}", data=data, method=method,
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as r:
        raw = r.read()
    return json.loads(raw) if raw else None


class _FakeGitHub(BaseHTTPRequestHandler):
    """Answers POST /repos/{owner}/{name}/issues like api.github.com does, and
    remembers what it was asked, so a test can see an approval really ran."""
    received: list = []

    def do_POST(self):  # noqa: N802 - http.server's name
        length = int(self.headers.get("Content-Length") or 0)
        payload = json.loads(self.rfile.read(length) or b"{}")
        _FakeGitHub.received.append((self.path, payload))
        number = len(_FakeGitHub.received)
        out = json.dumps({"number": number, "html_url": f"https://github.example/issues/{number}"}).encode()
        self.send_response(201)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *args):  # keep the test output quiet
        pass


@pytest.fixture(scope="module")
def fake_github():
    _FakeGitHub.received = []
    httpd = ThreadingHTTPServer(("127.0.0.1", 0), _FakeGitHub)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{httpd.server_address[1]}"
    httpd.shutdown()
    httpd.server_close()


@pytest.fixture(scope="module")
def connector(server, fake_github):
    """A GitHub connector with exactly one action class enabled: create_issue.
    Approval is left at its default, so every call waits for a person."""
    made = _api(server, "/connectors", method="POST", body={
        "kind": "github", "name": f"{PREFIX}-repo", "prefix": f"{PREFIX}/repo", "interval": 0,
        "config": {"repo": "owner/approvals-demo", "api": fake_github, "actions": "create_issue"},
    })
    yield made
    try:
        _api(server, f"/connectors/{made['id']}", method="DELETE")
    except Exception:  # noqa: BLE001
        pass


def _clear_pending(server, connector):
    """Deny anything this connector still holds, so one failed test cannot leave
    a waiting action behind for the next to trip over."""
    for action in _api(server, "/source-actions?state=pending") or []:
        if action.get("source") == connector["id"]:
            _api(server, f"/source-actions/{action['id']}/deny", method="POST", body={"note": "cleanup"})


@pytest.fixture(autouse=True)
def _tidy(server, connector):
    _clear_pending(server, connector)
    yield
    _clear_pending(server, connector)


def _seed(server, connector, title):
    """Ask for an action the way an agent does. It must come back pending."""
    rec = _api(server, f"/sources/{connector['id']}/act", method="POST", body={
        "action": "create_issue", "params": {"title": title, "body": f"Body for {title}"}})
    assert rec["state"] == "pending", rec
    return rec["id"]


def _open(pg, server):
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    pg.keyboard.press("Control+k")
    pg.fill("#palette-input", "Approvals")
    pg.keyboard.press("Enter")
    expect(pg.locator("#approvals-modal")).to_be_visible(timeout=5000)


def test_the_palette_opens_approvals_and_the_badge_counts_what_waits(page, server, connector):
    approve_id = _seed(server, connector, f"{PREFIX} badge check")
    try:
        page.goto(server)
        page.wait_for_selector("body[data-ready]", timeout=10000)
        # The side toolbar carries the count where other badges appear.
        expect(page.locator("#approvals-open .ap-count")).to_have_text(re.compile(r"^[1-9]\d*$"), timeout=8000)
        _open(page, server)
        expect(page.locator(".ap-card", has_text=f"{PREFIX} badge check")).to_be_visible()
    finally:
        _api(server, f"/source-actions/{approve_id}/deny", method="POST", body={"note": "cleanup"})


def test_approve_runs_the_stored_action_and_deny_records_the_decision(page, server, connector, fake_github):
    approve_id = _seed(server, connector, f"{PREFIX} approve me")
    deny_id = _seed(server, connector, f"{PREFIX} deny me")
    _open(page, server)

    approve_card = page.locator(f'[data-testid="approval-card"][data-id="{approve_id}"]')
    deny_card = page.locator(f'[data-testid="approval-card"][data-id="{deny_id}"]')

    # The headline is the action in plain words; the parameters are one click away.
    expect(approve_card.locator(".ap-title")).to_contain_text("Create GitHub issue in owner/approvals-demo")
    expect(approve_card).to_contain_text("Asked by")
    approve_card.get_by_role("button", name="Show full parameters").click()
    expect(approve_card.locator(".ap-params")).to_contain_text(f"title: {PREFIX} approve me")

    # Approve asks first; cancelling changes nothing.
    approve_card.get_by_role("button", name="Approve…").click()
    expect(approve_card.locator(".ap-confirm")).to_be_visible()
    approve_card.get_by_role("button", name="Cancel").click()
    expect(approve_card.locator(".ap-confirm")).to_have_count(0)
    assert not any("approve me" in p.get("title", "") for _, p in _FakeGitHub.received)

    approve_card.get_by_role("button", name="Approve…").click()
    approve_card.get_by_role("button", name="Yes, approve").click()
    latest = page.locator(".ap-latest")
    expect(latest).to_contain_text("Done.", timeout=15000)
    expect(page.locator(f'[data-testid="approval-card"][data-id="{approve_id}"]')).to_have_count(0)
    # It really ran, with the stored title, against the stand-in provider.
    assert any(p.get("title") == f"{PREFIX} approve me" for _, p in _FakeGitHub.received)

    deny_card.get_by_role("button", name="Deny").click()
    expect(latest).to_contain_text("Denied.", timeout=8000)
    expect(page.locator(f'[data-testid="approval-card"][data-id="{deny_id}"]')).to_have_count(0)
    assert not any(p.get("title") == f"{PREFIX} deny me" for _, p in _FakeGitHub.received)

    # History keeps both, each with its state and outcome.
    page.locator("#ap-tab-history").click()
    done = page.locator(f'[data-testid="history-card"][data-id="{approve_id}"]')
    denied = page.locator(f'[data-testid="history-card"][data-id="{deny_id}"]')
    expect(done.locator(".ap-state")).to_have_text("Done")
    expect(done.locator(".ap-outcome")).to_contain_text("issue")
    expect(denied.locator(".ap-state")).to_have_text("Denied")
    expect(denied.locator(".ap-outcome")).to_contain_text("Denied")


def test_an_empty_queue_explains_itself(page, server, connector):
    _open(page, server)
    expect(page.locator("#approvals-body")).to_contain_text(
        "No actions waiting. Agents can only act after you enable an action for a connector.", timeout=5000)


def test_a_failed_approval_says_why(page, server, connector, fake_github):
    """An approval that cannot run is shown as failed with the reason, not dropped."""
    seeded = _api(server, f"/sources/{connector['id']}/act", method="POST", body={
        "action": "create_issue", "params": {"title": f"{PREFIX} unreachable"}})
    # Point the connector somewhere nothing listens, after the request is queued.
    broken = _api(server, "/connectors", method="POST", body={
        "kind": "github", "name": f"{PREFIX}-broken", "prefix": f"{PREFIX}/broken", "interval": 0,
        "config": {"repo": "owner/approvals-demo", "api": "http://127.0.0.1:9", "actions": "create_issue"}})
    rec = _api(server, f"/sources/{broken['id']}/act", method="POST", body={
        "action": "create_issue", "params": {"title": f"{PREFIX} unreachable"}})
    try:
        _open(page, server)
        card = page.locator(f'[data-testid="approval-card"][data-id="{rec["id"]}"]')
        expect(card).to_be_visible(timeout=5000)
        card.get_by_role("button", name="Approve…").click()
        card.get_by_role("button", name="Yes, approve").click()
        expect(page.locator(".ap-result-failed")).to_contain_text("Failed:", timeout=15000)
        assert "undefined" not in page.locator("#approvals-body").inner_text()
    finally:
        _api(server, f"/source-actions/{seeded['id']}/deny", method="POST", body={"note": "cleanup"})
        _api(server, f"/connectors/{broken['id']}", method="DELETE")


def test_phone_layout_keeps_the_decision_buttons_reachable(browser, server, connector):
    approve_id = _seed(server, connector, f"{PREFIX} on a phone")
    ctx = browser.new_context(viewport=PHONE, service_workers="block")
    pg = ctx.new_page()
    try:
        _open(pg, server)
        card = pg.locator(f'[data-testid="approval-card"][data-id="{approve_id}"]')
        expect(card).to_be_visible(timeout=5000)
        approve = card.get_by_role("button", name="Approve…")
        deny = card.get_by_role("button", name="Deny")
        for button in (approve, deny):
            box = button.bounding_box()
            assert box and box["height"] >= 44, box   # a comfortable thumb target
        # No horizontal scrolling of the page itself.
        assert pg.evaluate("document.documentElement.scrollWidth <= window.innerWidth + 1")
        shots = os.environ.get("GRIMOIRE_APPROVALS_SHOTS")
        if shots:
            os.makedirs(shots, exist_ok=True)
            pg.screenshot(path=os.path.join(shots, "approvals-phone.png"))
    finally:
        ctx.close()
        _api(server, f"/source-actions/{approve_id}/deny", method="POST", body={"note": "cleanup"})


def test_a_desktop_screenshot_of_the_seeded_panel(page, server, connector):
    """Only when asked for: GRIMOIRE_APPROVALS_SHOTS names the directory."""
    shots = os.environ.get("GRIMOIRE_APPROVALS_SHOTS")
    if not shots:
        pytest.skip("set GRIMOIRE_APPROVALS_SHOTS to capture screenshots")
    ids = [_seed(server, connector, f"{PREFIX} screenshot {n}") for n in (1, 2)]
    try:
        _open(page, server)
        expect(page.locator(".ap-card")).to_have_count(2, timeout=5000)
        os.makedirs(shots, exist_ok=True)
        page.locator("#approvals-modal .modal-box").screenshot(path=os.path.join(shots, "approvals-desktop.png"))
    finally:
        for i in ids:
            _api(server, f"/source-actions/{i}/deny", method="POST", body={"note": "cleanup"})
