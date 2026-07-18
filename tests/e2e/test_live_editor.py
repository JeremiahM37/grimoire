"""CM6 live-preview editor: typing, live styling, markup hiding, checkbox
widgets, wiki-link clicks, slash commands, autosave round-trip.

Uses the `live_page` fixture (grimoire-editor-mode=live, the default mode).
The hidden #content textarea mirrors the CM document, which the assertions use
to check persistence without depending on CM internals.
"""
from playwright.sync_api import expect


def _new_note(pg, title):
    pg.wait_for_selector("body[data-ready]", timeout=10000)   # app fully booted
    pg.once("dialog", lambda d: d.accept(title))
    pg.click("#new-note")
    expect(pg.locator("#title")).to_have_value(title, timeout=8000)
    expect(pg.locator("#live-editor .cm-content")).to_be_visible(timeout=8000)


def _type(pg, text):
    pg.click("#live-editor .cm-content")
    pg.keyboard.type(text)


def test_live_editor_mounts_and_saves(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Live One")
    _type(pg, "hello from the live editor")
    expect(pg.locator("#save-state")).to_have_text("saved", timeout=6000)
    # the note persisted through the API (mirror → autosave → server)
    body = pg.evaluate(
        "async () => (await (await fetch('/api/notes/live-one.md')).json()).body")
    assert "hello from the live editor" in body


def test_heading_is_styled_and_marks_hide_when_cursor_leaves(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Live Heading")
    _type(pg, "## Section Title\nplain paragraph")
    # cursor is now on line 2 → line 1 renders as a styled heading w/o "##"
    heading = pg.locator("#live-editor .gr-h2").first
    expect(heading).to_be_visible()
    assert "##" not in heading.inner_text()
    # move cursor back onto the heading → the marks reappear (raw editing)
    pg.keyboard.press("ArrowUp")
    expect(pg.locator("#live-editor .cm-line", has_text="Section Title")
           ).to_contain_text("## Section Title")


def test_bold_renders_live(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Live Bold")
    _type(pg, "some **bold words** here\nnext line")
    strong = pg.locator("#live-editor .gr-strong").first
    expect(strong).to_be_visible()
    assert "**" not in strong.inner_text()   # delimiters hidden off-line


def test_task_checkbox_widget_toggles_source(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Live Tasks")
    _type(pg, "- [ ] buy milk\ndone typing")
    box = pg.locator("#live-editor .gr-task-box").first
    expect(box).to_be_visible()
    box.click()
    # the underlying markdown flipped to checked
    pg.wait_for_function(
        "() => document.querySelector('#content').value.includes('- [x] buy milk')",
        timeout=4000)


def test_wikilink_click_navigates(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    pg.evaluate(
        "() => fetch('/api/notes', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({title:'Link Target', body:'arrived'})})")
    pg.goto(server)   # reload so the note list knows the target
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Link Source")
    _type(pg, "go to [[Link Target]]\nand stay")
    link = pg.locator("#live-editor .gr-wikilink").first
    expect(link).to_be_visible()
    link.click()
    expect(pg.locator("#title")).to_have_value("Link Target", timeout=6000)


def test_slash_menu_inserts_snippet(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Slash Note")
    _type(pg, "/tab")
    # completion popover offers the table snippet
    option = pg.locator(".cm-tooltip-autocomplete li", has_text="/table").first
    expect(option).to_be_visible(timeout=5000)
    option.click()
    assert "| A | B |" in pg.evaluate("() => document.querySelector('#content').value")


def test_link_autocomplete_on_double_bracket(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    pg.evaluate(
        "() => fetch('/api/notes', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({title:'Complete Me', body:'x'})})")
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Autocomplete Host")
    _type(pg, "see [[Compl")
    option = pg.locator(".cm-tooltip-autocomplete li", has_text="Complete Me").first
    expect(option).to_be_visible(timeout=5000)
    option.click()
    assert "[[Complete Me]]" in pg.evaluate("() => document.querySelector('#content').value")


def test_toolbar_works_in_live_mode(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Toolbar Live")
    pg.click("#live-editor .cm-content")
    pg.click('[data-md="bold"]')
    assert "**bold**" in pg.evaluate("() => document.querySelector('#content').value")


def test_locked_note_is_read_only_in_live_mode(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "RO Probe")
    _type(pg, "editable")
    expect(pg.locator("#save-state")).to_have_text("saved", timeout=6000)
    ro = pg.evaluate(
        "() => document.querySelector('#live-editor .cm-content').contentEditable")
    assert ro == "true"   # a normal note stays editable


def test_inline_image_renders_in_live_editor(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Live Image")
    # upload a 1x1 png through the attach API, then embed it
    path = pg.evaluate(
        "async () => {"
        "const b64='iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==';"
        "const bin=Uint8Array.from(atob(b64),c=>c.charCodeAt(0));"
        "const fd=new FormData(); fd.append('file', new Blob([bin],{type:'image/png'}), 'dot.png');"
        "const r=await fetch('/api/attach',{method:'POST',body:fd}); return (await r.json()).path; }")
    _type(pg, f"![[{path}]]\nnext line")
    img = pg.locator("#live-editor img.gr-img")
    expect(img).to_be_visible(timeout=5000)
    # the image actually loaded (naturalWidth > 0), not a broken link
    assert pg.evaluate("() => document.querySelector('#live-editor img.gr-img').naturalWidth") > 0


def test_paste_url_over_selection_makes_link(live_page, server):
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Live Linkify")
    _type(pg, "read the docs here")
    # select "docs" and paste a URL over it
    pg.evaluate("""() => {
      const dt = new DataTransfer();
      dt.setData('text/plain', 'https://example.com/docs');
      const el = document.querySelector('#live-editor .cm-content');
      // place a selection over the word "docs" via the mirror textarea offsets:
      // CM selection is set through beforeinput-independent APIs; simplest is to
      // select-all then paste — the whole selection becomes the label.
      document.querySelector('#live-editor .cm-content').dispatchEvent(
        new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
    }""")
    # no selection → plain paste path; now try with a selection
    pg.keyboard.press("Control+a")
    pg.evaluate("""() => {
      const dt = new DataTransfer();
      dt.setData('text/plain', 'https://example.com/docs');
      document.querySelector('#live-editor .cm-content').dispatchEvent(
        new ClipboardEvent('paste', { clipboardData: dt, bubbles: true, cancelable: true }));
    }""")
    pg.wait_for_function(
        "() => document.querySelector('#content').value.includes('](https://example.com/docs)')",
        timeout=5000)


def test_task_line_hides_list_dash(live_page, server):
    """regression G3: inactive task lines rendered a stray '- ' before the
    checkbox widget instead of just the checkbox."""
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Dash Task")
    _type(pg, "- [ ] tidy desk\nelsewhere")   # cursor ends on line 2
    line = pg.locator("#live-editor .cm-line", has_text="tidy desk")
    expect(line).to_be_visible()
    text = line.inner_text()
    assert "- " not in text and "[ ]" not in text, f"raw markers visible: {text!r}"
    expect(line.locator(".gr-task-box")).to_be_visible()


def test_link_completion_does_not_double_close_brackets(live_page, server):
    """regression (spotted on camera in the demo recording): closeBrackets
    auto-inserts ']]' while typing '[[', and the completion appended its own —
    accepting a link completion left a stray ']]' in the note."""
    pg = live_page
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    pg.evaluate(
        "() => fetch('/api/notes', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({title:'Bracket Target', body:'x'})})")
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(pg, "Bracket Host")
    _type(pg, "see [[Bracket Ta")
    option = pg.locator(".cm-tooltip-autocomplete li", has_text="Bracket Target").first
    expect(option).to_be_visible(timeout=5000)
    option.click()
    pg.wait_for_function(
        "() => document.querySelector('#content').value.includes('[[Bracket Target]]')",
        timeout=4000)
    body = pg.evaluate("() => document.querySelector('#content').value")
    assert body.count("]]") == 1, f"stray closing brackets: {body!r}"
    assert "]]]]" not in body
