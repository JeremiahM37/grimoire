# Grimoire Notes — Design Document

> **Name:** **Grimoire Notes** (product name). The codebase still uses the original `grimoire` codename internally (the `server` package, the `GRIMOIRE_` env prefix, the systemd service, and the `/home/admin/projects/grimoire` path) — user-facing surfaces all say Grimoire.
> **One-liner:** A **self-hosted personal context server** — one knowledge and trust boundary shared by you and your AI agents: your knowledge base, retrieval, credentials, and your agents' memory, mounted over MCP — with a first-class notes app as the human console.
> *(Direction settled 2026-07: the substrate is the product; the editor is its human console. Agents get `remember`/`recall` (auditable memory-as-notes) and `use_credential` (USE-not-READ brokering); humans get the trust surfaces — memory review, grant console, retrieval inspection.)*

Status: v0.1–v0.9 shipped · Started 2026-07-16 · Lives at `/home/admin/projects/grimoire/`

> **What this document is.** The design record: what was intended and why,
> kept because the reasoning is worth more than a snapshot of the code. It is
> NOT the current contract — where a detail here disagrees with the running
> system, the README (surfaces, settings, tool list) and SECURITY.md (threat
> model) are authoritative, and both are checked against the code by tests.

---

## 1. Vision & why it's different

There are excellent notes apps. The best local-first ones nail plain-markdown + links + plugins; the best self-hosted web ones nail programmable notes. None is *AI-native*, and none treats **your secrets as a first-class, AI-usable resource**.

grimoire is built on three bets nobody else combines:

1. **AI-native, not AI-bolted-on.** The AI reads, writes, links, and searches notes as first-class operations — over MCP — so grimoire works with Claude Code, claude.ai, the homelab agents, or any MCP client. "Ask your notes" is a core surface, not a plugin. Notes and AI share one substrate.
2. **A secure secret vault your AI can actually use.** grimoire doubles as an encrypted store for AI agent tokens, API keys, and MCP server credentials — and can hand a *scoped, audited, time-boxed* secret to an agent that needs to call a service or MCP. This is the unlock: your knowledge base becomes the trusted broker between your notes, your AI, and the services it drives. No other notes app does this.
3. **Truly everywhere, truly yours.** Local-first, plain `.md` files (zero lock-in, git-friendly), CRDT-ready sync across phone/tablet/desktop, plus read surfaces for constrained devices (Kindle/e-ink) via static export. Offline by default; your data is a folder you own.

### Non-goals
Not a cloud SaaS. Not a proprietary format (everything is markdown + a rebuildable index). Not a plugin marketplace at launch. Not a team/multiplayer product first — single-user, multi-device is the target; collaboration is a later CRDT payoff.

---

## 2. Competitive lens

| Capability | Local-first desktop apps | Self-hosted web apps | Cloud workspaces | Built-in OS notes | **grimoire** |
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

- **Vault** — a directory of markdown files you own. The source of truth. `vault/` holds notes; `vault/.grimoire/` holds the rebuildable index, config, and encrypted secret store (git-ignorable). One grimoire instance serves one vault (multi-vault later).
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
│  grimoire server (one static Go binary, self-hosted)                               │
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
                         vault/  (plain .md + .grimoire/)
```

### 4.1 Storage (plain files, rebuildable index)
- **Files are truth.** Every note is a `.md` on disk. Edits from the PWA, CLI, AI, or a text editor all converge on the file. A **vault watcher** (watchdog/inotify + a debounced reconciler) keeps the index in sync; the index can always be dropped and rebuilt (`grimoire reindex`).
- **Index = SQLite**, one file in `.grimoire/index.db`: `notes` (path, id, title, frontmatter, mtime, hash), `links` (src→dst, resolved/unresolved), `tags`, `fts` (FTS5 over title+body), `vectors` (embeddings, optional), plus vault-ops tables (secrets/grants/audit/sync/devices). Mirrors the proven homelab doc-rag pattern.
- **No lock-in:** point any markdown editor or git at the same `vault/` and it just works.

### 4.2 Editor & frontend
- **PWA, mobile-first**, installable, offline (service worker + IndexedDB cache of recent notes + a write queue that syncs when back online). Same conventions as the homelab PWAs.
- **Editor: CodeMirror 6** — live markdown, `[[` autocomplete against titles/aliases, inline preview, tag/link decorations. Best-in-class editing with no build-step runtime.
- Surfaces: editor, daily note, backlinks pane, graph, search, "ask", capture inbox, secret vault (locked), settings.

### 4.3 Search & "ask your notes" (RAG)
- **Local search:** SQLite FTS5 — instant, offline, ranked, with tag/path filters. No network.
- **Ask your notes:** retrieval over the vector table → answer with citations (which notes). Pluggable model backend: **local Ollama** (default, private) or Claude (via a vault secret). Reuses the homelab's doc-rag + Ollama-native-Anthropic-endpoint knowledge. Private notes are excluded from RAG unless explicitly opted in per query.

### 4.4 Secret vault (the differentiator)
- **At rest:** secrets encrypted with a master key derived from a passphrase (Argon2id → key; Fernet, i.e. AES-128-CBC + HMAC-SHA256, implemented in-tree rather than pulled in). The `.grimoire/secrets.json` envelope holds the salt, the KDF name and the ciphertext, and is useless without the passphrase; the passphrase is never stored. Vault unlocks per-session (kept in memory only).
- **In notes:** reference by handle `{{secret:name}}` — renders as `••••` in the UI, never as plaintext, never indexed, never in RAG context.
- **AI use, not AI read:** an agent never receives a raw secret. It requests a **grant** ("use `gh-token` against the GitHub MCP"); grimoire brokers the call or injects the secret into a scoped subprocess/MCP session, time-boxed, and logs it to the **audit** table. Revocable. This makes grimoire the trusted secret broker between your notes and your AI — the thing that lets an agent actually *do* things safely.
- Threat model documented explicitly (§10): what a compromised session can and cannot reach.

### 4.5 Sync (everywhere, incl. e-ink)
- **Local-first.** Every device has a full or partial vault copy; the server is a sync hub, not a gatekeeper.
- **v1 sync:** delta protocol — client sends `{path, hash, mtime}` manifest; server replies with adds/updates/deletes; content transferred for changed files; **divergence → conflict copy** (`note (conflict 2026-07-16 device).md`) never silent loss. Simple, robust, Syncthing-grade.
- **v2 sync:** per-note **CRDT** (Yjs/automerge text) for conflict-free concurrent edits — the real payoff, designed-for now (notes carry a stable `id`; edits are ops).
- **e-ink / Kindle:** a **static read-only export** (`grimoire export --static`) — plain HTML, no JS, huge-font-friendly, hyperlinked — served at a URL the Kindle/e-reader browser can open. Read-mostly devices get the whole graph without needing the PWA.
- **Devices table** tracks each client for sync state + per-device revoke.

### 4.6 AI-native surface (MCP in and out)
- **grimoire as MCP server:** exposes tools — the shipped list is in the README and is checked against the server by a test; this line records the original intent, and some names moved (`write_note` shipped as `update_note`, `list_backlinks` as `backlinks`, and `link_notes` was dropped). Any MCP client (Claude Code, claude.ai bridge, homelab Discord bot, agentdeck agents) can read/query/write your notes. This is how "easily incorporates AI" is delivered — not a chat box, a protocol.
- **grimoire as MCP client / secret broker:** using vault secrets + grants, grimoire can call *other* MCP servers or services on the AI's behalf (scoped, audited).
- Inline AI actions in the editor (summarize, expand, link-suggest, tag-suggest) route through the same backend.

### 4.7 Capture
- **CLI** (`grimoire`): `new`, `daily`, `capture -`, `search`, `ask`, `open`, `reindex`, `export`, `secret {add,use,ls}`, `sync`, `serve`, `mcp`. Scriptable; pipes to daily/inbox.
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
# MCP served separately (stdio + streamable-HTTP) via cmd/grimoire-mcp
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
- **Backend:** Go 1.26+, standard library HTTP, SQLite (FTS5) via modernc.org/sqlite — pure Go, so the whole product is one static binary with no runtime and no cgo. Fernet + Argon2id for the vault; fsnotify for the vault watcher.
- **Frontend:** PWA, CodeMirror 6, vanilla ES modules (no heavy build), service worker + IndexedDB. Homelab-PWA conventions.
- **AI:** local **Ollama** default (private RAG + embeddings via `nomic-embed-text`, already in the homelab), Claude optional via a vault secret. Reuses doc-rag learnings.
- **CLI:** a single `grimoire` entrypoint (argparse/click).
- **Install:** `pipx install grimoire` → `grimoire serve --vault ~/notes`; Docker image; systemd unit. "Easy install" is a design constraint, tested.

## 9. Roadmap
Status: **1.1.0** — deployed, hermetic tests + Playwright e2e + `verify` green.

> **On the numbering.** The phases below were development milestones, numbered
> independently of the release tags, which had drifted to v2.4.x. The line
> restarts at **1.x**, and that is not cosmetic: the Go module lives at
> `github.com/JeremiahM37/grimoire/go`, and Go requires a `/vN` path suffix for
> any major version ≥ 2 — so on this path v2.x was never a publishable module
> version at all, only a git tag that `go install` had to route around, which is
> why it resolved to a pseudo-version of main.
>
> The reset lands on **1.1.0** rather than 1.0.0 because v1.0.0 was already cut
> on day one and is cached in `proxy.golang.org` with its hash recorded in
> `sum.golang.org`, which is append-only. Re-pointing a tag the checksum
> database has already seen makes every later fetch fail with a mismatch, and
> nothing can undo it — so the tag is spent, and 1.1.0 is the first version the
> `go/` module can actually resolve to.
- **v0.1 (core) ✅:** vault + watcher/reindex, note CRUD (files ⇄ index), frontmatter, `[[wiki-links]]` + backlinks, tags, daily notes, FTS5 search, PWA editor, CLI, hermetic test suite + `.verify.yaml`.
- **v0.2 (AI) ✅:** embeddings + ask-your-notes (auto-Ollama, else offline extractive), grimoire-as-MCP-server, inline AI actions, private-notes exclusion.
- **v0.3 (secrets) ✅:** encrypted vault, grants + audit, AI secret-broker (USE-not-READ).
- **v0.4 (capture) ✅:** browser extension, audio memos + whisper, share target, CLI capture.
- **v0.5 (sync) ✅:** delta sync + conflict copies, live cross-device refresh, static e-ink export.
- **v0.6–v0.9 ✅ (best-in-class):** tag browsing, graph view, task checkboxes, command palette (Ctrl-K), real editor (toolbar/smart-lists/tab), image/file attachments, theme toggle, outline/TOC, note templates, per-note HTML export, in-app settings, **encryption-at-rest for private notes**, soft-delete/trash + undo, aliases, word count, pin/favorite, calendar.
- **v0.10–v0.13 ✅:** tables, find & replace, unlinked mentions, random/duplicate, zip import/export, search operators, properties editor, tag rename, **desktop-first-class** (split view + draggable divider, collapsible/resizable sidebar, context menu, focus mode, keyboard nav), callouts/highlights, code syntax highlighting, tag autocomplete + browser, note hover previews, offline draft protection, **security hardening** (Argon2id, lockout, idle-lock, SSRF guard, scope-bypass fix, CSP, rotation, revocation — see SECURITY.md), **background auto-sync** with a peer.
- **v1.0 ✅ true CRDT sync:** `go/internal/crdt` is a real sequence CRDT (fractional-index / Logoot). Concurrent edits to the same note auto-merge with no conflict copies (proven by a randomized fuzz test); the body is CRDT'd while frontmatter converges deterministically; independent same-name histories are conflict-copied rather than garbled.
- **1.0 — the trust boundary made true, and three things that were written but never read:**
  - **Untrusted content** (`internal/trust`). Connectors put other people's
    writing into the corpus an agent reads from; nothing distinguished it. Now
    every note carries an `origin`, every content surface reports and can
    filter on the derived trust level, untrusted passages are fenced before a
    reader sees them, and an untrusted fact may not supersede a trusted one.
    Measured against a no-intervention baseline in `benchmarks/injection/`
    rather than asserted.
  - **`grimoire eval`** (`internal/eval`). The benchmark discipline pointed at
    the operator's own vault: a frozen question set, scored with no reader and
    no judge, so changing an embedder is a number instead of a feeling.
  - **Just-in-time grants**. An agent can ASK for a credential it has no grant
    for, and a person approves or denies. Grants that must exist before the
    need select for pre-granting everything, which is the failure least
    privilege exists to prevent.
  - **The belief digest**. Superseded facts were kept and only ever read
    backwards (`as_of`); `GET /api/memory/changes` reads them forwards, which
    is the direction people actually ask about.
  - **Staleness**. Memory had a temporal model and knowledge had none, so the
    half of the corpus most likely to be wrong carried no signal. Age is
    reported on every hit, never acted on, and there is a review queue.
  - **The read audit reads itself**. The trail added in v2.4.5 had never been
    queried. A burst detector over it looks for breadth, not depth.
  - **Value-slot reconciliation** (`internal/memory/slots.go`). Benchmarking
    the memory engine for the first time found that write-time reconciliation
    recognised **1 of 72** real update pairs on LongMemEval's knowledge-update
    transcripts: `Attribute` reads SUBJECT PREDICATE VALUE, which is what an
    agent writes, not how a fact arrives. A second rule — same discriminative
    terms, different value of the same kind — takes it to 20/72 for one extra
    false positive in 400, deterministic, ~48 µs per comparison. The
    competitors do this with a model call per write; this is the version that
    can sit on an agent's hot path.
- **remaining:** publish (OSP). The name stays **Grimoire** — the placeholder
  was reviewed against the alternatives and kept, so this line is a decision
  now rather than a to-do.

## 10. Risks & threat model (sketch — expanded per phase)
- **Vault brokering is the crown jewel and the biggest risk.** A compromised unlocked session could request grants. Mitigations: grants are scoped + time-boxed + revocable + audited; secrets never leave the process as plaintext to the client; per-secret allow-lists of which MCP/service a token may be used against; a "panic lock" that drops the in-memory key. Default-deny.
- **fs ⇄ index races** (external edit mid-index) → hash+mtime reconciliation, atomic writes (temp+rename), debounce; index is disposable.
- **Sync data loss** → never silent overwrite; conflict copies; content-hash verification; local-first means the device always has its own copy.
- **RAG leaking private notes** → private excluded from vectors by default; explicit per-query opt-in; tests assert non-leakage.
- **Path traversal / injection** → note paths sandboxed to the vault; titles/links escaped; negative tests.
- **Model/endpoint drift** (Ollama/Claude) → backend isolated behind one interface; degrade gracefully when AI is unavailable (search/edit still work fully offline).
