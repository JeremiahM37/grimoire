"""Graph view: the glow renderer really paints, and the controls around it behave on desktop and phone."""
import base64

import pytest
from conftest import DESKTOP, PHONE
from playwright.sync_api import expect


def _graph(page, server, current=None, second="beta"):
    """Two folders of twelve notes, each a chain, joined by one link. Served by interception: the vault is untouched."""
    ids = [f"alpha/n{i}.md" for i in range(12)] + [f"{second}/n{i}.md" for i in range(12)]
    edges = []
    for base in (0, 12):
        for i in range(11):
            edges += [base + i, base + i + 1]
    edges += [5, 17]
    data = {"v": 2, "ids": ids, "titles": [f"Note {i}" for i in range(24)], "t": [1_600_000_000 + i * 86400 for i in range(24)],
            "tags": [[] for _ in ids], "edges": edges, "unresolved": []}
    page.route("**/api/graph*", lambda route: route.fulfill(json=data))
    if current:
        note = {"path": current, "title": "Note 5", "body": "body", "frontmatter": {}, "backlinks": [], "links": []}
        page.route(f"**/api/notes/{current}", lambda route: route.fulfill(json=note))
    page.goto(server + ("/#" + current if current else ""))
    page.wait_for_selector("body[data-ready]")
    if current:
        expect(page.locator("#title")).to_have_value("Note 5")
    page.evaluate("document.querySelector('#graph-open').click()")
    expect(page.locator("#graph-canvas")).to_have_attribute("data-phase", "settled", timeout=10000)


def _node_position(page, index):
    """Where a note is on the stage, once it has stopped moving.

    Hiding the details column or opening the graph re-fits the camera over a few frames. A click aimed at a position
    read mid-glide lands on empty space, so wait for the same answer on consecutive frames instead of sleeping.
    """
    return page.evaluate("""index => new Promise(resolve => {
      const engine = document.getElementById('graph-canvas').engine;
      let last = null, steady = 0;
      const step = () => {
        const p = engine.screenPosition(index);
        steady = last && Math.abs(p.x - last.x) < 0.5 && Math.abs(p.y - last.y) < 0.5 ? steady + 1 : 0;
        last = p;
        if (steady >= 4) resolve({x: p.x, y: p.y}); else requestAnimationFrame(step);
      };
      requestAnimationFrame(step);
    })""", index)


def _colourful_pixels(page):
    """How many pixels of the stage are clearly coloured (not background, not grey text)."""
    shot = base64.b64encode(page.locator("#graph-canvas").screenshot()).decode()
    return page.evaluate("""async data => {
      // decoded by hand: the app's content security policy rightly refuses to fetch a data: URL
      const image = await createImageBitmap(new Blob([Uint8Array.from(atob(data), c => c.charCodeAt(0))], { type: 'image/png' }));
      const canvas = new OffscreenCanvas(image.width, image.height), context = canvas.getContext('2d');
      context.drawImage(image, 0, 0);
      const pixels = context.getImageData(0, 0, image.width, image.height).data;
      let count = 0;
      for (let i = 0; i < pixels.length; i += 4) {
        const max = Math.max(pixels[i], pixels[i + 1], pixels[i + 2]), min = Math.min(pixels[i], pixels[i + 1], pixels[i + 2]);
        if (max > 110 && max - min > 70) count++;
      }
      return count;
    }""", shot)


@pytest.mark.parametrize("theme", ["dark", "light"])
def test_glow_renderer_paints_notes_without_errors(page, server, theme):
    errors = []
    page.on("pageerror", lambda error: errors.append(str(error)))
    page.on("console", lambda message: errors.append(message.text) if message.type == "error" else None)
    page.add_init_script(f"localStorage.setItem('grimoire-theme', '{theme}')")
    _graph(page, server)
    expect(page.locator("#graph-stat")).to_have_text("24 notes · 23 links")
    # a shader that failed to compile would surface as the WebGL alert and an empty stage
    expect(page.locator(".graph-overlay[role=alert]")).to_have_count(0)
    assert _colourful_pixels(page) > 600, "the stage shows no coloured notes"
    assert not errors, errors


def test_colour_by_folder_and_reset(page, server):
    _graph(page, server)
    expect(page.locator(".graph-legend h3")).to_have_text("Clusters")
    page.click("#graph-filters-toggle")
    page.select_option("#graph-color", "folder")
    expect(page.locator(".graph-legend h3")).to_have_text("Folders")
    expect(page.locator(".graph-legend .graph-cluster")).to_have_count(2)
    expect(page.locator(".graph-legend")).to_contain_text("alpha")
    # an active filter is visible from the bar even with the drawer closed
    expect(page.locator("#graph-filters-toggle")).to_contain_text("•")
    page.click("#graph-reset")
    expect(page.locator(".graph-legend h3")).to_have_text("Clusters")
    expect(page.locator("#graph-filters-toggle")).not_to_contain_text("•")


def test_colour_by_author_separates_your_notes_from_agent_memory(page, server):
    # the second folder is agent memory, so half the vault was written by agents
    _graph(page, server, second="memory")
    page.click("#graph-filters-toggle")
    page.select_option("#graph-color", "author")
    expect(page.locator(".graph-legend h3")).to_have_text("Written by")
    rows = page.locator(".graph-legend .graph-cluster")
    expect(rows).to_have_count(2)
    expect(rows.nth(0)).to_contain_text("Your notes")
    expect(rows.nth(0)).to_contain_text("12")
    expect(rows.nth(1)).to_contain_text("Agent memory")
    swatches = rows.locator("i").evaluate_all("els => els.map(el => getComputedStyle(el).backgroundColor)")
    assert swatches[0] != swatches[1]


def test_colour_by_author_is_offered_only_when_agents_have_written(page, server):
    _graph(page, server)
    page.click("#graph-filters-toggle")
    expect(page.locator("#graph-color option[value=author]")).to_have_count(0)


def test_local_graph_reach_follows_the_open_note(page, server):
    _graph(page, server, current="alpha/n5.md")
    page.select_option("#graph-scope", "local")
    # note 5 links to 4, 6 and (across folders) 17
    expect(page.locator("#graph-stat")).to_have_text("4 notes · 3 links")
    page.click("#graph-filters-toggle")
    page.locator("#graph-filters").get_by_role("button", name="2 hops").click()
    expect(page.locator("#graph-stat")).to_have_text("8 notes · 7 links")


def test_local_scope_needs_an_open_note(page, server):
    _graph(page, server)
    assert page.locator("#graph-scope option[value=local]").is_disabled()


def test_details_panel_can_be_hidden_and_returns_on_selection(page, server):
    _graph(page, server)
    expect(page.locator("#graph-inspector")).to_be_visible()
    before = page.locator("#graph-canvas").bounding_box()["width"]
    page.click("#graph-panel-toggle")
    expect(page.locator("#graph-inspector")).to_be_hidden()
    assert page.locator("#graph-canvas").bounding_box()["width"] > before + 200
    position = _node_position(page, 5)
    box = page.locator("#graph-canvas").bounding_box()
    page.mouse.click(box["x"] + position["x"], box["y"] + position["y"])
    expect(page.locator("#graph-inspector")).to_be_visible()
    expect(page.locator("#graph-selection")).to_contain_text("Note 5")


@pytest.mark.parametrize("page", [DESKTOP, PHONE], indirect=True, ids=["desktop", "phone"])
def test_graph_stage_fills_the_panel(page, server):
    _graph(page, server)
    stage = page.locator("#graph-canvas").bounding_box()
    viewport = page.viewport_size
    # the map is the panel: it gets most of the screen, not a strip between toolbars and a legend
    assert stage["height"] >= viewport["height"] * 0.78
    assert stage["width"] >= viewport["width"] * (0.99 if viewport["width"] < 780 else 0.72)
    assert page.evaluate("document.documentElement.scrollWidth <= innerWidth")
    for control in ("#graph-search", "#graph-filters-toggle", "#graph-close", "#graph-fit"):
        box = page.locator(control).bounding_box()
        assert box and box["x"] >= 0 and box["x"] + box["width"] <= viewport["width"], control


def test_phone_details_are_a_sheet_over_the_map(page, server):
    page.set_viewport_size(PHONE)
    _graph(page, server)
    expect(page.locator("#graph-inspector")).to_be_hidden()
    stage = page.locator("#graph-canvas").bounding_box()
    position = _node_position(page, 5)
    page.mouse.click(stage["x"] + position["x"], stage["y"] + position["y"])
    expect(page.locator("#graph-inspector")).to_be_visible()
    expect(page.locator("#graph-selection")).to_contain_text("Note 5")
    after = page.locator("#graph-canvas").bounding_box()
    # the sheet covers the map instead of resizing it, so the layout under your finger does not jump
    assert abs(after["height"] - stage["height"]) < 1
    sheet = page.locator("#graph-inspector").bounding_box()
    assert sheet["height"] <= PHONE["height"] * 0.45
    # scope lives in the filter drawer on a phone
    expect(page.locator("#graph-scope")).to_be_hidden()
    page.click("#graph-filters-toggle")
    expect(page.locator("#graph-scope")).to_be_visible()


def test_reduced_motion_focus_does_not_animate(browser, server):
    context = browser.new_context(viewport=DESKTOP, reduced_motion="reduce")
    page = context.new_page()
    try:
        _graph(page, server)
        position = _node_position(page, 5)
        box = page.locator("#graph-canvas").bounding_box()
        page.mouse.click(box["x"] + position["x"], box["y"] + position["y"])
        expect(page.locator("#graph-selection")).to_contain_text("Note 5")
        page.wait_for_timeout(300)
        frames = page.evaluate("""() => new Promise(resolve => {
          const context = document.querySelector('#graph-canvas canvas.graph-focus').getContext('2d');
          const original = context.clearRect.bind(context); let count = 0;
          context.clearRect = (...args) => { count++; return original(...args); };
          setTimeout(() => { context.clearRect = original; resolve(count); }, 600);
        })""")
        assert frames == 0, f"the overlay repainted {frames} times while idle with reduced motion"
    finally:
        context.close()
