"""Phone UX: bottom tab bar, full-screen note, sheets, pinned toolbar, back gesture."""
import pytest
from playwright.sync_api import expect

PIXEL = dict(viewport={"width": 412, "height": 915}, device_scale_factor=2.625, is_mobile=True, has_touch=True)
NOTES = {f"mobile-{i}.md": f"# Mobile note {i}\n\nBody of note {i} about kestrels.\n" for i in range(6)}


@pytest.fixture()
def phone(browser, server):
    ctx = browser.new_context(service_workers="block", **PIXEL)
    ctx.add_init_script("localStorage.setItem('grimoire-editor-mode', 'classic')")
    pg = ctx.new_page()
    for path, body in NOTES.items():
        pg.request.post(f"{server}/api/notes", data={"path": path, "body": body})
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]")
    expect(pg.locator("#tabbar")).to_be_visible()
    yield pg
    ctx.close()


def swipe(page, x1, y1, x2, y2, steps=8):
    cdp = page.context.new_cdp_session(page)
    cdp.send("Input.dispatchTouchEvent", {"type": "touchStart", "touchPoints": [{"x": x1, "y": y1}]})
    for i in range(1, steps + 1):
        cdp.send("Input.dispatchTouchEvent", {"type": "touchMove", "touchPoints": [{"x": x1 + (x2 - x1) * i / steps, "y": y1 + (y2 - y1) * i / steps}]})
    cdp.send("Input.dispatchTouchEvent", {"type": "touchEnd", "touchPoints": []})


def test_list_is_home_with_no_overflow_and_big_targets(phone):
    assert phone.evaluate("document.documentElement.scrollWidth <= innerWidth")
    small = phone.evaluate("""() => [...document.querySelectorAll('#tabbar button, #search, .note-row')]
      .filter(e => e.getBoundingClientRect().height > 0 && e.getBoundingClientRect().height < 44).length""")
    assert small == 0
    assert phone.locator("#sidebar.open").count() == 1


def test_open_note_and_back_by_button_and_system_back(phone):
    phone.locator(".note-row", has_text="Mobile note 2").tap()
    expect(phone.locator("#title")).to_have_value("Mobile note 2")
    expect(phone.locator("#tabbar")).to_have_count(0)
    phone.locator("#menu-open").tap()
    expect(phone.locator("#tabbar")).to_be_visible()
    phone.locator(".note-row", has_text="Mobile note 3").tap()
    expect(phone.locator("#title")).to_have_value("Mobile note 3")
    phone.go_back()  # Android back gesture
    expect(phone.locator("#tabbar")).to_be_visible()


def test_edit_keeps_toolbar_above_keyboard(phone, server):
    phone.locator(".note-row", has_text="Mobile note 1").tap()
    phone.locator("#content").tap()
    expect(phone.locator("#ed-toolbar")).to_be_visible()
    phone.set_viewport_size({"width": 412, "height": 560})  # the keyboard takes the bottom
    phone.wait_for_timeout(300)
    box = phone.locator("#ed-toolbar").bounding_box()
    assert box and abs(box["y"] + box["height"] - 560) <= 2, box
    assert box["height"] >= 44
    phone.keyboard.type(" zebra")
    phone.locator("#ed-toolbar .tb[data-md=bold]").tap()
    phone.wait_for_timeout(1500)
    body = phone.request.get(f"{server}/api/notes/mobile-1.md").json()["body"]
    assert "zebra" in body and "**" in body


def test_search_filters_the_list(phone):
    phone.locator("#search").fill("Mobile note 4")
    expect(phone.locator(".note-row")).to_have_count(1)


def test_capture_sheet_saves_and_swipes_away(phone, server):
    phone.locator("#tabbar .fab").tap()
    sheet = phone.locator("#capture-sheet .modal-box")
    expect(sheet).to_be_visible()
    phone.fill("#capture-text", "quokka capture from the phone")
    phone.locator("#capture-save").tap()
    expect(phone.locator("#capture-sheet")).to_have_count(0)
    hits = phone.request.get(f"{server}/api/search", params={"q": "quokka"}).json()
    assert "quokka" in str(hits)
    # swipe-down dismisses a sheet
    phone.locator("#tabbar .fab").tap()
    box = phone.locator("#capture-sheet .modal-box").bounding_box()
    swipe(phone, 206, box["y"] + 14, 206, box["y"] + 260)
    expect(phone.locator("#capture-sheet")).to_have_count(0)


def test_more_sheet_opens_screens_and_back_closes_them(phone):
    phone.locator("#tabbar >> text=More").tap()
    phone.locator("#more-sheet >> text=Graph").tap()
    expect(phone.locator("#graph-modal")).to_be_visible()
    phone.wait_for_selector("#graph-canvas canvas, #graph-canvas")
    phone.wait_for_timeout(1500)
    box = phone.locator("#graph-canvas").bounding_box()
    phone.touchscreen.tap(box["x"] + box["width"] / 2, box["y"] + box["height"] / 2)  # tap on the graph
    phone.wait_for_timeout(300)
    assert phone.evaluate("document.documentElement.scrollWidth <= innerWidth")
    phone.go_back()
    expect(phone.locator("#graph-modal")).to_have_count(0)
    expect(phone.locator("#tabbar")).to_be_visible()


def test_manifest_and_offline_shell(browser, server):
    ctx = browser.new_context(**PIXEL)
    pg = ctx.new_page()
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]")
    m = pg.request.get(f"{server}/manifest.webmanifest").json()
    assert m["display"] == "standalone" and m["start_url"] == "/"
    sizes = {i["sizes"] for i in m["icons"]}
    assert {"192x192", "512x512"} <= sizes and any(i.get("purpose") == "maskable" for i in m["icons"])
    pg.evaluate("navigator.serviceWorker.ready.then(() => 1)")
    pg.wait_for_timeout(1500)
    ctx.set_offline(True)
    pg.reload()
    pg.wait_for_selector("body[data-ready]")
    expect(pg.locator("#app")).to_be_visible()
    ctx.close()


def test_static_assets_are_compressed_and_cached(server, browser):
    ctx = browser.new_context()
    pg = ctx.new_page()
    html = pg.request.get(f"{server}/").text()
    import re
    js = re.search(r'/assets/index-[^"]+\.js', html).group(0)
    r = pg.request.get(f"{server}{js}", headers={"Accept-Encoding": "gzip"})
    assert "immutable" in r.headers["cache-control"]
    assert r.headers.get("content-encoding") == "gzip"
    ctx.close()


def test_theme_color_follows_the_theme_not_a_hard_coded_cream(phone, server):
    m = phone.request.get(f"{server}/manifest.webmanifest").json()
    assert m["theme_color"] == m["background_color"] == "#1a1814"
    phone.evaluate("document.documentElement.dataset.theme = 'dark'")
    phone.wait_for_function("[...document.querySelectorAll('meta[name=theme-color]')].every(m => m.content === '#1a1814')")
    phone.evaluate("document.documentElement.dataset.theme = 'light'")
    phone.wait_for_function("[...document.querySelectorAll('meta[name=theme-color]')].every(m => m.content === '#faf7f0')")
    assert phone.locator('meta[name=theme-color]').count() == 2


def test_folder_tap_folds_it_and_stays_on_the_list(phone, server):
    """A folder heading is a dropdown: tapping it folds the folder, it does not leave the list."""
    phone.request.post(f"{server}/api/notes", data={"path": "fieldwork/kestrel-survey.md", "body": "# Kestrel survey\n"})
    phone.request.post(f"{server}/api/notes", data={"path": "agent-findings.md", "body": "# Agent findings\n"},
                       headers={"X-Grimoire-Agent": "e2e-agent"})
    phone.reload()
    phone.wait_for_selector("body[data-ready]")
    mine, agents = phone.locator("#section-mine"), phone.locator("#section-agents")
    expect(mine.locator(".note-row", has_text="Kestrel survey")).to_have_count(1)
    expect(agents.locator(".note-row", has_text="Agent findings")).to_have_count(1)
    folder = mine.locator("details.folder").filter(has=phone.locator("summary.folder-head", has_text="fieldwork"))
    head = folder.locator("summary.folder-head")
    head.scroll_into_view_if_needed()
    head.tap()
    expect(phone.locator("#tabbar")).to_be_visible()
    assert folder.evaluate("e => e.open") is False
    head.tap()
    assert folder.evaluate("e => e.open") is True
    agents.locator("summary.section-head").tap()
    expect(phone.locator("#tabbar")).to_be_visible()
    folder.locator(".note-row", has_text="Kestrel survey").tap()
    expect(phone.locator("#title")).to_have_value("Kestrel survey")
