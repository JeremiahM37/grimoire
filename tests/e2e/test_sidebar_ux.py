"""Sidebar and note-document density: search snippets, the related-notes bar,
row height, and one typeface for reading view and editor."""
import pytest
from conftest import DESKTOP, PHONE
from playwright.sync_api import expect


def _seed(server, page, path, title, body):
    response = page.request.post(server + "/api/notes", data={"path": path, "title": title, "body": body})
    assert response.ok, response.text()


def test_content_search_shows_snippet_with_mark(page, server):
    _seed(server, page, "zephyr-log.md", "Ordinary Log", "# Ordinary Log\n\nThe zephyrquill beacon was lit at dusk.\n")
    page.goto(server)
    page.wait_for_selector("body[data-ready]")
    page.fill("#search", "zephyrquill")
    row = page.locator('.note-row[data-path="zephyr-log.md"]')
    expect(row).to_be_visible(timeout=8000)
    expect(row.locator(".snip mark")).to_have_text("zephyrquill", ignore_case=True)
    # the snippet is text, not markup: the server's brackets and any tags are gone
    snip = row.locator(".snip").inner_text()
    assert "[" not in snip and "<" not in snip and "zephyrquill" in snip.lower()


def test_title_match_is_marked_and_no_match_shows_empty_state(page, server):
    _seed(server, page, "lantern-note.md", "Lantern Inventory", "# Lantern Inventory\n\nFour lamps.\n")
    page.goto(server)
    page.wait_for_selector("body[data-ready]")
    page.fill("#search", "lantern")
    expect(page.locator('.note-row[data-path="lantern-note.md"] .t mark')).to_have_text("Lantern", ignore_case=True, timeout=8000)
    page.fill("#search", "qqnomatchzz")
    empty = page.locator(".search-empty")
    expect(empty).to_have_text("No notes match “qqnomatchzz”.", timeout=8000)
    expect(page.locator(".note-row")).to_have_count(0)


def test_related_bar_collapses_expands_and_stays_bounded(page, server):
    _seed(server, page, "sidebar-target.md", "Sidebar Target", "# Sidebar Target\n\nThe thing being pointed at.\n")
    _seed(server, page, "sidebar-source.md", "Sidebar Source", "# Sidebar Source\n\nLinks to [[Sidebar Target]].\n")
    page.goto(server + "/#sidebar-target.md")
    page.wait_for_selector("body[data-ready]")
    bar = page.locator("#note-connections")
    expect(bar.locator("summary")).to_contain_text("1 backlink", timeout=8000)
    assert bar.get_attribute("open") is None
    assert page.locator("#backlinks").is_hidden()
    assert bar.bounding_box()["height"] <= 34
    bar.locator("summary").click()
    expect(page.locator("#backlinks a", has_text="Sidebar Source")).to_be_visible()
    viewport = page.viewport_size["height"]
    assert bar.bounding_box()["height"] <= 0.4 * viewport
    # the choice is remembered for the next visit (wait for the toggle to be stored before reloading)
    stored = None
    for _ in range(100):  # poll the stored value; the toggle is written by a handler that runs just after the click
        stored = page.evaluate("localStorage.getItem('grimoire-related-open')")
        if stored == "1":
            break
        page.wait_for_timeout(20)
    assert stored == "1"
    page.reload()
    page.wait_for_selector("body[data-ready]")
    expect(page.locator("#note-connections")).to_have_attribute("open", "")


@pytest.mark.parametrize("page", [PHONE], indirect=True, ids=["phone"])
def test_phone_related_bar_and_note_rows_fit_touch_targets(page, server):
    _seed(server, page, "phone-target.md", "Phone Target", "# Phone Target\n\nTapped on a phone.\n")
    _seed(server, page, "phone-source.md", "Phone Source", "# Phone Source\n\nSee [[Phone Target]].\n")
    page.goto(server)
    page.wait_for_selector("body[data-ready]")
    row = page.locator('.note-row[data-path="phone-target.md"]')
    expect(row).to_be_visible(timeout=8000)
    assert 44 <= row.bounding_box()["height"] <= 52
    row.click()
    bar = page.locator("#note-connections")
    expect(bar.locator("summary")).to_contain_text("1 backlink", timeout=8000)
    assert bar.bounding_box()["height"] < 44


@pytest.mark.parametrize("page", [DESKTOP], indirect=True, ids=["desktop"])
def test_desktop_note_rows_are_compact(page, server):
    _seed(server, page, "compact-row.md", "Compact Row", "# Compact Row\n")
    page.goto(server + "/#compact-row.md")
    page.wait_for_selector("body[data-ready]")
    row = page.locator('.note-row[data-path="compact-row.md"]')
    expect(row).to_be_visible(timeout=8000)
    assert row.bounding_box()["height"] <= 34


def test_reading_view_and_editor_share_one_typeface(live_page, server):
    _seed(server, live_page, "typeface.md", "Typeface Check", "# Typeface Check\n\nBody text for the reading view.\n")
    live_page.goto(server + "/#typeface.md")
    live_page.wait_for_selector("body[data-ready]")
    editor = live_page.locator("#live-editor .cm-content")
    expect(editor).to_be_visible(timeout=8000)
    editor_style = editor.evaluate("el => { const s = getComputedStyle(el); return [s.fontFamily, s.fontSize]; }")
    live_page.click("#preview-toggle")
    reading = live_page.locator("#preview > .md")
    expect(reading).to_be_visible(timeout=8000)
    reading_style = reading.evaluate("el => { const s = getComputedStyle(el); return [s.fontFamily, s.fontSize]; }")
    assert reading_style == editor_style
    assert "sans" in reading_style[0].lower()
