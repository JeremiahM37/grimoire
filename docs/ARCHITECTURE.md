# Grimoire — Architecture

A map for contributors. The one-paragraph version: **plain markdown files are
the source of truth; everything else is a rebuildable cache or a view.** The
server is one static Go binary over SQLite (FTS5, pure-Go driver, no cgo); the
client is a no-build vanilla-JS PWA with one vendored artifact (the CodeMirror 6
live editor).

## Invariants (break these and you've broken Grimoire)

1. **The vault is truth.** `*.md` files under `GRIMOIRE_VAULT` are the data.
   The SQLite index (`.grimoire/index.db`) can be deleted at any time and is
   rebuilt on boot (`index.reindex()`); the watcher reconciles external edits.
2. **Escape first, then render.** Both markdown renderers (server
   `go/internal/render`, client `mdToHtml`) HTML-escape input before applying
   rules. Any new rule operates on escaped text.
3. **Private/encrypted never leaks.** Private notes are excluded from vectors,
   RAG, `/read`, export, transclusion and live queries on unauthenticated
   surfaces. Encrypted notes exist on disk / in history / in trash only as
   ciphertext (`grimoire:enc:v1:…`); plaintext lives in memory and the
   authenticated API only, and never in localStorage drafts.
4. **User input never reaches SQL/FTS as syntax.** Parameterized queries only;
   FTS input is quoted; sort keys and columns come from whitelists
   (`go/internal/queries`).
5. **Paths are confined.** All file access goes through `Vault.SafePath` /
   `SafeRawPath` (or the plugin/history equivalents), which resolve and verify
   containment. `.grimoire/` is reserved.
6. **Provenance travels with the text.** Every note has an `origin`, every
   retrieval hit reports the trust level derived from it, and the derivation
   lives in exactly one place (`internal/trust`). A surface that returns note
   text and does not say where it came from is a bug, not an omission — the
   whole model rests on a caller being able to tell your writing from a
   stranger's. Corollary: untrusted text may never supersede a trusted memory
   fact, and the check belongs where the candidates are chosen, not where the
   answer is filtered.

## Server (`go/`)

| Package | Responsibility |
|--------|----------------|
| `cmd/grimoire` | The binary: server *and* CLI. Env-driven config; `GRIMOIRE_*` variables |
| `cmd/grimoire-mcp` | The agent interface: knowledge tools + `remember`/`recall` + `use_credential`/`list_grants` |
| `internal/db` | SQLite connection + schema (modernc.org/sqlite — pure Go, no cgo) |
| `internal/vault` | File I/O, path safety, slugs, note serialization |
| `internal/markdown` | Parsing: frontmatter (order-preserving), links, tags, title derivation |
| `internal/index` | vault ⇄ index reconciliation; links/tags/FTS/vectors rows; hybrid retrieval |
| `internal/render` | Markdown → safe HTML (link map, transclusion, queries) for `/read` + export |
| `internal/queries` | Live-query engine: typed parse → parameterized execution |
| `internal/history` | Per-note version snapshots (ring buffer) |
| `internal/embed` | Embedders: hashing floor, built-in model2vec, Ollama, OpenAI-compatible |
| `internal/ai` | Answer synthesis, question decomposition, reranking, consolidation, transcription |
| `internal/crypto`, `internal/secrets` | Argon2id KDF, Fernet sealing, secret vault + broker, note encryption, just-in-time grant requests |
| `internal/trust` | Origin → trust level, and the fence untrusted passages are wrapped in before a reader sees them |
| `internal/eval` | Frozen question sets + judge-free scoring of retrieval on the operator's own vault |
| `internal/readlog` | The restricted-read trail, and the burst detector that reads it back |
| `internal/crdt`, `internal/crdtstore` | Sequence CRDT for concurrent note-body merges |
| `internal/sync` | Bidirectional delta sync with a peer |
| `internal/watcher` | Debounced filesystem watcher |
| `internal/settings` | UI-editable operational settings (`.grimoire/settings.json`) |
| `internal/api` | HTTP surface, one file per domain; `Routes()` assembles + security headers |

Retrieval has TWO paths and they are not interchangeable. `/api/retrieve` ranks
one query; `/api/retrieve?smart=1` decomposes the question, retrieves per
sub-question and reranks the pool. The second was unreachable on its own until
v2.5, with two consequences: the console's "what would the agent see" inspected
a different ranking from the one the agent used, and every published benchmark
number was measured on the plain path rather than the one `/api/ask` used.

Both are now resolved, and the resolution went the other way from the guess:
**`/api/ask` no longer decomposes by default** (`smart: true` opts in), because
measuring it showed plain retrieval nominally ahead at 1/70th the latency —
see benchmarks/longmemeval/REPORT-multihop.md. The console inspects the same
path the answer uses, and the benchmarks now measure the shipped default.
With no LLM configured the two paths are identical anyway, which is why this
is a parameter rather than a mode.

Route-ordering rule: `/api/notes/{path...}` is greedy, and Go's ServeMux
requires a `{path...}` wildcard to be the LAST segment of a pattern — so the
per-note actions (`/pin`, `/rename`, `/history/…`) cannot be routed by pattern
at all. They are dispatched on the trailing segment in `internal/api/dispatch.go`,
and `/api/notes/random` is registered as its own literal route.

## Client (`web/`)

| File | Responsibility |
|------|----------------|
| `util.js` | Dependency-free base: `$`, `api()` fetch, toasts, `esc()`, slugs |
| `markdown.js` | Client markdown engine (mirrors `internal/render`): `mdToHtml`, dynamic-block hydration, heading anchors. Link data injected via `setNoteIndex` — no app-state coupling |
| `app.js` | Application shell: state, note list/folders, editor wiring, palette, modals, sync polling. Organized by `/* ---------- section ---------- */` banners, each < ~100 lines |
| `editor.js` | Editor facade — one API over live (CM6) and classic (textarea) modes. The hidden textarea mirrors the CM doc so reads stay uniform; writers call `Editor.sync()` |
| `plugins.js` | Plugin runtime: registry, activation, fence renderers, panels, events |
| `canvas.js` | JSON Canvas view (pan/zoom/drag/edges) |
| `graph.js` | Force-directed graph view |
| `vaultui.js` | Secret-vault modal (crypto stays server-side) |
| `vendor/editor.js` | Built CM6 bundle — generated by `tools/build-editor.mjs`, never edited by hand |
| `sw.js` | Offline service worker. **Bump `CACHE` whenever shell files change** |

## Plugins (`plugins/` + `<vault>/plugins/`)

Manifest + ES-module entry; built-ins trusted/enabled, vault plugins disabled
until explicitly enabled. Full contract in [PLUGINS.md](PLUGINS.md).

## Data flows worth knowing

* **Save**: editor → debounced `PUT /api/notes/{path}` → history snapshot of
  the previous body → vault write → `index.upsert` → SSE-less "rev" polling
  picks it up on other open devices.
* **Query block**: preview renders a placeholder → `POST /api/query`
  (authenticated, may see private) → hydrated. `/read`/export run the same
  engine server-side with `include_private=False`.
* **Sync**: CRDT-first per note (shared atom ids after first contact),
  conflict-copy fallback — data is never silently lost.
* **Pulled document**: connector → `Runner.write` sets `origin:` in the
  frontmatter → ordinary note write → `index.upsert` stamps `notes.untrusted`
  and `vectors.untrusted` → ranking can exclude it (`Filter.TrustedOnly`,
  applied INSIDE ranking so corpus statistics do not carry it) → `ai.Context`
  carries `Untrusted` → `RenderReaderPrompt` fences it. Every hop is where it
  is for a reason; the one that is easy to get wrong is the filter's position,
  because filtering the OUTPUT still lets poisoned rows move BM25.
* **Confirming a note**: console → `POST /api/stale/verify` → `verified:` into
  the note's frontmatter → `index.upsert` → the retrieval cache is PATCHED, not
  rebuilt, so every per-note field the cache holds has to be refreshed there.
  Missing one is invisible until a warm server disagrees with a cold one.

## Testing

* `go/internal/*/[_]test.go` — pure logic (renderer, queries, CRDT, crypto…)
  and the HTTP API against a fresh temp vault per test
* `compat/fixtures/` — frozen output from the original Python implementation,
  replayed by `go/internal/compat`. They are a historical contract: the
  generator is gone with the implementation that produced them, so a fixture
  that fails means the Go build changed, not that the fixture is stale
* `tests/e2e` — Playwright against the real binary; `page` fixture pins the
  classic editor, `live_page` runs CM6; shared fixtures in `tests/e2e/conftest.py`
* `.verify.yaml` — live smoke (isolated port 9119; never the production vault)

Lint: `gofmt -l go/`, `go vet ./...`, and `ruff check tests/ benchmarks/` must stay clean (config in
`pyproject.toml`).
