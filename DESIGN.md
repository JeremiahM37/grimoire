# mnemo — Design Document

> **Working name.** `mnemo` (from Mnemosyne, memory) is a placeholder — final name decided before publishing.
> **One-liner:** A local-first, AI-native notes app in the Obsidian/SilverBullet class — plain-markdown, wiki-linked, synced everywhere — with a built-in **encrypted secret vault** your AI can use, scoped and audited.

Status: v0.1–v0.9 shipped · Started 2026-07-16 · Lives at `/home/admin/projects/mnemo/`

---

## 1. Vision & why it's different

There are excellent notes apps. Obsidian nails local-first plain-markdown + links + plugins. SilverBullet nails self-hosted web + code-runnable notes. Neither is *AI-native*, and none treats **your secrets as a first-class, AI-usable resource**.

mnemo is built on three bets nobody else combines:

1. **AI-native, not AI-bolted-on.** The AI reads, writes, links, and searches notes as first-class operations — over MCP — so mnemo works with Claude Code, claude.ai, the homelab agents, or any MCP client. "Ask your notes" is a core surface, not a plugin. Notes and AI share one substrate.
2. **A secure secret vault your AI can actually use.** mnemo doubles as an encrypted store for AI agent tokens, API keys, and MCP server credentials — and can hand a *scoped, audited, time-boxed* secret to an agent that needs to call a service or MCP. This is the unlock: your knowledge base becomes the trusted broker between your notes, your AI, and the services it drives. No other notes app does this.
3. **Truly everywhere, truly yours.** Local-first, plain `.md` files (zero lock-in, git-friendly), CRDT-ready sync across phone/tablet/desktop, plus read surfaces for constrained devices (Kindle/e-ink) via static export. Offline by default; your data is a folder you own.

### Non-goals
Not a cloud SaaS. Not a proprietary format (everything is markdown + a rebuildable index). Not a plugin marketplace at launch. Not a team/multiplayer product first — single-user, multi-device is the target; collaboration is a later CRDT payoff.

---

## 2. Competitive lens

| Capability | Obsidian | SilverBullet | Notion | Apple Notes | **mnemo** |
|---|---|---|---|---|---|
| Plain-markdown, no lock-in | ✅ | ✅ | ❌ | ❌ | ✅ |
| Self-hosted / local-first | ✅ (local) | ✅ (server) | ❌ | ❌ | ✅ both |
| Wiki-links + backlinks + graph | ✅ | ✅ | ⚠️ | ❌ | ✅ |
| Local full-text search | ✅ | ✅ | ✅ | ✅ | ✅ (FTS5) |
| **Ask-your-notes (RAG)** | ⚠️ plugin | ❌ | ⚠️ | ❌ | ✅ core |
| **AI-native (MCP in + out)** | ❌ | ❌ | ⚠️ | ❌ | ✅ |
| **Encrypted secret vault + AI-scoped use** | ❌ | ❌ | ❌ | ❌ | ✅ **unique** |
| Multi-device sync incl. e-ink | 💲 paid | ⚠️ | ✅ | ✅ | ✅ |
| Audio memos + transcription | ⚠️ plugin | ❌ | ⚠️ | ✅ | ✅ (local whisper) |
| Browser capture | ⚠️ plugin | ❌ | ✅ web clipper | ⚠️ | ✅ |
| CLI / scriptable | ⚠️ | ⚠️ | ❌ | ❌ | ✅ |
| Open source | ❌ | ✅ | ❌ | ❌ | ✅ (OSP) |

---

## 3. Core concepts & vocabulary

- **Vault** — a directory of markdown files you own. The source of truth. `vault/` holds notes; `vault/.mnemo/` holds the rebuildable index, config, and encrypted secret store (git-ignorable). One mnemo instance serves one vault (multi-vault later).
- **Note** — one `.md` file. YAML frontmatter (id, title, tags, created, updated, `private: true`, aliases) + markdown body. The file *is* the note; the DB is a cache.
- **Link** — `[[wiki link]]` (by title/alias/id), `#tags`, and standard markdown links. Backlinks are derived. Unresolved links are first-class (they mark notes worth creating).
- **Daily note** — `journal/YYYY-MM-DD.md`, one tap/command away, templated.
- **Index** — SQLite (FTS5 for search) + a link graph + optional vector table (embeddings) for RAG. Fully rebuildable from the vault; never authoritative.
- **Secret** — an encrypted credential (API key, token, MCP server config) in the vault's sealed store. Referenced from notes by handle (`{{secret:openai}}`) but never rendered in plaintext. Usable by AI only through a scoped, audited grant.
- **Grant** — a time-boxed, scope-limited authorization for an agent/session to *use* (not read) a secret — e.g. "let this session call the GitHub MCP with the `gh-readonly` token for 30 min." Every use is logged.
- **Capture** — an inbound note from outside the app: browser clip, audio memo, CLI, share-sheet, email-in (later).

---

## 4. Architecture

```
┌ clients ─────────────────────────────────────────────────────────┐
│  PWA (phone/tablet/desktop)   CLI   browser ext   e-ink export    │
│  MCP clients (Claude Code, claude.ai, homelab agents)             │
└───────────────┬───────────────────────────────────────────────────┘
        HTTPS · MCP (stdio/SSE) · sync protocol
┌───────────────┴───────────────────────────────────────────────────┐
│  mnemo server (FastAPI, self-hosted)                               │
│  ┌ notes API ┐ ┌ search/RAG ┐ ┌ secret vault ┐ ┌ sync ┐ ┌ MCP ┐   │
│  │ CRUD·links│ │ FTS5+vector│ │ sealed store │ │deltas│ │in/out│   │
│  └─────┬─────┘ └─────┬──────┘ └──────┬───────┘ └──┬───┘ └──┬───┘   │
│        │  index (SQLite: notes, links, fts, vectors, secrets,      │
│        │         grants, audit, sync_state, devices)               │
│  ┌─────┴──────────────────────────────────────────────────────┐   │
│  │  Vault watcher: file ⇄ index reconciler (fs is source of    │   │
│  │  truth; edits from any surface land as .md, re-indexed)     │   │
│  └────────────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────────────┘
                         vault/  (plain .md + .mnemo/)
```

### 4.1 Storage (plain files, rebuildable index)
- **Files are truth.** Every note is a `.md` on disk. Edits from the PWA, CLI, AI, or a text editor all converge on the file. A **vault watcher** (watchdog/inotify + a debounced reconciler) keeps the index in sync; the index can always be dropped and rebuilt (`mnemo reindex`).
- **Index = SQLite**, one file in `.mnemo/index.db`: `notes` (path, id, title, frontmatter, mtime, hash), `links` (src→dst, resolved/unresolved), `tags`, `fts` (FTS5 over title+body), `vectors` (embeddings, optional), plus vault-ops tables (secrets/grants/audit/sync/devices). Mirrors the proven homelab doc-rag pattern.
- **No lock-in:** point Obsidian or git at the same `vault/` and it just works.

### 4.2 Editor & frontend
- **PWA, mobile-first**, installable, offline (service worker + IndexedDB cache of recent notes + a write queue that syncs when back online). Same conventions as the homelab PWAs.
- **Editor: CodeMirror 6** — live markdown, `[[` autocomplete against titles/aliases, inline preview, tag/link decorations. SilverBullet-quality editing without its runtime.
- Surfaces: editor, daily note, backlinks pane, graph, search, "ask", capture inbox, secret vault (locked), settings.

### 4.3 Search & "ask your notes" (RAG)
- **Local search:** SQLite FTS5 — instant, offline, ranked, with tag/path filters. No network.
- **Ask your notes:** retrieval over the vector table → answer with citations (which notes). Pluggable model backend: **local Ollama** (default, private) or Claude (via a vault secret). Reuses the homelab's doc-rag + Ollama-native-Anthropic-endpoint knowledge. Private notes are excluded from RAG unless explicitly opted in per query.

### 4.4 Secret vault (the differentiator)
- **At rest:** secrets encrypted with a master key derived from a passphrase (Argon2id → key; libsodium/`age` sealed boxes). The `.mnemo/secrets.age` blob is useless without the passphrase; the passphrase is never stored. Vault unlocks per-session (kept in memory only).
- **In notes:** reference by handle `{{secret:name}}` — renders as `••••` in the UI, never as plaintext, never indexed, never in RAG context.
- **AI use, not AI read:** an agent never receives a raw secret. It requests a **grant** ("use `gh-token` against the GitHub MCP"); mnemo brokers the call or injects the secret into a scoped subprocess/MCP session, time-boxed, and logs it to the **audit** table. Revocable. This makes mnemo the trusted secret broker between your notes and your AI — the thing that lets an agent actually *do* things safely.
- Threat model documented explicitly (§10): what a compromised session can and cannot reach.

### 4.5 Sync (everywhere, incl. e-ink)
- **Local-first.** Every device has a full or partial vault copy; the server is a sync hub, not a gatekeeper.
- **v1 sync:** delta protocol — client sends `{path, hash, mtime}` manifest; server replies with adds/updates/deletes; content transferred for changed files; **divergence → conflict copy** (`note (conflict 2026-07-16 device).md`) never silent loss. Simple, robust, Syncthing-grade.
- **v2 sync:** per-note **CRDT** (Yjs/automerge text) for conflict-free concurrent edits — the real payoff, designed-for now (notes carry a stable `id`; edits are ops).
- **e-ink / Kindle:** a **static read-only export** (`mnemo export --static`) — plain HTML, no JS, huge-font-friendly, hyperlinked — served at a URL the Kindle/e-reader browser can open. Read-mostly devices get the whole graph without needing the PWA.
- **Devices table** tracks each client for sync state + per-device revoke.

### 4.6 AI-native surface (MCP in and out)
- **mnemo as MCP server:** exposes tools — `search_notes`, `read_note`, `write_note`, `append_daily`, `list_backlinks`, `ask_notes`, `create_note`, `link_notes`, `list_tags`. Any MCP client (Claude Code, claude.ai bridge, homelab Discord bot, agentdeck agents) can read/query/write your notes. This is how "easily incorporates AI" is delivered — not a chat box, a protocol.
- **mnemo as MCP client / secret broker:** using vault secrets + grants, mnemo can call *other* MCP servers or services on the AI's behalf (scoped, audited).
- Inline AI actions in the editor (summarize, expand, link-suggest, tag-suggest) route through the same backend.

### 4.7 Capture
- **CLI** (`mnemo`): `new`, `daily`, `capture -`, `search`, `ask`, `open`, `reindex`, `export`, `secret {add,use,ls}`, `sync`, `serve`, `mcp`. Scriptable; pipes to daily/inbox.
- **Browser extension / bookmarklet:** POST selection+URL+title → `/api/capture` → inbox note with source metadata.
- **Audio memos:** record in PWA → upload → **local whisper** transcription → note with audio attachment + transcript (reuses homelab GPU/whisper).
- **Share-sheet** (PWA share target) and **email-in** (later).

---

## 5. Data model (SQLite index, all rebuildable except vault-ops)

```sql
notes(id, path, title, frontmatter_json, mtime, hash, private, created, updated)
links(src_id, dst_ref, dst_id NULL, kind /*wiki|md|tag*/, resolved)
tags(note_id, tag)
fts USING fts5(title, body, content=notes)          -- local search
vectors(note_id, chunk, embedding BLOB)              -- RAG (optional backend)
attachments(id, note_id, kind /*audio|image|file*/, path, meta_json)
-- vault-ops (NOT rebuildable from files — the authoritative store for these)
secrets(name, ciphertext BLOB, meta_json, created)   -- sealed; key never stored
grants(id, secret_name, grantee /*session/agent*/, scope, expires_at, created)
audit(id, ts, actor, action, secret_name NULL, note_id NULL, detail)
devices(id, name, last_sync, sync_cursor)
sync_state(device_id, path, hash, mtime)
```

## 6. API surface (v1 sketch)

```
GET/POST/PUT/DELETE /api/notes[/{id}]     # CRUD; PUT writes the .md file
GET  /api/notes/{id}/backlinks
GET  /api/search?q=&tag=&path=            # FTS5
POST /api/ask            {q, include_private?}   # RAG answer + citations
GET  /api/daily          # today's note (create if absent)
POST /api/capture        {text, url?, title?, source}
POST /api/audio          # upload → transcribe → note
GET  /api/graph          # nodes+edges for the graph view
POST /api/vault/unlock   {passphrase}      # session-scoped key in memory
CRUD /api/secrets        # names + meta only; ciphertext never returned
POST /api/secrets/{name}/grant   {grantee, scope, ttl}
POST /api/sync/manifest  ·  POST /api/sync/pull  ·  POST /api/sync/push
GET  /api/audit
# MCP served separately (stdio + SSE) via server/mcp_server.py
```

## 7. Test strategy (the user asked for depth — this is a first-class column)

Four kinds, all hermetic by default (temp vault, no network, local stub embedder):

1. **Unit** — markdown parse/frontmatter, wiki-link resolution + backlinks, FTS ranking, secret seal/unseal (round-trip + wrong-passphrase), grant expiry, sync delta computation, export renderer.
2. **API** — every endpoint against a temp vault; note CRUD writes real files; ask-notes with a stub retriever; secret grant lifecycle; capture/audio (mock transcriber).
3. **E2E (Playwright)** — real browser: create/edit a note, `[[` autocomplete + backlink appears, daily note, search, ask, lock/unlock vault, capture inbox, offline write → reconnect → sync. Phone + desktop viewports.
4. **Regression** — every fixed bug gets a red-green test (fails before, passes after) — the discipline proven on agentdeck.
5. **Negative / adversarial** — malformed frontmatter, path-traversal in note paths (`../`), oversized uploads, wrong passphrase, expired/over-scope grant denied, secret never appears in search/RAG/API responses, sync conflict produces a conflict copy (never silent loss), injection in `[[links]]`/titles, unauthorized secret read → 403. Security assertions are tests, not hopes.

`.verify.yaml` wires unit+API+e2e + a real headless UI flow (create→link→search→ask), per the homelab house rule.

## 8. Tech stack
- **Backend:** Python 3.12+, FastAPI + uvicorn, SQLite (FTS5 + optional sqlite-vec), watchdog (vault watcher), libsodium/`age` or `cryptography` (Fernet+Argon2) for the vault, httpx.
- **Frontend:** PWA, CodeMirror 6, vanilla ES modules (no heavy build), service worker + IndexedDB. Homelab-PWA conventions.
- **AI:** local **Ollama** default (private RAG + embeddings via `nomic-embed-text`, already in the homelab), Claude optional via a vault secret. Reuses doc-rag learnings.
- **CLI:** a single `mnemo` entrypoint (argparse/click).
- **Install:** `pipx install mnemo` → `mnemo serve --vault ~/notes`; Docker image; systemd unit. "Easy install" is a design constraint, tested.

## 9. Roadmap
Status: **v0.1–v0.9 shipped** (2026-07-16) — deployed, 160 hermetic tests + Playwright e2e + `verify` 3/3.
- **v0.1 (core) ✅:** vault + watcher/reindex, note CRUD (files ⇄ index), frontmatter, `[[wiki-links]]` + backlinks, tags, daily notes, FTS5 search, PWA editor, CLI, hermetic test suite + `.verify.yaml`.
- **v0.2 (AI) ✅:** embeddings + ask-your-notes (auto-Ollama, else offline extractive), mnemo-as-MCP-server, inline AI actions, private-notes exclusion.
- **v0.3 (secrets) ✅:** encrypted vault, grants + audit, AI secret-broker (USE-not-READ).
- **v0.4 (capture) ✅:** browser extension, audio memos + whisper, share target, CLI capture.
- **v0.5 (sync) ✅:** delta sync + conflict copies, live cross-device refresh, static e-ink export.
- **v0.6–v0.9 ✅ (best-in-class):** tag browsing, graph view, task checkboxes, command palette (Ctrl-K), real editor (toolbar/smart-lists/tab), image/file attachments, theme toggle, outline/TOC, note templates, per-note HTML export, in-app settings, **encryption-at-rest for private notes**, soft-delete/trash + undo, aliases, word count, pin/favorite, calendar.
- **v1.0 (todo):** CRDT sync, threat-model doc, rename off the "mnemo" placeholder, publish (OSP). Niche backlog: kanban board, split-view, version history (partly covered by the git-friendly plain-md vault).

## 10. Risks & threat model (sketch — expanded per phase)
- **Vault brokering is the crown jewel and the biggest risk.** A compromised unlocked session could request grants. Mitigations: grants are scoped + time-boxed + revocable + audited; secrets never leave the process as plaintext to the client; per-secret allow-lists of which MCP/service a token may be used against; a "panic lock" that drops the in-memory key. Default-deny.
- **fs ⇄ index races** (external edit mid-index) → hash+mtime reconciliation, atomic writes (temp+rename), debounce; index is disposable.
- **Sync data loss** → never silent overwrite; conflict copies; content-hash verification; local-first means the device always has its own copy.
- **RAG leaking private notes** → private excluded from vectors by default; explicit per-query opt-in; tests assert non-leakage.
- **Path traversal / injection** → note paths sandboxed to the vault; titles/links escaped; negative tests.
- **Model/endpoint drift** (Ollama/Claude) → backend isolated behind one interface; degrade gracefully when AI is unavailable (search/edit still work fully offline).
