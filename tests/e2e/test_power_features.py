"""Power features in the browser: version history modal, note composer
(extract/merge), folder tree, outgoing links, slides, canvas."""
from playwright.sync_api import expect


def _new_note(pg, title):
    pg.wait_for_selector("body[data-ready]", timeout=10000)   # app fully booted
    pg.once("dialog", lambda d: d.accept(title))
    pg.click("#new-note")
    expect(pg.locator("#title")).to_have_value(title, timeout=8000)


def _palette(pg, query):
    pg.keyboard.press("Control+k")
    pg.fill("#palette-input", query)


def test_version_history_view_and_restore(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(page, "Hist Note")
    page.fill("#content", "first version")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.fill("#content", "second version")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    _palette(page, "version history")
    page.keyboard.press("Enter")
    expect(page.locator("#history-modal .v-row").first).to_be_visible(timeout=8000)
    page.locator(".h-view").first.click()
    expect(page.locator("#history-preview")).to_contain_text("first version")
    page.once("dialog", lambda d: d.accept())
    page.locator(".h-restore").first.click()
    expect(page.locator("#content")).to_have_value("first version\n", timeout=8000)


def test_extract_selection_creates_linked_note(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(page, "Extract Host")
    page.fill("#content", "keep this EXTRACT-THIS-PART end")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    # select the middle words
    page.evaluate("""() => {
      const ta = document.querySelector('#content');
      const s = ta.value.indexOf('EXTRACT'); ta.setSelectionRange(s, s + 'EXTRACT-THIS-PART'.length);
    }""")
    page.once("dialog", lambda d: d.accept("Extracted Bit"))
    _palette(page, "extract selection")
    page.keyboard.press("Enter")
    expect(page.locator("#content")).to_have_value(
        "keep this [[Extracted Bit]] end", timeout=8000)
    body = page.evaluate(
        "async () => (await (await fetch('/api/notes/extracted-bit.md')).json()).body")
    assert "EXTRACT-THIS-PART" in body


def test_merge_note_into_another(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    page.evaluate(
        "() => fetch('/api/notes', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({title:'Merge Target', body:'target body'})})")
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(page, "Merge Source")
    page.fill("#content", "source content to move")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    dialogs = iter(["Merge Target", None])   # prompt(title) then confirm()
    page.on("dialog", lambda d: d.accept(next(dialogs) or ""))
    _palette(page, "merge this note")
    page.keyboard.press("Enter")
    expect(page.locator("#title")).to_have_value("Merge Target", timeout=8000)
    body = page.evaluate(
        "async () => (await (await fetch('/api/notes/merge-target.md')).json()).body")
    assert "source content to move" in body and "## Merge Source" in body


def test_folder_tree_groups_and_collapses(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    for path, title in [("projects/alpha.md", "Alpha P"), ("projects/beta.md", "Beta P")]:
        page.evaluate(
            "([p, t]) => fetch('/api/notes', {method:'POST',"
            "headers:{'Content-Type':'application/json'},"
            "body: JSON.stringify({path: p, title: t, body: 'x'})})", [path, title])
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    folder = page.locator("details.folder", has_text="projects/")
    expect(folder).to_be_visible(timeout=8000)
    expect(folder.locator(".note-row", has_text="Alpha P")).to_be_visible()
    folder.locator("summary").click()      # collapse
    expect(folder.locator(".note-row", has_text="Alpha P")).not_to_be_visible()


def test_outgoing_links_panel(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    page.evaluate(
        "() => fetch('/api/notes', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({title:'Out Target', body:'x'})})")
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(page, "Out Source")
    page.fill("#content", "see [[Out Target]] and [[Ghost Note]]")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    page.goto(server + "/#out-source.md")
    page.reload()
    out = page.locator(".outgoing")
    expect(out).to_be_visible(timeout=8000)
    expect(out.locator("a.wikilink", has_text="Out Target")).to_be_visible()
    expect(out.locator(".unresolved", has_text="Ghost Note")).to_be_visible()


def test_slides_present_mode(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(page, "Deck Note")
    page.fill("#content", "# One\nfirst\n---\n# Two\nsecond")
    expect(page.locator("#save-state")).to_have_text("saved", timeout=5000)
    _palette(page, "present this note")
    page.keyboard.press("Enter")
    expect(page.locator("#slides .slide h1", has_text="One")).to_be_visible(timeout=5000)
    page.keyboard.press("ArrowRight")
    expect(page.locator("#slides .slide h1", has_text="Two")).to_be_visible()
    page.keyboard.press("Escape")
    expect(page.locator("#slides")).to_have_count(0)


def test_canvas_create_add_card_persist(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    dialogs = iter(["Ideas", "my first card"])   # canvas name, then card text
    page.on("dialog", lambda d: d.accept(next(dialogs, "")))
    _palette(page, "new canvas")
    page.keyboard.press("Enter")
    expect(page.locator("#canvas-view")).to_be_visible(timeout=8000)
    # double-click the background to add a text card
    page.dblclick(".cv-viewport", position={"x": 400, "y": 300})
    expect(page.locator(".cv-node", has_text="my first card")).to_be_visible(timeout=5000)
    expect(page.locator("#cv-save")).to_have_text("saved", timeout=6000)
    # persisted server-side in JSON Canvas format
    doc = page.evaluate(
        "async () => await (await fetch('/api/canvas/canvases/ideas.canvas')).json()")
    assert doc["nodes"][0]["text"] == "my first card"
    page.click("#cv-close")
    expect(page.locator("#canvas-view")).to_have_count(0)
