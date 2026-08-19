<div align="center">

# ✦ Grimoire

**A self-hosted personal context server** — one knowledge and trust boundary
shared by you and your AI agents. Your knowledge base, retrieval, credentials,
and your agents' memory, mounted over MCP. With a first-class notes app as the
human console.

<!-- badges -->
![status](https://img.shields.io/badge/status-stable-2ea44f)
![license](https://img.shields.io/badge/license-MIT-blue)
![go](https://img.shields.io/badge/go-1.26%2B-00add8)
![docker](https://img.shields.io/badge/docker-ready-2496ed)
![MCP](https://img.shields.io/badge/MCP-first--class-5b4bff)
![PWA](https://img.shields.io/badge/console-offline%20PWA-19c37d)
[![LoCoMo](https://img.shields.io/badge/LoCoMo-81.6%25%20vs%20full--context%2082.2%25-2ea44f)](benchmarks/locomo/)
[![LongMemEval](https://img.shields.io/badge/LongMemEval-75.0%25%20%40%2020×%20fewer%20tokens-2ea44f)](benchmarks/longmemeval/)

![Grimoire console](docs/screenshots/hero.png)

</div>

Your agents already need four things from you: what you know, a way to search
it, credentials to act for you, and somewhere to keep what *they* learn. Today
those live in four disconnected tools — or worse, pasted into prompts. Grimoire
is the single self-hosted server an agent mounts to get all four:

```
          ┌──────────────────── one trust boundary ────────────────────┐
agent ──MCP──►  knowledge (your markdown)  retrieval (RAG + citations) │
          │     credentials (USE, never READ)  agent memory (auditable)│
          └────────────────────────────────────────────────────────────┘
                   the same policy layer decides what an agent
                   can READ and what it can DO
```

- **Knowledge** — plain markdown files you own. **Mount your existing vault**
  (any folder of `.md`, including one another notes app manages) — no migration;
  the watcher reconciles external edits live.
- **Retrieval** — `ask`/`search` over MCP with citations; fully local (Ollama or
  a deterministic offline fallback). Always auditable: *"what would the agent
  see for X?"* shows the exact retrieved chunks.
- **Credentials** — an encrypted vault (Argon2id + Fernet) whose secrets your
  agent can **use but never read**: you mint a scoped, time-boxed grant; the
  server injects the value into the outbound call; every use is audited.
- **Agent memory** — `remember`/`recall` tools writing to a `memory/` namespace
  of ordinary notes with provenance (which agent, when, from what task). You
  read, edit, diff, and **roll back** your agent's memory like any note.

Nothing else puts these in one trust boundary: memory layers (Mem0, Letta, Zep)
have no knowledge base or credentials; notes-RAG tools (Khoj, editor plugins)
have no agent memory or secrets; token vaults (Auth0 GenAI, Arcade, Infisical)
have no knowledge layer. Grimoire is the unified, self-hosted version.

**Not wiring up agents yet?** Grimoire is also a full offline notes app in its
own right — CodeMirror live preview, wiki-links, backlinks, graph, daily notes,
transclusion, canvas. Mount your existing markdown vault with no migration and
daily-drive it; the agent substrate is there when you want it.

## Quick start

```bash
docker compose up -d        # → http://localhost:9111 · notes land in ./vault
```

Already have a pile of markdown? Skip the empty-vault cold start:

```bash
grimoire ingest ~/obsidian-vault    # bulk-import a folder of markdown/text
grimoire seed-demo                  # …or write a small sample vault to explore
```

Mount an **existing** markdown vault instead (editing through Grimoire preserves
foreign frontmatter byte-for-byte — nested YAML and all):

```yaml
# docker-compose.yml
volumes:
  - /path/to/your/vault:/vault
```

<details>
<summary>…or run from source (no Docker)</summary>

```bash
cd go && go build -o grimoire ./cmd/grimoire && go build -o grimoire-mcp ./cmd/grimoire-mcp
GRIMOIRE_VAULT=~/notes GRIMOIRE_WEB_DIR=../web ./grimoire   # → http://<host>:9111
```

One static binary, no runtime, no virtualenv. The same binary is the CLI:
`grimoire capture "a thought"`, `grimoire search deploy`, `grimoire ingest
~/old-notes`, `grimoire export --out site/` — run `grimoire help` for the list.
</details>

**Connect an agent (MCP):** any MCP client can mount Grimoire — Claude Code,
desktop assistants, custom agents. Example config:

```jsonc
// Claude Code's .mcp.json shown; adapt to your client
{ "mcpServers": { "grimoire": {
    "command": "/path/to/grimoire/go/grimoire-mcp",
    "env": { "GRIMOIRE_API": "http://localhost:9111",
             "GRIMOIRE_AGENT_NAME": "my-agent" } } } }
```

The agent gets, in one mount:

<!-- tools:begin (checked against the server by a test) -->

| | tools |
|---|---|
| **Credentials — use, never read** | **`use_credential`** · **`list_grants`** |
| **Agent memory** | **`remember`** · **`recall`** · **`consolidate_memory`** |
| Knowledge | `search_notes` · `ask_notes` · `read_note` · `list_notes` · `backlinks` · `list_tags` |
| Writing | `create_note` · `update_note` · `append_daily` |
| Exact values | `get_fact` · **`set_fact`** |
| Orientation | `get_briefing` · `kb_info` |

<!-- tools:end -->

(That table is checked against the running server by a test, so it cannot drift
out of date.)

`ask_notes` decomposes multi-hop questions and LLM-reranks the evidence when a
model is configured. `consolidate_memory` compacts the `memory/` namespace
(merge redundant entries, supersede stale ones) so recall stays sharp as it
grows — snapshotted first, so every rewrite is reviewable and roll-back-able.

**Structured facts** — for values that must be *exact* (a port, a version, an
owner, a decision), prose RAG is the wrong tool. Write `key:: value` inline in
any note and agents can look it up deterministically via `get_fact` — no
paraphrase, no hallucination. It's still plain markdown you read and edit; the
facts table is just a projection of it, exactly like tags and backlinks.

The MCP server speaks **stdio** by default (local desktop agents). For web or
remote clients (Open WebUI, hosted), run it over **streamable-HTTP** with no
proxy — `GRIMOIRE_MCP_TRANSPORT=http ./grimoire-mcp` serves at
`http://127.0.0.1:9112/mcp` (localhost-bound; front it with your reverse proxy
+ auth before exposing it).

> **Headless agents:** non-interactive runs often skip untrusted project-level
> MCP configs silently — register the server at user scope (or pass your CLI's
> explicit MCP-config flag) and have the agent call `kb_info` once to verify the
> mount. A silently missing mount looks identical to "no knowledge exists."

> **Make agents actually use it:** mounted tools are necessary, not sufficient —
> agents reliably read a repo's context file, and only sometimes browse tool
> lists. Run `grimoire agent-setup` to print the MCP config **plus a
> CLAUDE.md/AGENTS.md snippet** that tells agents to call `get_briefing` first
> and consult the KB before assuming project facts.

**The 60-second demo:** ask your agent to research something → it `ask`s your
notes (you can inspect exactly what it retrieved) → it calls an API with
`use_credential` (the key never enters its context) → it `remember`s what it
learned → you open `memory/` in the console, read the note it wrote, edit one
line, roll back another. That loop is the product.

## The human console

A substrate needs a place where the human reads, reviews, and decides — so
Grimoire ships a full offline-PWA notes app on the same API:

| Rendered markdown | Graph view |
|---|---|
| ![preview](docs/screenshots/preview.png) | ![graph](docs/screenshots/graph.png) |

- **Trust surfaces** (the console's real job):

| Agent-memory review | Retrieval inspection |
|---|---|
| ![agent memory](docs/screenshots/agent-memory.png) | ![retrieval inspection](docs/screenshots/retrieval-inspection.png) |
| Memory notes badged 🤖 with provenance (which agent, which task) — edit or roll back any entry. | "What would the agent see for X?" — the exact ranked chunks, nothing hidden. |

<div align="center">
<img src="docs/screenshots/credential-console.png" width="640" alt="credential console">
<br><sub>The credential console: secrets your agent can use but never read — scoped, time-boxed, revocable.</sub>
</div>
- **Editing** — CodeMirror 6 live preview (markup revealed only where you're
  editing), slash commands, `[[` autocomplete, classic plain-text mode, offline
  drafts. Wiki-links, backlinks + outgoing links, unlinked mentions, hover
  previews, tags, graph, daily notes + calendar, live ```` ```query ```` blocks,
  transclusion, footnotes, version history, folder tree, canvas, slides.
- **Plugins** — seven first-party: on-topic ones on by default (KaTeX, Mermaid,
  kanban, vault stats); optional widgets one toggle away (pomodoro, journal
  heatmap, word goal) + an in-app scaffold. [docs/PLUGINS.md](docs/PLUGINS.md)
- Encryption-at-rest for private notes, e-ink `/read` surface, HTML export,
  CRDT-merged multi-device sync, trash + undo, CLI.

## Config

Everything is environment-driven (same variables bare-metal, systemd, Docker):

| Variable | Default | What it does |
|----------|---------|--------------|
| `GRIMOIRE_VAULT` | `~/grimoire-vault` | The folder of `.md` files — your data |
| `GRIMOIRE_PORT` / `GRIMOIRE_HOST` | `9111` / `0.0.0.0` | Bind address |
| `GRIMOIRE_AUTH_TOKEN` | *(empty = open)* | Bearer token for the API/console |
| `GRIMOIRE_AGENT_NAME` | `agent` | Memory attribution for an MCP client |
| `GRIMOIRE_OLLAMA_URL` | *(empty)* | Reachable Ollama → generative ask/summarize |
| `GRIMOIRE_LLM` / `GRIMOIRE_LLM_MODEL` | auto / `qwen3.5:4b` | Answer backend (`ollama` · `claude` · `openai`) + model |
| `GRIMOIRE_LLM_BASE_URL` / `_API_KEY` | *(empty)* | Any OpenAI-compatible endpoint (OpenAI, OpenRouter, Together, Groq, vLLM, LM Studio, LiteLLM…); key can also live in the vault as `llm-api-key` |
| `GRIMOIRE_EMBED_MODEL` | `nomic-embed-text` | Embeddings (offline hashing fallback built in) |
| `GRIMOIRE_LOCAL_EMBED` / `_MODEL` | `auto` / `potion-base-8M` | Local semantic embeddings — the ~30 MB model is fetched once on first start (`grimoire fetch-model` to pre-seed); `off` to stay on the hashing embedder |
| `GRIMOIRE_WHISPER_URL` / `_MODEL` | *(empty)* | Audio-memo transcription |
| `GRIMOIRE_DAILY_DIR` / `GRIMOIRE_INBOX_DIR` | `journal` / `inbox` | Vault sub-folders |
| `GRIMOIRE_SYNC_PEER` / `_TOKEN` / `_INTERVAL` | *(off)* | Background sync with a peer |
| `GRIMOIRE_VAULT_IDLE_LOCK` | `900` | Credential-vault auto-lock (seconds) |
| `GRIMOIRE_BROKER_ALLOW_PRIVATE` | `0` | Allow brokered calls to private-range hosts |
| `GRIMOIRE_FRAME_OPTIONS` | `SAMEORIGIN` | X-Frame-Options (reverse-proxy embedding) |
| `GRIMOIRE_MCP_TRANSPORT` | `stdio` | `http` serves MCP over streamable-HTTP instead |
| `GRIMOIRE_MCP_ADDR` / `_PORT` | `127.0.0.1` / `9112` | Bind for the MCP http transport |
| `GRIMOIRE_URL` | `http://127.0.0.1:$PORT` | API the MCP server talks to |
| `GRIMOIRE_PLUGIN_DIR` | `plugins` | Where plugin bundles are loaded from |
| `GRIMOIRE_MODEL_DIR` | *(cache dir)* | Where the local embedding model is stored |
| `GRIMOIRE_EMBED_BASE_URL` / `_API_KEY` | *(empty)* | OpenAI-compatible embeddings endpoint |
| `GRIMOIRE_NO_WATCHER` | `0` | Disable the filesystem watcher (tests/CI) |

AI/model settings can also be changed live in ⚙ Settings (persisted in the
vault, no restart). Editor mode (live/classic) and theme are per-device.

## Security posture (short version)

Secrets sealed with Argon2id + Fernet, key in memory only, brute-force lockout,
idle auto-lock, passphrase rotation. Broker: origin-exact + path-prefix scopes,
SSRF-guarded, fully audited; secret values never appear in any response. Private
notes excluded from retrieval, `/read`, export, transclusion, and queries on
unauthenticated surfaces. Strict CSP. Full threat model: [SECURITY.md](SECURITY.md).

## Benchmarks

Grimoire's retrieval is measured on the two public long-conversation memory
benchmarks the agent-memory field uses — [LoCoMo](https://github.com/snap-research/locomo)
(ACL 2024) and [LongMemEval](https://github.com/xiaowu0162/LongMemEval)
(ICLR 2025) — under pre-registered protocols with all baselines run under
identical conditions: stratified question samples, conversations ingested as
plain session notes, questions asked verbatim against the same retrieval
code the MCP tools serve, fixed reader (`claude-haiku-4-5`), strict blind
LLM judge (`claude-sonnet-5`).

**LoCoMo** (500 questions, ~24k-token conversations):

| context given to the reader | accuracy | context tokens / question |
|---|---|---|
| nothing | 1.2% | 0 |
| grimoire retrieval, zero-dependency default | 76.8% | ~6.2k |
| grimoire retrieval + the local model (default) | 81.6% | ~7.0k |
| grimoire retrieval + nomic-embed (Ollama) | **81.6%** | ~6.2k |
| entire conversation in context | 82.2% | ~24k |

**LongMemEval** (200 questions, ~117k-token haystacks of ~50 chat sessions):

| context given to the reader | accuracy | context tokens / question |
|---|---|---|
| nothing | 6.5% | 0 |
| grimoire retrieval + the local model (default) | **74.5%** | ~6.8k |
| grimoire retrieval + nomic-embed (Ollama) | 73.0% | ~5.8k |
| entire haystack in context | 70.5% | ~117k |

On LoCoMo, retrieval is statistically indistinguishable from stuffing the
whole conversation into context (McNemar p = 0.82 nomic / p = 0.51
model2vec, n = 500) at ~4× fewer tokens. On LongMemEval's much larger
haystacks, retrieval **matches and directionally beats** full context
(p = 0.26) at ~20× fewer tokens — long-context needle-finding degrades
where focused retrieval doesn't, especially on temporal reasoning (81.5%
vs 68.5%). Full methods, per-category tables, per-question raw data, and
the honest failure notes: [benchmarks/locomo/](benchmarks/locomo/) ·
[benchmarks/longmemeval/](benchmarks/longmemeval/).

**Round 8** — `finalize` now expands each top hit into a query-focused excerpt
of its note (its other high-scoring chunks, in document order) instead of
merging the hit with its immediate neighbours. Measured against a same-epoch
control, replicated on the questions the study had never scored, and pooled:
**LongMemEval +4.3 points (72.3% → 76.6%, n = 470, exact McNemar p = 0.0045)**
with **no measurable change on LoCoMo** (−0.3 points, n = 999, p = 0.84). Still
~15× fewer tokens than full context. Details: [benchmarks/longmemeval/REPORT.md](benchmarks/longmemeval/REPORT.md).

**Measurement floor** (round 6): re-reading a byte-identical context set with
the same frozen reader and judge flips 8–12% of answers and moves accuracy by
1–4 points, so a difference smaller than roughly 5 points between two
retrieval variants is not resolvable at these sample sizes. The comparisons
above are either far larger than that (retrieval vs no memory) or are parity
claims, which sampling noise only makes harder to assert — but any future
tuning claim needs a same-epoch control, and the harnesses now provide one.

## Tests

```bash
cd go && go test ./...        # hermetic: 146 unit + api + sync + CLI tests
.venv/bin/pytest tests/e2e    # 100 real-browser flows against the built binary
verify run .verify.yaml       # live api + headless-browser smoke (isolated port)
```

## Layout

```
go/cmd/grimoire            the server AND the CLI — one static binary
go/cmd/grimoire-mcp        the agent interface (knowledge · memory · credentials)
go/internal/index          SQLite(FTS5) + vectors over plain markdown
go/internal/secrets        credential vault (Argon2id + Fernet) + broker
go/internal/crdt           sequence CRDT for concurrent-edit merges
go/internal/embed          embedders, incl. the built-in local model2vec
web/                       the human console (offline PWA, no build step)
plugins/                   first-party console plugins
tests/e2e/                 real-browser flows (implementation-agnostic)
docs/                      ARCHITECTURE · PLUGINS
```

**More docs:** [ARCHITECTURE](docs/ARCHITECTURE.md) ·
[PLUGINS](docs/PLUGINS.md) · [SECURITY](SECURITY.md) · [CONTRIBUTING](CONTRIBUTING.md)

---

<div align="center">
<sub>MIT licensed · self-hosted · one trust boundary for you and your agents.</sub>
</div>
