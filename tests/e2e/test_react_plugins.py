"""React plugin-host contract tests beyond the built-in plugin smoke tests."""

import json
import re
import shutil
from pathlib import Path

import conftest
from playwright.sync_api import expect


def _install(page, name):
    plugin = f'''
export function activate(g) {{
  let panel, saves = 0;
  g.registerPanel({{id: '{name}', title: 'Bridge {name}', render(el) {{
    panel = el; el.textContent = 'ready';
  }}}});
  g.on('note-save', note => {{
    saves += 1;
    if (panel) panel.textContent = `saved ${{saves}}: ${{note.title}}`;
  }});
  g.registerCommand({{name: 'Bridge {name} insert', run: () => {{
    const note = g.getCurrentNote();
    if (!note || note.title !== 'Bridge Probe') throw new Error('wrong current note');
    g.insertText(` [bridge:${{note.title}}]`);
  }}}});
  g.registerCommand({{name: 'Bridge {name} toast', run: () => g.toast('bridge toast works')}});
}}
'''
    created = page.evaluate(
        """async name => {
          const response = await fetch('/api/plugins/scaffold', {
            method: 'POST', headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({name}),
          });
          return {ok: response.ok, text: await response.text()};
        }""",
        name,
    )
    assert created["ok"], created["text"]
    (Path(conftest.VAULT) / "plugins" / name / "client.js").write_text(plugin)
    enabled = page.evaluate(
        """async name => (await fetch(`/api/plugins/${name}/enable`, {
          method: 'POST', headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({enabled: true}),
        })).ok""",
        name,
    )
    assert enabled


def _remove(name):
    vault = Path(conftest.VAULT)
    shutil.rmtree(vault / "plugins" / name, ignore_errors=True)
    state_path = vault / ".grimoire" / "plugins.json"
    if state_path.exists():
        state = json.loads(state_path.read_text())
        state.pop(name, None)
        state_path.write_text(json.dumps(state, indent=2) + "\n")


def _new_note(page):
    page.click("#new-note")
    page.fill("#new-note-title", "Bridge Probe")
    page.click("#new-note-create")
    expect(page.locator("#title")).to_have_value("Bridge Probe")


def _run_palette(page, command):
    page.keyboard.press("Control+k")
    page.fill("#palette-input", command)
    page.keyboard.press("Enter")


def test_react_plugin_bridge_classic(server, page):
    name = "react-bridge-classic"
    page.goto(server)
    page.wait_for_selector("body[data-ready]")
    _install(page, name)
    try:
        page.reload()
        page.wait_for_selector("body[data-ready]")
        expect(page.locator(".plugin-panel", has_text=f"Bridge {name}")).to_be_visible()
        _new_note(page)
        _run_palette(page, f"bridge {name} insert")
        expect(page.locator("#content")).to_have_value(re.compile(r"\[bridge:Bridge Probe\]"), timeout=6000)
        expect(page.locator(".plugin-panel", has_text="saved 1: Bridge Probe")).to_be_visible(timeout=6000)
        _run_palette(page, f"bridge {name} toast")
        expect(page.locator("[role=alert]")).to_contain_text("bridge toast works")
        page.keyboard.press("Control+k")
        page.fill("#palette-input", f"bridge {name}")
        expect(page.locator("#palette-list .pal-item", has_text=f"Bridge {name} insert")).to_have_count(1)
        expect(page.locator("#palette-list .pal-item", has_text=f"Bridge {name} toast")).to_have_count(1)
    finally:
        _remove(name)


def test_react_plugin_bridge_live(server, live_page):
    name = "react-bridge-live"
    live_page.goto(server)
    live_page.wait_for_selector("body[data-ready]")
    _install(live_page, name)
    try:
        live_page.reload()
        live_page.wait_for_selector("body[data-ready]")
        _new_note(live_page)
        _run_palette(live_page, f"bridge {name} insert")
        expect(live_page.locator("#live-editor")).to_contain_text("[bridge:Bridge Probe]", timeout=6000)
        expect(live_page.locator(".plugin-panel", has_text="saved 1: Bridge Probe")).to_be_visible(timeout=6000)
    finally:
        _remove(name)
