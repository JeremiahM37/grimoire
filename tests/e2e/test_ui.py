"""Playwright e2e — real browser against a real server on a temp vault."""
import os
import socket
import subprocess
import sys
import time
from pathlib import Path

import re

import pytest
from playwright.sync_api import expect, sync_playwright

ROOT = Path(__file__).resolve().parents[2]
PORT = 9121
BASE = f"http://127.0.0.1:{PORT}"
PHONE = {"width": 390, "height": 844}
DESKTOP = {"width": 1280, "height": 860}


def _free(port):
    try:
        out = subprocess.run(["ss", "-tlnp"], capture_output=True, text=True).stdout
        for line in out.splitlines():
            if f":{port} " in line and "pid=" in line:
                subprocess.run(["kill", "-9", line.split("pid=")[1].split(",")[0]],
                               capture_output=True)
    except Exception:
        pass


@pytest.fixture(scope="session")
def server(tmp_path_factory):
    vault = tmp_path_factory.mktemp("e2e-vault")
    env = {**os.environ, "MNEMO_VAULT": str(vault), "MNEMO_PORT": str(PORT)}
    _free(PORT)
    proc = subprocess.Popen([sys.executable, "-m", "server"], cwd=ROOT, env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    for _ in range(100):
        with socket.socket() as s:
            if s.connect_ex(("127.0.0.1", PORT)) == 0:
                break
        time.sleep(0.1)
    else:
        proc.kill(); raise RuntimeError("server did not start")
    yield BASE
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
    _free(PORT)


@pytest.fixture(scope="session")
def browser():
    with sync_playwright() as p:
        b = p.chromium.launch()
        yield b
        b.close()


@pytest.fixture()
def page(browser, server, request):
    ctx = browser.new_context(viewport=getattr(request, "param", DESKTOP))
    pg = ctx.new_page()
    yield pg
    ctx.close()


@pytest.mark.parametrize("page", [DESKTOP, PHONE], indirect=True, ids=["desktop", "phone"])
def test_app_loads(page, server):
    page.goto(server)
    expect(page.locator("#side-head h1")).to_have_text("mnemo")


def test_create_note_and_it_appears(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("My E2E Note"))
    page.click("#new-note")
    expect(page.locator(".note-row .t", has_text="My E2E Note")).to_be_visible(timeout=8000)
    expect(page.locator("#title")).to_have_value("My E2E Note")


def test_edit_saves_and_persists(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Persist Test"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Persist Test", timeout=8000)
    page.fill("#content", "# Persist Test\n\nbody with a #savedtag")
    # wait for the debounced autosave
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # reload → content persisted (came from the real .md file via reindex)
    page.reload()
    page.click(".note-row .t >> text=Persist Test")
    expect(page.locator("#content")).to_have_value(re.compile("#savedtag"), timeout=8000)


def test_wikilink_backlink_and_navigation(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Target Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Target Note", timeout=8000)
    page.once("dialog", lambda d: d.accept("Source Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Source Note", timeout=8000)
    page.fill("#content", "links to [[Target Note]]")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # open Target → its backlinks show Source
    page.click(".note-row .t >> text=Target Note")
    expect(page.locator("#backlinks a", has_text="Source Note")).to_be_visible(timeout=8000)
    # clicking the backlink navigates
    page.click("#backlinks a >> text=Source Note")
    expect(page.locator("#title")).to_have_value("Source Note")


def test_search_filters_list(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Searchable Apples"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Searchable Apples", timeout=8000)
    page.fill("#content", "apples are a red fruit")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.fill("#search", "fruit")
    expect(page.locator(".note-row", has_text="Searchable Apples")).to_be_visible(timeout=8000)


def test_tag_click_filters_list(page, server):
    page.goto(server)
    # a note that carries a distinctive tag
    page.once("dialog", lambda d: d.accept("Tagged Alpha"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Tagged Alpha", timeout=8000)
    page.fill("#content", "belongs to #projectx")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # an unrelated note that must NOT survive the filter
    page.once("dialog", lambda d: d.accept("Unrelated Beta"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Unrelated Beta", timeout=8000)
    page.fill("#content", "no tag here")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # reopen the tagged note, switch to preview, click its tag
    page.click(".note-row .t >> text=Tagged Alpha")
    expect(page.locator("#title")).to_have_value("Tagged Alpha", timeout=8000)
    page.click("#preview-toggle")
    page.click(".md .tag >> text=#projectx")
    # the filter bar shows the tag and the list is narrowed to matches only
    expect(page.locator("#tag-filter-bar")).to_contain_text("projectx", timeout=8000)
    expect(page.locator(".note-row .t", has_text="Tagged Alpha")).to_be_visible()
    expect(page.locator(".note-row .t", has_text="Unrelated Beta")).to_have_count(0)
    # clearing restores the full list
    page.click("#clear-tag")
    expect(page.locator("#tag-filter-bar")).to_have_count(0)
    expect(page.locator(".note-row .t", has_text="Unrelated Beta")).to_be_visible(timeout=8000)


def test_external_note_appears_without_reload(page, server):
    """The 'syncs from all devices' promise: a note created OUTSIDE this tab
    (device sync / MCP agent / external editor) shows up via the live poll."""
    page.goto(server)
    expect(page.locator(".note-row").first).to_be_visible(timeout=8000)
    # create a note out-of-band, exactly as a sync client or the AI would
    page.evaluate(
        "() => fetch('/api/notes', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({title:'Synced From Afar', body:'no reload needed'})})")
    # the poll (5s) picks it up without any user action or reload
    expect(page.locator(".note-row .t", has_text="Synced From Afar")).to_be_visible(timeout=12000)


def test_daily_note(page, server):
    page.goto(server)
    page.click("#daily")
    import time as _t
    expect(page.locator("#title")).to_have_value(_t.strftime("%Y-%m-%d"), timeout=8000)


def test_ask_your_notes(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Coffee Guide"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Coffee Guide", timeout=8000)
    page.fill("#content", "Espresso is brewed by forcing hot water through fine coffee grounds.")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.click("#ask-open")
    page.fill("#ask-q", "how is espresso brewed")
    page.click("#ask-go")
    expect(page.locator("#ask-answer")).to_contain_text("coffee", timeout=8000)
    expect(page.locator("#ask-cites .cite", has_text="Coffee Guide")).to_be_visible()
    # clicking a citation navigates
    page.click("#ask-cites .cite")
    expect(page.locator("#title")).to_have_value("Coffee Guide")


def test_private_toggle_hides_from_ask(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Secret Recipe"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Secret Recipe", timeout=8000)
    page.fill("#content", "The mysterious flumberry sauce uses a rare ingredient.")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # mark private
    page.click("#private-toggle")
    expect(page.locator("#private-toggle")).to_have_text("🔒", timeout=5000)
    # ask should not surface it
    page.click("#ask-open")
    page.fill("#ask-q", "flumberry sauce ingredient")
    page.click("#ask-go")
    page.wait_for_timeout(1200)
    expect(page.locator("#ask-cites")).not_to_contain_text("Secret Recipe")


def test_secret_vault_flow(page, server):
    page.goto(server)
    page.click("#vault-open")
    # wait for the vault body to finish its async render, then branch on state
    page.wait_for_selector("#v-init, #v-unlock, #v-add", timeout=8000)
    if page.locator("#v-init").count():
        page.fill("#v-pass", "mypassphrase123")
        page.click("#v-init")
    elif page.locator("#v-unlock").count():
        page.fill("#v-pass", "mypassphrase123")
        page.click("#v-unlock")
    # add a secret (value never displayed)
    expect(page.locator("#v-add")).to_be_visible(timeout=8000)
    page.fill("#v-name", "githubtoken")
    page.fill("#v-val", "ghp_supersecret_value")
    page.click("#v-add")
    expect(page.locator(".v-row", has_text="githubtoken")).to_be_visible(timeout=8000)
    # the raw value must NEVER be in the DOM
    assert "ghp_supersecret_value" not in page.content()
    # lock it
    page.click("#v-lock")
    expect(page.locator("#v-unlock")).to_be_visible(timeout=8000)


def test_deep_link_hashchange_navigates(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Deep A"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Deep A", timeout=8000)
    page.once("dialog", lambda d: d.accept("Deep B"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Deep B", timeout=8000)
    # changing the hash (as browser back/forward or a shared link would) re-opens
    page.evaluate("location.hash = 'deep-a.md'")
    expect(page.locator("#title")).to_have_value("Deep A", timeout=5000)


def test_preview_does_not_execute_injected_script(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("XSS Probe"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("XSS Probe", timeout=8000)
    page.fill("#content", "danger <script>window.__xss=1</script> <img src=x onerror='window.__xss=2'>")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.evaluate("window.__xss = 0")
    page.click("#preview-toggle")   # render markdown → HTML
    page.wait_for_timeout(400)
    assert page.evaluate("window.__xss") == 0     # no injected script/handler ran
    # and the raw tag is shown as escaped text, not a live element
    assert page.locator("#preview script").count() == 0


def test_no_console_errors_on_load(page, server):
    errs = []
    page.on("console", lambda m: errs.append(m.text) if m.type == "error" else None)
    page.on("pageerror", lambda e: errs.append(str(e)))
    page.goto(server)
    expect(page.locator("#side-head h1")).to_have_text("mnemo")
    page.wait_for_timeout(500)
    assert not [e for e in errs if "favicon" not in e], errs
