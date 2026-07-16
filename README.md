# mnemo

> Local-first, **AI-native** notes — Obsidian/SilverBullet class, plain-markdown,
> wiki-linked — with (coming) an encrypted secret vault your AI can use.
> Working name; see `DESIGN.md` for the full design & roadmap.

Your notes are just `.md` files in a folder you own. mnemo adds a fast index,
wiki-links + backlinks, daily notes, local full-text search, a mobile PWA editor,
and a CLI — and is being built AI-native (MCP in/out, ask-your-notes) with a
secure token vault so your AI can actually *do* things with your credentials.

## Status — v0.1 (core)

Working today: vault (plain `.md` + rebuildable SQLite index), note CRUD
(files ⇄ index), `[[wiki-links]]` + backlinks, `#tags`, daily notes, capture
inbox, FTS5 local search, a mobile-first offline PWA editor (live preview,
`[[` autocomplete, wiki-link navigation), and a CLI. 41 hermetic tests
(unit + API + **negative/adversarial** + Playwright e2e). Next: AI (RAG + MCP),
then the secret vault, then capture (audio/browser), then sync.

## Run

```bash
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
MNEMO_VAULT=~/notes .venv/bin/python -m server      # http://<host>:9111
```

## CLI

```bash
python cli/mnemo.py new "Title" "body..."     # or pipe: echo hi | mnemo capture
python cli/mnemo.py daily "a log line"
python cli/mnemo.py search QUERY
python cli/mnemo.py serve --port 9111
```

## Tests

```bash
.venv/bin/pytest                 # unit + api + negative + Playwright e2e (hermetic)
```

## Layout

```
server/          FastAPI + SQLite index; vault ⇄ index reconciler
server/routers/  notes · search · daily/capture · misc
web/             mobile-first PWA editor (no build step)
cli/mnemo.py     scriptable CLI
tests/           unit / api (+ negative) / e2e
DESIGN.md        vision, architecture, roadmap, threat model
```
