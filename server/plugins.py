"""Plugin discovery + lifecycle.

Two plugin sources, with different trust levels:

* **Built-in** — `<repo>/plugins/<name>/`, shipped with Grimoire. Trusted code;
  the on-topic ones (see `DEFAULT_ENABLED_BUILTINS`) are enabled by default, the
  rest ship one toggle away. Each can be enabled/disabled from Settings.
* **Vault** — `<vault>/plugins/<name>/`, user-installed and synced with the
  vault like any other content. Because a vault plugin is arbitrary JavaScript
  executed in the app's origin, vault plugins are **disabled by default** and
  must be enabled explicitly, one by one, from Settings. The UI shows a
  warning when enabling one.

A plugin is a directory with a `plugin.json` manifest:

    {
      "name": "kanban",                  // must match the directory name
      "version": "1.0.0",
      "description": "Kanban boards from ```kanban fences",
      "client": "client.js",             // entry, loaded as an ES module
      "styles": "style.css"              // optional stylesheet
    }

Enablement state lives in `.grimoire/plugins.json` (vault-local, not synced
content — each device/host decides what runs). Asset serving is path-confined
to the plugin's own directory.
"""
from __future__ import annotations

import json
import re
from pathlib import Path

from . import config

BUILTIN_DIR = config.ROOT / "plugins"
_NAME_RE = re.compile(r"^[a-z][a-z0-9-]{0,40}$")

# Which built-ins are ON out of the box. The bar: does it make the *notes /
# knowledge* richer? Content renderers (math, diagrams, boards) are invisible
# until you use their syntax, and vault-stats reports on the vault itself — all
# on-topic. The productivity widgets (pomodoro timer, writing-streak heatmap,
# daily word goal) are genuinely useful but off-topic sidebar furniture; they
# ship enabled-able, not enabled, so a fresh vault looks like a focused tool
# rather than a kitchen sink. Everything here is still one toggle away.
DEFAULT_ENABLED_BUILTINS = {"katex", "mermaid", "kanban", "vault-stats"}


def _state_path() -> Path:
    return config.grimoire_dir() / "plugins.json"


def _load_state() -> dict:
    try:
        return json.loads(_state_path().read_text(encoding="utf-8"))
    except Exception:
        return {}


def _save_state(state: dict) -> None:
    _state_path().parent.mkdir(parents=True, exist_ok=True)
    _state_path().write_text(json.dumps(state, indent=2), encoding="utf-8")


def vault_dir() -> Path:
    return config.VAULT / "plugins"


def _read_manifest(pdir: Path, source: str) -> dict | None:
    """Load + validate one plugin's manifest. Returns None for anything broken —
    a malformed plugin must never take the app down."""
    mf = pdir / "plugin.json"
    if not mf.is_file():
        return None
    try:
        data = json.loads(mf.read_text(encoding="utf-8"))
    except Exception:
        return None
    name = data.get("name", "")
    if name != pdir.name or not _NAME_RE.match(name):
        return None                      # manifest must match its directory
    client = data.get("client", "client.js")
    if not (pdir / client).is_file():
        return None
    return {
        "name": name,
        "version": str(data.get("version", "0.0.0")),
        "description": str(data.get("description", "")),
        "source": source,
        "client": client,
        "styles": data.get("styles") if isinstance(data.get("styles"), str) else None,
    }


def discover() -> list[dict]:
    """All plugins from both sources with their effective enabled state.
    Built-ins default to enabled; vault plugins default to DISABLED (untrusted)."""
    state = _load_state()
    out: list[dict] = []
    seen: set[str] = set()
    for source, root in (("builtin", BUILTIN_DIR), ("vault", vault_dir())):
        if not root.is_dir():
            continue
        for pdir in sorted(root.iterdir()):
            if not pdir.is_dir() or pdir.name in seen:
                continue                 # builtin name wins over a vault clone
            m = _read_manifest(pdir, source)
            if not m:
                continue
            default_on = source == "builtin" and m["name"] in DEFAULT_ENABLED_BUILTINS
            m["enabled"] = bool(state.get(m["name"], {}).get("enabled", default_on))
            out.append(m)
            seen.add(pdir.name)
    return out


def set_enabled(name: str, enabled: bool) -> dict | None:
    """Persist enablement. Returns the plugin entry, or None if unknown."""
    plugin = next((p for p in discover() if p["name"] == name), None)
    if plugin is None:
        return None
    state = _load_state()
    state.setdefault(name, {})["enabled"] = enabled
    _save_state(state)
    plugin["enabled"] = enabled
    return plugin


def asset_path(name: str, rel: str) -> Path | None:
    """Resolve a plugin asset path, confined to that plugin's directory.
    Only enabled plugins serve assets (a disabled plugin's code never loads)."""
    plugin = next((p for p in discover() if p["name"] == name), None)
    if plugin is None or not plugin["enabled"]:
        return None
    root = (BUILTIN_DIR if plugin["source"] == "builtin" else vault_dir()) / name
    try:
        p = (root / rel).resolve()
        p.relative_to(root.resolve())    # raises ValueError on traversal
    except ValueError:
        return None
    return p if p.is_file() else None

SCAFFOLD_CLIENT = """/**
 * {name} — a Grimoire plugin.
 *
 * This skeleton was generated by "Create a plugin". It registers one palette
 * command and one sidebar panel; delete what you don't need. Full API docs:
 * docs/PLUGINS.md in the Grimoire repo.
 *
 * Enable it under Settings → Plugins (vault plugins are off until you opt in).
 */
export function activate(grimoire) {{
  grimoire.registerCommand({{
    icon: "🔌",
    name: "{name}: hello",
    run: () => grimoire.toast("Hello from {name}!"),
  }});

  grimoire.registerPanel({{
    id: "{name}",
    title: "🔌 {name}",
    render(el) {{
      el.textContent = "Edit plugins/{name}/client.js in your vault to build me.";
    }},
  }});
}}
"""


def scaffold(name: str) -> dict:
    """Write a hello-world vault plugin skeleton. Stays DISABLED until the user
    enables it in Settings — scaffolding must not grant execution by itself."""
    if not _NAME_RE.match(name):
        raise ValueError("plugin name must be lowercase letters/digits/hyphens")
    if any(p["name"] == name for p in discover()):
        raise ValueError(f"a plugin named {name!r} already exists")
    pdir = vault_dir() / name
    pdir.mkdir(parents=True, exist_ok=False)
    (pdir / "plugin.json").write_text(json.dumps({
        "name": name, "version": "0.1.0",
        "description": "My Grimoire plugin (edit plugin.json and client.js)",
        "client": "client.js",
    }, indent=2), encoding="utf-8")
    (pdir / "client.js").write_text(SCAFFOLD_CLIENT.format(name=name), encoding="utf-8")
    return {"name": name, "path": f"plugins/{name}", "enabled": False}
