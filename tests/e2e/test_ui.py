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
VAULT_PASS = "mypassphrase123"


def _ensure_vault_unlocked(page):
    """The e2e server shares one vault across tests — init OR unlock as needed."""
    page.click("#vault-open")
    page.wait_for_selector("#v-init, #v-unlock, #v-lock", timeout=8000)
    if page.locator("#v-init").count():
        page.fill("#v-pass", VAULT_PASS); page.click("#v-init")
    elif page.locator("#v-unlock").count():
        page.fill("#v-pass", VAULT_PASS); page.click("#v-unlock")
    from playwright.sync_api import expect as _expect
    _expect(page.locator("#v-lock")).to_be_visible(timeout=8000)
    page.click("#vault-close")


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
    # keep e2e hermetic/offline regardless of ambient env
    for var in ("MNEMO_OLLAMA_URL", "MNEMO_LLM", "MNEMO_LLM_MODEL", "MNEMO_WHISPER_URL"):
        env.pop(var, None)
    # the API indexes on every write; the watcher would only add redundant reindex
    # churn over the shared, ever-growing e2e vault (and can starve the server)
    env["MNEMO_NO_WATCHER"] = "1"
    _free(PORT)
    # IMPORTANT: discard server output. A PIPE that nobody drains fills the ~64KB
    # OS buffer after enough uvicorn access-log lines, blocking the server on
    # write — it silently stops serving late in a large run.
    proc = subprocess.Popen([sys.executable, "-m", "server"], cwd=ROOT, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
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
    expect(page.locator("#side-head h1")).to_have_text("Grimoire")


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


def test_graph_view_opens_and_renders(page, server):
    page.goto(server)
    # two linked notes so the graph has at least one edge
    page.once("dialog", lambda d: d.accept("Graph Hub"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Graph Hub", timeout=8000)
    page.once("dialog", lambda d: d.accept("Graph Spoke"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Graph Spoke", timeout=8000)
    page.fill("#content", "points at [[Graph Hub]]")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.click("#graph-open")
    expect(page.locator("#graph-modal")).to_be_visible()
    expect(page.locator("#graph-canvas")).to_be_visible()
    expect(page.locator("#graph-stat")).to_contain_text("notes", timeout=5000)
    expect(page.locator("#graph-stat")).to_contain_text("links")
    # canvas actually paints something (nodes/edges drawn)
    page.wait_for_timeout(400)
    painted = page.evaluate(
        "() => { const c=document.getElementById('graph-canvas');"
        "const x=c.getContext('2d').getImageData(0,0,c.width,c.height).data;"
        "let n=0; for(let i=3;i<x.length;i+=4){if(x[i]!==0)n++;} return n; }")
    assert painted > 0, "graph canvas rendered nothing"
    page.click("#graph-close")
    expect(page.locator("#graph-modal")).to_be_hidden()


def test_task_checkbox_toggles_and_persists(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Task List"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Task List", timeout=8000)
    page.fill("#content", "# Task List\n\n- [ ] buy milk\n- [x] done thing")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # render preview: the open task is an unchecked box, the done one is checked
    page.click("#preview-toggle")
    boxes = page.locator("#preview .task-box")
    expect(boxes).to_have_count(2)
    expect(boxes.nth(0)).not_to_be_checked()
    expect(boxes.nth(1)).to_be_checked()
    # tick the first task
    boxes.nth(0).check()
    # source updates to [x] and autosaves
    expect(page.locator("#content")).to_have_value(re.compile(r"- \[x\] buy milk"), timeout=5000)
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # persists across reload
    page.reload()
    page.click(".note-row .t >> text=Task List")
    expect(page.locator("#content")).to_have_value(re.compile(r"- \[x\] buy milk"), timeout=8000)


def test_command_palette_jumps_to_note(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Palette Target Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Palette Target Note", timeout=8000)
    # open a different note so we can prove the palette navigates
    page.once("dialog", lambda d: d.accept("Some Other Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Some Other Note", timeout=8000)
    # Ctrl+K opens the palette
    page.keyboard.press("Control+k")
    expect(page.locator("#palette")).to_be_visible()
    page.fill("#palette-input", "palette target")
    expect(page.locator("#palette-list .pal-item.sel")).to_contain_text("Palette Target Note")
    page.keyboard.press("Enter")
    expect(page.locator("#palette")).to_be_hidden()
    expect(page.locator("#title")).to_have_value("Palette Target Note", timeout=5000)


def test_command_palette_runs_command(page, server):
    page.goto(server)
    expect(page.locator(".note-row").first).to_be_visible(timeout=8000)
    page.click("#palette-open")
    expect(page.locator("#palette")).to_be_visible()
    page.fill("#palette-input", "graph")
    expect(page.locator("#palette-list .pal-item.sel")).to_contain_text("graph")
    page.keyboard.press("Enter")
    # running the "Open graph view" command opens the graph modal
    expect(page.locator("#graph-modal")).to_be_visible(timeout=5000)


def test_editor_smart_list_continuation(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("List Editor Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("List Editor Note", timeout=8000)
    ta = page.locator("#content")
    ta.click()
    ta.fill("")
    page.keyboard.type("- first")
    page.keyboard.press("Enter")            # auto-continues with "- "
    page.keyboard.type("second")
    expect(ta).to_have_value("- first\n- second")
    page.keyboard.press("Enter")            # "- " empty
    page.keyboard.press("Enter")            # empty item ends the list
    expect(ta).to_have_value("- first\n- second\n")
    # a task line continues as an unchecked task
    ta.fill("")
    page.keyboard.type("- [x] done")
    page.keyboard.press("Enter")
    page.keyboard.type("next")
    expect(ta).to_have_value("- [x] done\n- [ ] next")


def test_editor_toolbar_and_tab(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Toolbar Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Toolbar Note", timeout=8000)
    ta = page.locator("#content")
    ta.click()
    ta.fill("")
    # toolbar task button inserts a task marker at the line start
    page.click('.tb[data-md="task"]')
    expect(ta).to_have_value("- [ ] ")
    page.keyboard.type("write tests")
    # bold toolbar wraps a placeholder when nothing selected
    page.click('.tb[data-md="bold"]')
    expect(ta).to_have_value(re.compile(r"\*\*bold\*\*"))
    # Tab indents the current line by two spaces
    ta.fill("plain")
    page.keyboard.press("Home")
    page.keyboard.press("Tab")
    expect(ta).to_have_value("  plain")
    page.keyboard.press("Shift+Tab")
    expect(ta).to_have_value("plain")


def test_image_embed_renders_and_loads_in_preview(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Image Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Image Note", timeout=8000)
    # upload a 1x1 png the way paste/drop does, then embed it
    path = page.evaluate(
        "async () => {"
        "const b64='iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';"
        "const bin=Uint8Array.from(atob(b64),c=>c.charCodeAt(0));"
        "const fd=new FormData(); fd.append('file', new Blob([bin],{type:'image/png'}), 'dot.png');"
        "const r=await fetch('/api/attach',{method:'POST',body:fd}); return (await r.json()).path; }")
    assert path and path.startswith("attachments/")
    page.fill("#content", f"# Image Note\n\n![[{path}]]")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.click("#preview-toggle")
    img = page.locator("#preview img.embed")
    expect(img).to_be_visible()
    # the embedded image actually decodes (served by /api/file)
    loaded = page.evaluate(
        "() => { const i=document.querySelector('#preview img.embed');"
        "return i && i.complete && i.naturalWidth>0; }")
    assert loaded, "embedded image did not load from /api/file"


def test_theme_toggle_cycles_and_persists(page, server):
    page.goto(server)
    get = "() => document.documentElement.getAttribute('data-theme')"
    assert page.evaluate(get) in (None, "")          # auto by default
    page.click("#theme-toggle")
    assert page.evaluate(get) == "light"
    page.click("#theme-toggle")
    assert page.evaluate(get) == "dark"
    page.reload()                                     # persists
    assert page.evaluate(get) == "dark"
    page.click("#theme-toggle")
    assert page.evaluate(get) in (None, "")           # back to auto


def test_outline_lists_headings_and_navigates(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Outline Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Outline Note", timeout=8000)
    page.fill("#content", "# Top\n\nintro\n\n## Section A\n\naaa\n\n## Section B\n\nbbb")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.click("#outline-btn")
    expect(page.locator("#outline .mi[data-line]")).to_have_count(3)
    expect(page.locator("#outline")).to_contain_text("Section B")
    # clicking a heading moves the caret to that line in the editor
    page.click("#outline .mi >> text=Section B")
    at_caret = page.evaluate(
        "() => { const t=document.getElementById('content');"
        "return t.value.slice(t.selectionStart, t.selectionStart+12); }")
    assert at_caret == "## Section B", f"caret landed at {at_caret!r}"


def test_templates_save_and_apply_via_palette(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Template Source"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Template Source", timeout=8000)
    page.fill("#content", "# {{title}}\n\nmeeting on {{date}}")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # save the current note as a template (palette command → name prompt)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "save current note as template")
    page.once("dialog", lambda d: d.accept("Meeting Tpl"))
    page.keyboard.press("Enter")
    page.wait_for_timeout(400)   # let refreshTemplates run
    # now create a note from it (palette shows "New from: Meeting Tpl" → title prompt)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "new from meeting")
    expect(page.locator("#palette-list .pal-item.sel")).to_contain_text("New from: Meeting Tpl")
    page.once("dialog", lambda d: d.accept("Monday Standup"))
    page.keyboard.press("Enter")
    expect(page.locator("#title")).to_have_value("Monday Standup", timeout=8000)
    import datetime
    today = datetime.date.today().isoformat()
    expect(page.locator("#content")).to_have_value(re.compile(rf"meeting on {today}"))
    expect(page.locator("#content")).not_to_have_value(re.compile(r"\{\{"))


def test_export_note_via_palette_opens_standalone_html(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Exportable Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Exportable Note", timeout=8000)
    page.fill("#content", "# Exportable Note\n\nhello **world**")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "export note as html")
    expect(page.locator("#palette-list .pal-item.sel")).to_contain_text("Export")
    with page.expect_popup() as pop:
        page.keyboard.press("Enter")
    exported = pop.value
    exported.wait_for_load_state()
    html = exported.content()
    assert "<h1>Exportable Note</h1>" in html and "<strong>world</strong>" in html


def test_settings_modal_persists(page, server):
    page.goto(server)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "open settings")
    page.keyboard.press("Enter")
    expect(page.locator("#settings-modal")).to_be_visible()
    expect(page.locator("#settings-body")).to_contain_text("extractive")   # offline in e2e
    page.fill("#set-model", "custom-model:9b")
    page.click("#set-save")
    expect(page.locator("#settings-modal")).to_be_hidden()
    # reopen — the value persisted
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "open settings")
    page.keyboard.press("Enter")
    expect(page.locator("#set-model")).to_have_value("custom-model:9b")


def test_encrypted_note_plaintext_never_in_localstorage(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("NoDraft Secret"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("NoDraft Secret", timeout=8000)
    page.fill("#content", "PLAINTEXTMARKER before encryption")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # init/unlock vault + encrypt this note
    _ensure_vault_unlocked(page)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "encrypt this note")
    page.keyboard.press("Enter")
    expect(page.locator("#content")).to_have_value(re.compile("PLAINTEXTMARKER"), timeout=8000)
    # edit the (unlocked) encrypted note — the draft must NOT store plaintext
    page.fill("#content", "PLAINTEXTMARKER edited secret content")
    page.wait_for_timeout(400)
    dump = page.evaluate("() => JSON.stringify(Object.entries(localStorage))")
    assert "PLAINTEXTMARKER" not in dump, "encrypted plaintext leaked into localStorage"


def test_encrypt_note_end_to_end(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Secret E2E"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Secret E2E", timeout=8000)
    page.fill("#content", "confidential body text")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # secret vault (shared across tests — init or unlock as needed)
    _ensure_vault_unlocked(page)
    # encrypt the note via the command palette
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "encrypt this note")
    page.keyboard.press("Enter")
    # still editable/plaintext while unlocked
    expect(page.locator("#content")).to_have_value(re.compile("confidential body text"), timeout=8000)
    # lock the vault, reopen → the body is hidden and read-only
    page.click("#vault-open")
    page.click("#v-lock")
    page.click("#vault-close")
    page.reload()
    page.click(".note-row .t >> text=Secret E2E")
    expect(page.locator("#content")).to_have_value(re.compile("encrypted at rest"), timeout=8000)
    assert page.evaluate("() => document.getElementById('content').readOnly") is True


def test_delete_then_undo_restores_note(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Trash E2E"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Trash E2E", timeout=8000)
    page.fill("#content", "please recover me")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # delete (confirm dialog) → note leaves the list, Undo toast appears
    page.once("dialog", lambda d: d.accept())
    page.click("#delete-note")
    expect(page.locator(".note-row .t", has_text="Trash E2E")).to_have_count(0, timeout=8000)
    page.click(".toast-btn >> text=Undo")
    # restored: back in the list and open in the editor
    expect(page.locator(".note-row .t", has_text="Trash E2E")).to_be_visible(timeout=8000)
    expect(page.locator("#content")).to_have_value(re.compile("please recover me"), timeout=8000)


def test_word_count_updates(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Wordy Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Wordy Note", timeout=8000)
    page.fill("#content", "one two three four five")
    expect(page.locator("#wordcount")).to_contain_text("5 words")


def test_alias_wikilink_navigates(page, server):
    page.goto(server)
    page.evaluate(
        "async () => {"
        "const p=(b)=>fetch('/api/notes',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});"
        "await p({title:'United States', body:'the country', frontmatter:{aliases:['USA']}});"
        "await p({title:'Geo Note', body:'I live in [[USA]]'}); }")
    page.reload()   # boot re-fetches notes + aliases
    page.click(".note-row .t >> text=Geo Note")
    expect(page.locator("#title")).to_have_value("Geo Note", timeout=8000)
    page.click("#preview-toggle")
    # [[USA]] renders as a RESOLVED wiki-link (alias known) and navigates
    link = page.locator("#preview a.wikilink", has_text="USA")
    expect(link).to_be_visible()
    assert "unresolved" not in (link.get_attribute("class") or "")
    link.click()
    expect(page.locator("#title")).to_have_value("United States", timeout=8000)


def test_pin_via_palette_floats_to_top(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Pin First"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Pin First", timeout=8000)
    page.once("dialog", lambda d: d.accept("Pin Second"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Pin Second", timeout=8000)
    # open the older note and pin it via the palette
    page.click(".note-row .t >> text=Pin First")
    expect(page.locator("#title")).to_have_value("Pin First", timeout=8000)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "pin unpin this note")
    page.keyboard.press("Enter")
    # it floats to the top with a pin marker
    expect(page.locator(".note-row").first).to_contain_text("Pin First")
    expect(page.locator(".note-row").first.locator(".pin")).to_be_visible()


def test_calendar_marks_and_opens_daily_note(page, server):
    import datetime
    page.goto(server)
    page.click("#daily")   # ensure today's daily note exists
    today = datetime.date.today().isoformat()
    expect(page.locator("#title")).to_have_value(today, timeout=8000)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "open calendar")
    page.keyboard.press("Enter")
    expect(page.locator("#calendar-modal")).to_be_visible()
    expect(page.locator(".cal-cell.today")).to_be_visible()      # today outlined
    expect(page.locator(".cal-cell.has")).to_have_count(1)       # today has a note dot
    page.click(".cal-cell.today")                                # opens it
    expect(page.locator("#calendar-modal")).to_be_hidden()
    expect(page.locator("#title")).to_have_value(today, timeout=8000)


def test_tasks_view_lists_toggles_and_jumps(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Task Alpha ZZ"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Task Alpha ZZ", timeout=8000)
    page.fill("#content", "# A\n- [ ] finish quarterly report zz\n- [ ] call alice zz")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "open tasks all notes")
    page.keyboard.press("Enter")
    expect(page.locator("#tasks-modal")).to_be_visible()
    expect(page.locator(".tg-text", has_text="finish quarterly report zz")).to_be_visible()
    # tick one → it leaves the open list
    page.locator(".tg-item", has_text="call alice zz").locator(".tg-box").check()
    expect(page.locator(".tg-text", has_text="call alice zz")).to_have_count(0, timeout=8000)
    # clicking a task jumps to its note
    page.locator(".tg-text", has_text="finish quarterly report zz").click()
    expect(page.locator("#tasks-modal")).to_be_hidden()
    expect(page.locator("#title")).to_have_value("Task Alpha ZZ", timeout=8000)


def test_help_modal_via_palette_and_shortcut(page, server):
    page.goto(server)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "keyboard shortcuts help")
    page.keyboard.press("Enter")
    expect(page.locator("#help-modal")).to_be_visible()
    expect(page.locator("#help-body")).to_contain_text("Command palette")
    page.click("#help-close")
    expect(page.locator("#help-modal")).to_be_hidden()
    # "?" reopens it (focus is on the close button, not a text field)
    page.keyboard.press("?")
    expect(page.locator("#help-modal")).to_be_visible()


def test_markdown_table_renders_in_preview(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Table Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Table Note", timeout=8000)
    page.fill("#content", "# T\n\n| Fruit | Qty |\n|-------|-----|\n| Apple | 3 |\n| Pear | 5 |")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.click("#preview-toggle")
    expect(page.locator("#preview table")).to_be_visible()
    expect(page.locator("#preview th", has_text="Fruit")).to_be_visible()
    expect(page.locator("#preview td", has_text="Apple")).to_be_visible()
    expect(page.locator("#preview tbody tr")).to_have_count(2)


def test_find_and_replace_all(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("FR Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("FR Note", timeout=8000)
    page.fill("#content", "foo bar foo baz foo")
    # Ctrl+F opens the find bar
    page.locator("#content").click()
    page.keyboard.press("Control+f")
    expect(page.locator("#find-bar")).to_be_visible()
    page.fill("#find-input", "foo")
    expect(page.locator("#find-count")).to_have_text("3")     # match count
    page.fill("#replace-input", "XX")
    page.click("#find-all")
    expect(page.locator("#content")).to_have_value("XX bar XX baz XX")
    expect(page.locator("#find-count")).to_have_text("0")
    page.keyboard.press("Escape")
    expect(page.locator("#find-bar")).to_be_hidden()


def test_unlinked_mentions_show_and_link(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Widget Factory"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Widget Factory", timeout=8000)
    page.once("dialog", lambda d: d.accept("Widget Report"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Widget Report", timeout=8000)
    page.fill("#content", "the Widget Factory shipped on time")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # open the target — Widget Report shows as an unlinked mention
    page.click(".note-row .t >> text=Widget Factory")
    expect(page.locator("#unlinked")).to_contain_text("Widget Report", timeout=8000)
    # one-click link → becomes a backlink, leaves the unlinked list
    page.locator("#unlinked .link-btn").first.click()
    expect(page.locator("#backlinks")).to_contain_text("Widget Report", timeout=8000)
    expect(page.locator("#unlinked")).not_to_contain_text("Widget Report")


def test_duplicate_note_via_palette(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Original ZZ"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Original ZZ", timeout=8000)
    page.fill("#content", "duplicate me please")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "duplicate this note")
    page.keyboard.press("Enter")
    expect(page.locator("#title")).to_have_value("Original ZZ (copy)", timeout=8000)
    expect(page.locator("#content")).to_have_value(re.compile("duplicate me please"))
    expect(page.locator(".note-row .t", has_text="Original ZZ (copy)")).to_be_visible()


def test_search_tag_operator_in_ui(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Op Note ZZ"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Op Note ZZ", timeout=8000)
    page.fill("#content", "content here #opztag")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.once("dialog", lambda d: d.accept("Other ZZ"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Other ZZ", timeout=8000)
    page.fill("#content", "content here no tag")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # a tag: operator narrows the results
    page.fill("#search", "content tag:opztag")
    expect(page.locator(".note-row", has_text="Op Note ZZ")).to_be_visible(timeout=8000)
    expect(page.locator(".note-row .t", has_text="Other ZZ")).to_have_count(0)


def test_properties_editor_saves_and_reloads(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Props Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Props Note", timeout=8000)
    page.fill("#content", "# Props\n\ncontent")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.click("#props-btn")
    expect(page.locator("#props-modal")).to_be_visible()
    page.fill("#pr-tags", "alpha, beta")
    page.fill("#pr-aliases", "PN Alias")
    page.check("#pr-pinned")
    page.click("#pr-add")
    page.locator(".pr-crow .pr-ck").last.fill("author")
    page.locator(".pr-crow .pr-cv").last.fill("jm")
    page.click("#pr-save")
    expect(page.locator("#props-modal")).to_be_hidden()
    # pinned marker shows in the list
    expect(page.locator(".note-row", has_text="Props Note").locator(".pin")).to_be_visible(timeout=8000)
    # a tag: search now finds it
    page.fill("#search", "content tag:alpha")
    expect(page.locator(".note-row", has_text="Props Note")).to_be_visible(timeout=8000)
    page.fill("#search", "")
    # reopen properties → values persisted
    page.click(".note-row .t >> text=Props Note")
    page.click("#props-btn")
    expect(page.locator("#pr-tags")).to_have_value(re.compile("alpha"))
    expect(page.locator("#pr-pinned")).to_be_checked()
    expect(page.locator(".pr-crow .pr-ck")).to_have_value("author")


def test_rename_tag_via_palette(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Tag Rename Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Tag Rename Note", timeout=8000)
    page.fill("#content", "has #renameme here")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)

    def handle(d):
        d.accept("freshtag" if "to:" in d.message else "renameme")
    page.on("dialog", handle)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "rename a tag across all notes")
    page.keyboard.press("Enter")
    # the open note's body is rewritten to the new tag
    expect(page.locator("#content")).to_have_value(re.compile(r"#freshtag"), timeout=8000)
    page.remove_listener("dialog", handle)


def test_offline_edit_recovers_and_retries(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Offline Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Offline Note", timeout=8000)
    page.fill("#content", "start")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # go offline and edit — the save fails but the draft is preserved
    page.context.set_offline(True)
    page.fill("#content", "edited while offline")
    expect(page.locator("#save-state")).to_contain_text("offline", timeout=6000)
    draft = page.evaluate("() => localStorage.getItem('mnemo-draft')")
    assert draft and "edited while offline" in draft
    # back online → the queued save retries automatically
    page.context.set_offline(False)
    page.evaluate("() => window.dispatchEvent(new Event('online'))")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=8000)
    # persisted to the server
    page.reload()
    page.click(".note-row .t >> text=Offline Note")
    expect(page.locator("#content")).to_have_value(re.compile("edited while offline"), timeout=8000)


def test_daily_prev_next_and_insert_date(page, server):
    import datetime
    page.goto(server)
    page.click("#daily")   # today's daily note
    today = datetime.date.today()
    expect(page.locator("#title")).to_have_value(today.isoformat(), timeout=8000)
    # previous day
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "previous day daily note")
    page.keyboard.press("Enter")
    yday = (today - datetime.timedelta(days=1)).isoformat()
    expect(page.locator("#title")).to_have_value(yday, timeout=8000)
    # next day → back to today
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "next day daily note")
    page.keyboard.press("Enter")
    expect(page.locator("#title")).to_have_value(today.isoformat(), timeout=8000)
    # insert today's date at cursor
    page.locator("#content").click()
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "insert todays date")
    page.keyboard.press("Enter")
    expect(page.locator("#content")).to_have_value(re.compile(today.isoformat()))


def test_tag_browser_filters(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Browse Seed"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Browse Seed", timeout=8000)
    page.fill("#content", "content with #browsetag")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "browse tags")
    page.keyboard.press("Enter")
    expect(page.locator("#tags-browser-modal")).to_be_visible()
    chip = page.locator(".tag-chip", has_text="browsetag")
    expect(chip).to_be_visible(timeout=5000)
    chip.click()   # filters the note list to that tag
    expect(page.locator("#tags-browser-modal")).to_be_hidden()
    expect(page.locator("#tag-filter-bar")).to_contain_text("browsetag", timeout=5000)


def test_tag_autocomplete(page, server):
    page.goto(server)
    # seed a note with a distinctive tag so it's in the tag index
    page.once("dialog", lambda d: d.accept("Tag Seed"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Tag Seed", timeout=8000)
    page.fill("#content", "seeded #zephyrtag here")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.reload()   # boot re-fetches the tag list
    page.once("dialog", lambda d: d.accept("Tag User"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Tag User", timeout=8000)
    ta = page.locator("#content")
    ta.click()
    ta.fill("about ")
    page.keyboard.type("#zeph")
    # suggestion dropdown shows the existing tag
    expect(page.locator("#complete .c", has_text="zephyrtag")).to_be_visible(timeout=5000)
    page.keyboard.press("Enter")   # accept
    expect(ta).to_have_value(re.compile(r"#zephyrtag"))


def test_wikilink_hover_preview(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Hover Target"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Hover Target", timeout=8000)
    page.fill("#content", "# Hover Target\n\nthis is the hoverable preview body")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.once("dialog", lambda d: d.accept("Hover Source"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Hover Source", timeout=8000)
    page.fill("#content", "see [[Hover Target]] now")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.click("#preview-toggle")
    # hovering the resolved wiki-link pops a preview of the target note
    page.hover("#preview a.wikilink >> text=Hover Target")
    expect(page.locator("#hover-preview")).to_be_visible(timeout=5000)
    expect(page.locator("#hover-preview")).to_contain_text("hoverable preview body")


def test_code_syntax_highlighting(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Code Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Code Note", timeout=8000)
    page.fill("#content", '# Code\n\n```python\ndef f(x):\n    return "hi"  # c\n```')
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.click("#preview-toggle")
    expect(page.locator("#preview pre code.lang-python")).to_be_visible()
    expect(page.locator("#preview .hl-kw", has_text="def")).to_be_visible()
    expect(page.locator("#preview .hl-str", has_text="hi")).to_be_visible()
    expect(page.locator("#preview .hl-com", has_text="# c")).to_be_visible()


def test_callouts_and_highlights_render(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Callout Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Callout Note", timeout=8000)
    page.fill("#content", "> [!tip] Pro tip\n> use ==highlights== here\n\nplain")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.click("#preview-toggle")
    expect(page.locator("#preview .callout.callout-tip")).to_be_visible()
    expect(page.locator("#preview .callout-title", has_text="Pro tip")).to_be_visible()
    expect(page.locator("#preview mark", has_text="highlights")).to_be_visible()


def test_note_list_keyboard_navigation(page, server):
    page.goto(server)
    for t in ("KbdNav Aaa", "KbdNav Bbb"):
        page.once("dialog", lambda d, t=t: d.accept(t))
        page.click("#new-note")
        expect(page.locator("#title")).to_have_value(t, timeout=8000)
    # search to narrow to the two, then arrow-navigate + Enter
    page.fill("#search", "kbdnav")
    expect(page.locator(".note-row .t", has_text="KbdNav")).to_have_count(2, timeout=8000)
    page.locator("#search").press("ArrowDown")
    expect(page.locator(".note-row.kbd-sel")).to_have_count(1)
    page.locator("#search").press("ArrowDown")
    page.locator("#search").press("Enter")
    # a KbdNav note is now open
    expect(page.locator("#title")).to_have_value(re.compile("KbdNav"), timeout=8000)


def test_focus_mode_hides_chrome_and_escapes(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Zen Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Zen Note", timeout=8000)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "toggle focus mode distraction free")
    page.keyboard.press("Enter")
    expect(page.locator("#ed-toolbar")).to_be_hidden()
    expect(page.locator("#sidebar")).to_be_hidden()
    expect(page.locator("#zen-exit")).to_be_visible()
    expect(page.locator("#content")).to_be_visible()      # writing surface stays
    page.keyboard.press("Escape")                          # exit focus
    expect(page.locator("#ed-toolbar")).to_be_visible()
    expect(page.locator("#zen-exit")).to_be_hidden()


def test_note_context_menu_duplicate_and_rename(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Ctx Note"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Ctx Note", timeout=8000)
    page.fill("#content", "context content")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # right-click the note row → context menu
    page.locator(".note-row .t >> text=Ctx Note").click(button="right")
    expect(page.locator("#ctx-menu")).to_be_visible()
    expect(page.locator("#ctx-menu")).to_contain_text("Duplicate")
    page.locator("#ctx-menu .mi", has_text="Duplicate").click()
    expect(page.locator(".note-row .t", has_text="Ctx Note (copy)")).to_be_visible(timeout=8000)
    # rename the copy via the context menu (updates the displayed title)
    page.once("dialog", lambda d: d.accept("Ctx Renamed"))
    page.locator(".note-row .t >> text=Ctx Note (copy)").click(button="right")
    page.locator("#ctx-menu .mi", has_text="Rename").click()
    expect(page.locator(".note-row .t", has_text="Ctx Renamed")).to_be_visible(timeout=8000)


def test_sidebar_collapse_toggle_and_persist(page, server):
    page.goto(server)
    expect(page.locator("#sidebar")).to_be_visible()
    page.click("#sidebar-toggle")
    expect(page.locator("#sidebar")).to_be_hidden()
    # persists across reload
    page.reload()
    expect(page.locator("#editor")).to_be_visible()
    expect(page.locator("#sidebar")).to_be_hidden()
    # Ctrl+\ brings it back
    page.keyboard.press("Control+\\")
    expect(page.locator("#sidebar")).to_be_visible()


def test_sidebar_resize_drags_and_persists(page, server):
    page.goto(server)
    expect(page.locator("#side-head h1")).to_have_text("Grimoire")
    w0 = page.evaluate("() => document.getElementById('sidebar').getBoundingClientRect().width")
    box = page.locator("#sidebar-resize").bounding_box()
    page.mouse.move(box["x"] + 4, box["y"] + 120)
    page.mouse.down()
    page.mouse.move(box["x"] + 4 + 90, box["y"] + 120, steps=6)
    page.mouse.up()
    w1 = page.evaluate("() => document.getElementById('sidebar').getBoundingClientRect().width")
    assert w1 > w0 + 50, f"sidebar did not widen: {w0} -> {w1}"
    # persists across reload (localStorage)
    page.reload()
    expect(page.locator("#side-head h1")).to_have_text("Grimoire")
    w2 = page.evaluate("() => document.getElementById('sidebar').getBoundingClientRect().width")
    assert abs(w2 - w1) < 6, f"sidebar width not persisted: {w1} -> {w2}"


def test_split_divider_resizes(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Divider A"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Divider A", timeout=8000)
    page.click("#split-btn")
    expect(page.locator("#editor2")).to_be_visible()
    main0 = page.evaluate("() => document.getElementById('editor').getBoundingClientRect().width")
    box = page.locator("#split-resize").bounding_box()
    page.mouse.move(box["x"] + 4, box["y"] + 150)
    page.mouse.down()
    page.mouse.move(box["x"] + 4 - 120, box["y"] + 150, steps=6)   # drag divider left
    page.mouse.up()
    main1 = page.evaluate("() => document.getElementById('editor').getBoundingClientRect().width")
    assert main1 < main0 - 60, f"main pane did not shrink: {main0} -> {main1}"


def test_split_view_desktop(page, server):
    page.goto(server)
    page.once("dialog", lambda d: d.accept("Split Left"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Split Left", timeout=8000)
    page.once("dialog", lambda d: d.accept("Split Right"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Split Right", timeout=8000)
    # main pane = Split Left
    page.click(".note-row .t >> text=Split Left")
    expect(page.locator("#title")).to_have_value("Split Left", timeout=8000)
    # Ctrl+click Split Right → opens in the second pane
    page.click(".note-row .t >> text=Split Right", modifiers=["Control"])
    expect(page.locator("#editor2")).to_be_visible()
    expect(page.locator("#title2")).to_have_value("Split Right", timeout=8000)
    expect(page.locator("#title")).to_have_value("Split Left")     # panes independent
    # edit the right pane; it autosaves
    page.fill("#content2", "right pane edited text")
    expect(page.locator("#save-state2")).to_have_text("saved", timeout=5000)
    # close split, reopen the right note in the main pane → edit persisted
    page.click("#editor2-close")
    expect(page.locator("#editor2")).to_be_hidden()
    page.click(".note-row .t >> text=Split Right")
    expect(page.locator("#content")).to_have_value(re.compile("right pane edited text"), timeout=8000)


@pytest.mark.parametrize("page", [PHONE], indirect=True, ids=["phone"])
def test_split_falls_back_to_single_pane_on_phone(page, server):
    page.goto(server)
    expect(page.locator("#side-head h1")).to_have_text("Grimoire")
    # the split control is hidden on phones, and the second pane never shows
    expect(page.locator("#split-btn")).to_be_hidden()
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "split view open current note on the right")
    page.keyboard.press("Enter")
    page.wait_for_timeout(300)
    assert "split" not in (page.locator("#app").get_attribute("class") or "")
    expect(page.locator("#editor2")).to_be_hidden()


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
    expect(page.locator("#side-head h1")).to_have_text("Grimoire")
    page.wait_for_timeout(500)
    assert not [e for e in errs if "favicon" not in e], errs
