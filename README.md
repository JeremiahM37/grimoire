# mnemo

> Local-first, **AI-native** notes — Obsidian/SilverBullet class, plain-markdown,
> wiki-linked — with an encrypted secret vault your AI can *use* but never *read*.
> Working name; see `DESIGN.md` for the full design & threat model.

Your notes are just `.md` files in a folder you own. mnemo adds a fast, fully
rebuildable index on top — wiki-links, backlinks, search, RAG, a graph — and is
AI-native to the core (it's both an MCP **server** and **client**). Nothing is
locked in: point Obsidian, vim, git, or Syncthing at the same folder and mnemo's
live watcher keeps up.

## What makes it different

1. **AI-native, not bolted on.** Notes are searchable/writable over MCP; an
   `ask-your-notes` RAG endpoint answers with citations. Uses a local Ollama when
   reachable (generative), else a deterministic offline extractive fallback.
2. **An encrypted secret vault your AI can USE, not READ.** Store agent tokens /
   API keys / MCP creds sealed at rest; hand an agent a *scoped, time-boxed,
   audited* grant and mnemo **brokers** the call — the secret value never reaches
   the caller. No other notes app does this.
3. **Truly local-first.** Plain `.md` is the source of truth; the SQLite index is
   a cache you can delete and rebuild. Edit anywhere; the watcher reconciles.

## Features

- **Editing** — mobile-first offline PWA, formatting toolbar, smart list/task
  continuation, Tab indent, live markdown preview, `[[` autocomplete, command
  palette (Ctrl/Cmd-K), outline/TOC, word count, light/dark theme.
- **Linking** — `[[wiki-links]]` + backlinks, frontmatter **aliases**, `#tags`
  (click to filter), an interactive **graph view**, create-on-click.
- **Notes** — daily notes + a **calendar**, **templates** (`{{date}}`/`{{title}}`),
  clickable **task checkboxes**, **pin/favorite**, image/file **attachments**
  (paste / drag-drop / `![[embed]]`), capture inbox.
- **AI** — ask-your-notes (RAG + citations), inline summarize/expand/tag/title,
  audio memos (whisper), browser capture (extension + bookmarklet + share target).
- **Security & safety** — the secret vault + broker; **encryption-at-rest** for
  private notes; **soft-delete/trash + undo**; private notes excluded from AI.
- **Sync & reach** — delta sync with conflict copies (never silent data loss),
  live cross-device refresh, an e-ink/Kindle `/read` surface, self-contained
  **HTML export** (print-to-PDF), a scriptable **CLI**, and an **MCP server**.

## Run

```bash
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
MNEMO_VAULT=~/notes .venv/bin/python -m server      # http://<host>:9111
```

Optional AI: set `MNEMO_OLLAMA_URL` (auto-enables generative answers) — or
configure it in-app under ⚙ Settings. Everything works fully offline without it.

## CLI

```bash
python cli/mnemo.py new "Title" "body..."     # or pipe: echo hi | mnemo capture
python cli/mnemo.py daily "a log line"
python cli/mnemo.py search QUERY
python cli/mnemo.py export note.md            # standalone HTML
python cli/mnemo.py mcp                        # run as an MCP server
python cli/mnemo.py serve --port 9111
```

## MCP

mnemo is an MCP server (`server/mcp_server.py`, FastMCP) exposing search / ask /
read / list / create / update / append-daily / backlinks / tags — so any MCP
client (Claude, etc.) can work with your notes directly.

## Tests

```bash
.venv/bin/pytest                 # 160 hermetic tests: unit + api + negative + e2e
verify run .verify.yaml          # live api + browser smoke (isolated port 9119)
```

Playwright e2e covers graph, palette, editor, templates, export, settings,
encryption, trash/undo, aliases, pin, calendar — plus the core flows.

## Layout

```
server/            FastAPI + SQLite index; vault ⇄ index reconciler; watcher
server/render.py   server-side markdown → HTML (e-ink + export)
server/crypto.py   secret-vault + note encryption (PBKDF2 + Fernet)
server/routers/    notes · search · daily/capture · ask · secrets · media ·
                   sync · read · templates · settings · misc
server/mcp_server.py   MCP server (9 tools)
web/               mobile-first PWA (no build step)
cli/mnemo.py       scriptable CLI
tests/             unit / api (+ negative) / integration / e2e
DESIGN.md          vision, architecture, roadmap, threat model
```
