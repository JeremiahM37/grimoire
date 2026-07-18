# Grimoire Plugins

Grimoire has a small, stable plugin API. Plugins are plain ES modules — no
build step, no SDK to install. Seven first-party plugins ship in-repo
(`plugins/`): **katex** (LaTeX math), **mermaid** (diagrams), **kanban**
(boards from fences), **pomodoro** (focus timer panel), **vault-stats**
(sidebar dashboard), **journal-heatmap** (daily-note streak grid) and
**word-goal** (daily writing target). They double as reference implementations.

Don't want to start from a blank file? Run **“Create a plugin”** from the
command palette — it writes a working skeleton into your vault's `plugins/`
directory (disabled until you enable it in Settings).

## Anatomy

A plugin is a directory with a manifest and an entry module:

```
plugins/word-of-the-day/
├── plugin.json          # manifest
├── client.js            # ES-module entry
└── style.css            # optional stylesheet (declare it in the manifest)
```

```json
{
  "name": "word-of-the-day",        // must equal the directory name
  "version": "1.0.0",
  "description": "Shows a word in the sidebar",
  "client": "client.js",
  "styles": "style.css"
}
```

```js
// client.js — export `activate` (or a default function)
export function activate(grimoire) {
  grimoire.registerPanel({
    id: "wotd",
    title: "📖 Word of the day",
    render(el) { el.textContent = "petrichor"; },
  });
}
```

## Where plugins live

| Source | Path | Trust | Default |
|--------|------|-------|---------|
| Built-in | `<repo>/plugins/<name>/` | shipped with Grimoire | **enabled** |
| Vault | `<vault>/plugins/<name>/` | your own / third-party | **disabled** |

Vault plugins sync with your vault like notes do — install once, available on
every device. Because a plugin is arbitrary JavaScript running in the app's
origin, vault plugins must be enabled explicitly (Settings → Plugins), one by
one, per host. The strict CSP still applies: a plugin cannot load code or
exfiltrate to external hosts — all requests are confined to your server.

## API surface (`grimoire`)

Contributions:

| Method | What it does |
|--------|--------------|
| `registerCommand({icon, name, run})` | Adds a command to the palette and slash menu |
| `registerSlashSnippet({name, detail, insert})` | Adds a `/` snippet (string or `() => string`) |
| `registerFenceRenderer(lang, async (el, source))` | Owns ```` ```lang ```` blocks in the preview |
| `registerPreviewTransform(async (root))` | Post-processes every rendered preview |
| `registerPanel({id, title, render(el)})` | Adds a sidebar section |
| `on(event, cb)` | Events: `boot`, `note-open`, `note-save` |

Host services:

| Method | What it does |
|--------|--------------|
| `api(path, opts)` | Authenticated fetch against `/api/*` |
| `toast(msg, isError)` | Notification toast |
| `openNote(path)` | Navigate to a note |
| `getCurrentNote()` | `{path, title, body}` or `null` |
| `insertText(text)` | Insert at the cursor (live + classic editors) |
| `loadScript(rel)` / `loadStyles(rel)` / `assetUrl(rel)` | Same-origin plugin assets (lazy-load heavy vendors!) |

## Rules of the road

1. **Fail quietly.** A plugin that throws is logged and skipped — never take
   the app down. Wrap risky work in try/catch anyway.
2. **Lazy-load heavy assets.** `katex` and `mermaid` only fetch their vendored
   bundles the first time a page needs them. Do the same.
3. **Escape everything** you put into `innerHTML`. The app's renderers escape
   first — don't be the plugin that introduces XSS.
4. **Degrade on /read and export.** Plugin fences fall back to plain code
   blocks on the no-JS surfaces (e-ink, HTML export). Choose fence formats that
   read fine as text.
5. **Version your manifest** and keep `name` equal to the directory name — the
   loader refuses mismatches.

## Server-side plugins

Deliberately not supported (yet). A server plugin is arbitrary Python with
filesystem access — a much bigger trust decision than sandboxed-by-CSP client
JS. The MCP server + REST API cover most automation needs; open an issue with
a use case if you hit a wall.
