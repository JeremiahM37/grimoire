"""Plugin runtime in a real browser: builtin plugins activate, fence renderers
own their blocks, panels mount in the sidebar, KaTeX renders math offline."""
from playwright.sync_api import expect


def _new_note(pg, title):
    pg.wait_for_selector("body[data-ready]", timeout=10000)   # app fully booted
    pg.once("dialog", lambda d: d.accept(title))
    pg.click("#new-note")
    expect(pg.locator("#title")).to_have_value(title, timeout=8000)


def test_builtin_plugins_load_and_panels_mount(page, server):
    page.goto(server)
    # vault-stats + pomodoro register sidebar panels on boot
    page.wait_for_selector("body[data-ready]", timeout=10000)
    expect(page.locator(".plugin-panel", has_text="Vault")).to_be_visible(timeout=8000)
    expect(page.locator(".plugin-panel", has_text="Pomodoro")).to_be_visible()


def test_kanban_fence_renders_board(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(page, "Board Note")
    page.fill("#content",
              "```kanban\n## Todo\n- write docs\n## Done\n- ship it\n```")
    page.click("#preview-toggle")
    expect(page.locator("#preview .kb-col-title", has_text="Todo")).to_be_visible(timeout=8000)
    expect(page.locator("#preview .kb-card", has_text="ship it")).to_be_visible()


def test_katex_renders_math_offline(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    _new_note(page, "Math Note")
    page.fill("#content", "```math\nc = \\sqrt{a^2 + b^2}\n```")
    page.click("#preview-toggle")
    # KaTeX output appears (vendored assets — no network beyond our origin)
    expect(page.locator("#preview .katex").first).to_be_visible(timeout=10000)


def test_pomodoro_command_in_palette(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    expect(page.locator(".plugin-panel", has_text="Pomodoro")).to_be_visible(timeout=8000)
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "pomodoro")
    expect(page.locator("#palette .pal-item", has_text="pomodoro")).to_be_visible(timeout=5000)


def test_new_builtin_panels_mount(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    expect(page.locator(".plugin-panel", has_text="Journal")).to_be_visible(timeout=8000)
    expect(page.locator(".plugin-panel", has_text="Today's goal")).to_be_visible()
    # heatmap grid renders cells
    expect(page.locator(".jh-grid .jh-cell").first).to_be_visible()


def test_plugin_scaffold_via_palette(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    page.once("dialog", lambda d: d.accept("my-first-plugin"))
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "create a plugin")
    page.keyboard.press("Enter")
    # skeleton exists and is listed as a disabled vault plugin
    page.wait_for_function(
        "async () => (await (await fetch('/api/plugins')).json())"
        ".some(p => p.name === 'my-first-plugin' && p.source === 'vault' && !p.enabled)",
        timeout=8000)
