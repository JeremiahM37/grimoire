# Configuration

Everything is environment-driven — the same variables bare-metal, under systemd,
and in Docker. Nothing here is required: an empty environment gives you a working
server on `~/grimoire-vault` at `:9111`.

Everything is environment-driven (same variables bare-metal, systemd, Docker):

| Variable | Default | What it does |
|----------|---------|--------------|
| `GRIMOIRE_VAULT` | `~/grimoire-vault` | The folder of `.md` files — your data |
| `GRIMOIRE_PORT` / `GRIMOIRE_HOST` | `9111` / `0.0.0.0` | Bind address |
| `GRIMOIRE_AUTH_TOKEN` | *(empty = open)* | Bearer token for the API/console |
| `GRIMOIRE_AGENT_NAME` | `agent` | Memory attribution for an MCP client |
| `GRIMOIRE_SESSION` | *(empty)* | Run id an MCP client stamps on every memory it writes. Set by whatever launches the agent, so "what did this run learn" (`/api/memory/changes?session=`) needs nothing from the model |
| `GRIMOIRE_BANK` | *(empty)* | The memory bank an MCP client's bank tools (`retain`, `bank_recall`, …) use when a call names none. See [MEMORY_BANKS.md](MEMORY_BANKS.md) |
| `GRIMOIRE_OLLAMA_URL` | *(empty)* | Reachable Ollama → generative ask/summarize |
| `GRIMOIRE_LLM` / `GRIMOIRE_LLM_MODEL` | auto / `qwen3.5:4b` | Answer backend (`ollama` · `claude` · `openai`) + model |
| `GRIMOIRE_LLM_BASE_URL` / `_API_KEY` | *(empty)* | Any OpenAI-compatible endpoint (OpenAI, OpenRouter, Together, Groq, vLLM, LM Studio, LiteLLM…); key can also live in the vault as `llm-api-key` |
| `GRIMOIRE_LLM_REASONING_EFFORT` | *(empty)* | Reasoning effort for models that think: `low`/`medium`/`high`/`max` is sent as `reasoning_effort`; `off` sends `thinking: {"type": "disabled"}` (the documented switch on DeepSeek's OpenAI-compatible API). Structured calls such as memory-bank extraction are where this matters most |
| `GRIMOIRE_LLM_EXTRA_BODY` | *(empty)* | A JSON object merged into every OpenAI-compatible request, for vendor fields no setting anticipates (e.g. `{"thinking":{"type":"disabled"}}`). A call's own fields win over it |
| `GRIMOIRE_EMBED_MODEL` | `nomic-embed-text` | Embeddings (offline hashing fallback built in) |
| `GRIMOIRE_MEMORY_VERIFY_THRESHOLD` | `0.3` | Probability that a remembered fact has changed since it was last verified above which `recall` marks it `freshness.action: verify`. Lower re-checks more often; higher re-checks less. See [FRESHNESS.md](FRESHNESS.md) |
| `GRIMOIRE_DECISION_URL` | *(empty = off)* | A typed-decision server on TypeSafe's Jev wire format (`POST /v1/systemone`): `https://api.typesafe.ai` for Jev, or a local `laya-serve`. When set, a fact remembered without a freshness tier is asked whether it describes changing state, and the answer sets its change-rate prior. Untrusted facts are never sent. See [FRESHNESS.md](FRESHNESS.md#decision-model) |
| `GRIMOIRE_DECISION_MODEL` | `jev-latest` | Model id sent with each decision |
| `GRIMOIRE_DECISION_API_KEY` | *(empty)* | Bearer key for the decision server; can also live in the vault as `decision-api-key` |
| `GRIMOIRE_DECISION_CALIBRATION` | `1.837,-2.493` | Platt curve `a,b` applied to the returned probability, fitted for jev-1.13.0; `1,0` turns it off |
| `GRIMOIRE_RETELL_URL` | *(off)* | Typed-decision server that judges whether a prompt restates a memory on file; the prompt before it is learned as that memory's cue ([MEMORY_USE.md](MEMORY_USE.md)) |
| `GRIMOIRE_RETELL_MODEL` | `jev-latest` | Model name sent to the re-tell judge |
| `GRIMOIRE_RETELL_API_KEY` | *(none)* | Key for the judge; else the vault secret `retell-api-key`, then the decision key |
| `GRIMOIRE_RETELL_THRESHOLD` | `0.9` | Yes-probability needed before a cue is learned |
| `GRIMOIRE_DREAM_INTERVAL_HOURS` | `24` | Hours between scheduled dreams (hygiene + security sweep over agent memory); `0` turns the schedule off. A dream only runs when memory changed since the last one |
| `GRIMOIRE_DREAM_APPLY` | `safe` | What a scheduled dream may change: `safe` applies mechanical, reversible fixes (index repair, freshness tiers and history from recorded evidence) with a history snapshot first; `off` only reports |
| `GRIMOIRE_LOCAL_EMBED` / `_MODEL` | `auto` / `potion-base-8M` | Local semantic embeddings — the ~30 MB model is fetched once on first start (`grimoire fetch-model` to pre-seed); `off` to stay on the hashing embedder |
| `GRIMOIRE_RERANK` | `auto` | Reranking of retrieved passages: `local` (built-in cross-encoder), `remote` (a `/rerank` service), `off`. `auto` uses `remote` when `GRIMOIRE_RERANK_URL` is set, else `local` when the model is on disk or may be downloaded, else `off`. See [Reranking](#reranking) |
| `GRIMOIRE_RERANK_MODEL` | `cross-encoder/ms-marco-MiniLM-L-6-v2` | Local: a hub repo id (fetched once, ~90 MB) or a path to a directory with `config.json`, `tokenizer.json` and `model.safetensors`. Remote: the model name the service expects |
| `GRIMOIRE_RERANK_URL` / `_API_KEY` | *(empty)* | Remote reranker base URL (`/rerank` is appended unless present) and bearer token |
| `GRIMOIRE_RERANK_MAX_LEN` | `256` | Local reranker: tokens per (query, passage) pair, specials included; longer pairs are truncated longest-first (max 512) |
| `GRIMOIRE_BANK_WORKERS` | `2` | Background workers for memory-bank operations (async retain, consolidation, mental-model refresh) and webhook delivery; `0` turns them off. Memory-bank recall uses the reranker above, except that `auto` only uses a local model already on disk |
| `GRIMOIRE_WEBHOOK_ALLOW_PRIVATE` | *(off)* | Let memory-bank webhooks reach loopback and private networks. Link-local and cloud-metadata addresses stay refused |
| `GRIMOIRE_WHISPER_URL` / `_MODEL` | *(empty)* | Audio-memo transcription |
| `GRIMOIRE_WEB_SEARCH_PROVIDER` | *(off)* | `searxng` · `brave` · `serper` · `google` — enables `search_web` / `open_urls` |
| `GRIMOIRE_WEB_SEARCH_URL` / `_KEY` / `_CX` | *(empty)* | SearXNG base URL · provider key (or `vault:name` to read it from the credential vault) · Google engine id |
| `GRIMOIRE_DAILY_DIR` / `GRIMOIRE_INBOX_DIR` | `journal` / `inbox` | Vault sub-folders |
| `GRIMOIRE_SYNC_PEER` / `_TOKEN` / `_INTERVAL` | *(off)* | Background sync with a peer |
| `GRIMOIRE_SYNC_FOLDER` | *(off)* | Sync and back up through a folder a cloud drive syncs (Dropbox, iCloud Drive, OneDrive, Google Drive). Usually set from Settings or `grimoire sync folder PATH`, which store it in `.grimoire/settings.json`; that stored value wins over this variable, and `off` there turns it off |
| `GRIMOIRE_SYNC_PASSPHRASE` / `_FILE` | *(empty)* | Passphrase for the folder backup, used once to derive and store the key (a fresh device, or `grimoire sync folder` without a prompt). With `GRIMOIRE_SYNC_FOLDER` set, the server joins the backup at startup, or creates one if the folder has none. Prefer `_FILE` (mode 0600) |
| `GRIMOIRE_SYNC_FOLDER_INTERVAL` | `60` | Seconds between folder sync rounds (minimum 10). A local edit also triggers a round a few seconds later |
| `GRIMOIRE_DEVICE_NAME` | hostname | How this device is named in folder sync status |
| `GRIMOIRE_VAULT_IDLE_LOCK` | `900` | Credential-vault auto-lock (seconds) |
| `GRIMOIRE_VAULT_PASSPHRASE_FILE` | *(empty)* | Unlock the credential vault at startup from a `0600` file — for a headless server whose agents need the broker after every restart (see SECURITY.md) |
| `GRIMOIRE_BROKER_ALLOW_PRIVATE` | `0` | Allow brokered calls to private-range hosts |
| `GRIMOIRE_FRAME_OPTIONS` | `SAMEORIGIN` | X-Frame-Options (reverse-proxy embedding) |
| `GRIMOIRE_TRUST_PROXY` | `0` | Honour `X-Forwarded-For` / `-Proto` — set only when a proxy you control sets them, since they are otherwise caller-supplied |
| `GRIMOIRE_RATE_GENERAL` / `_EXPENSIVE` | `500` / `2` per second | Rate limits (burst = 20×). `GRIMOIRE_RATE_LIMIT=off` disables both |
| `GRIMOIRE_ADMIN_TOKEN` | *(empty)* | Gates the administrative surface — vault, connectors, accounts, settings — while notes and retrieval stay open. For instances that want to answer questions from a trusted network without handing it the levers |
| `GRIMOIRE_METRICS` | *(on)* | `off` removes `/metrics` |
| `GRIMOIRE_MCP_TRANSPORT` | `stdio` | `http` serves MCP over streamable-HTTP instead |
| `GRIMOIRE_MCP_ADDR` / `_PORT` | `127.0.0.1` / `9112` | Bind for the MCP http transport |
| `GRIMOIRE_MCP_TOKEN` | *(empty)* | Bearer token the MCP http transport demands. **Required to bind anything but loopback** — the server refuses to start otherwise, because that transport carries the vault and the credential broker. Clients send `Authorization: Bearer …`, or `?token=` when they cannot set headers |
| `GRIMOIRE_URL` | `http://127.0.0.1:$PORT` | API the MCP server talks to |
| `GRIMOIRE_MCP_TOOLS` | `all` | Which tools the MCP server advertises. `core` is six tools (`get_briefing`, `search_notes`, `read_note`, `remember`, `recall`, `use_credential`), about 1.4k tokens of schema against about 6.2k for all 35. Add others by name: `core,ask_notes,get_fact`. An unknown name stops the server rather than silently dropping a tool |
| `GRIMOIRE_PUBLIC_BASE` / `GRIMOIRE_OAUTH_AUTHORIZE_BASE` | *(unset = OAuth off)* | Enable OAuth 2.1 on `/mcp` for claude.ai/ChatGPT connectors — public tunnel base + private, owner-only consent base. See [web-connectors.md](web-connectors.md) |
| `GRIMOIRE_OAUTH_AUTHORIZE_ADDR` | `127.0.0.1:9115` | Bind address for the private consent listener |
| `GRIMOIRE_OAUTH_DB` | `~/.grimoire-mcp/oauth.db` | Registered OAuth clients and issued tokens (hashed) |
| `GRIMOIRE_OAUTH_ALLOWED_LOGINS` | *(empty)* | Tailscale logins trusted to auto-approve a connector without the admin token |
| `GRIMOIRE_OAUTH_ALLOWED_REDIRECTS` | claude.ai/chatgpt.com/openai.com + localhost | Redirect-URI host allowlist for Dynamic Client Registration; replaces the default |
| `GRIMOIRE_PLUGIN_DIR` | `plugins` | Where plugin bundles are loaded from |
| `GRIMOIRE_MODEL_DIR` | *(cache dir)* | Where the local embedding model is stored |
| `GRIMOIRE_EMBED_BASE_URL` / `_API_KEY` | *(empty)* | OpenAI-compatible embeddings endpoint |
| `GRIMOIRE_CONTEXT_BUDGET` | `100000` | Characters under which `ask`/`/api/context` hand over the WHOLE vault instead of retrieving (0 disables) |
| `GRIMOIRE_STALE_AFTER_DAYS` | `180` | When a note starts being reported stale and appears in the review queue. `0` turns the signal off, for a vault of writing that does not go stale |
| `GRIMOIRE_FOLLOW_SYMLINKS` | `0` | Index through directory symlinks, so a folder of markdown that lives elsewhere is part of the vault without being copied into it |
| `GRIMOIRE_NO_WATCHER` | `0` | Disable the filesystem watcher (tests/CI) |

AI/model settings can also be changed live in ⚙ Settings (persisted in the
vault, no restart). Editor mode (live/classic) and theme are per-device.


## Reranking

First-stage retrieval scores the query and each passage separately. A reranker
reads them together and reorders the candidates, which mostly helps questions
whose answer does not share many words with the question.

- **local** runs the `cross-encoder/ms-marco-MiniLM-L-6-v2` cross-encoder
  inside the server, in pure Go — no extra process, no GPU, and the binary
  stays a single static file. The weights are downloaded on first use into
  `.grimoire/models/` and verified against pinned checksums; set
  `HF_HUB_OFFLINE=1` (or point `GRIMOIRE_RERANK_MODEL` at a local copy) on a
  host that must not reach the network. `HF_ENDPOINT` selects a mirror. On
  amd64 CPUs with AVX2 it scores about 100 passages of ~60 tokens in well
  under half a second. Scores are raw relevance logits.
- **remote** posts `{model, query, documents, top_n}` to `GRIMOIRE_RERANK_URL`
  and reads `results[{index, relevance_score}]` — the shape most hosted rerank
  APIs and self-hosted inference servers accept. A server that asks for
  `texts` instead of `documents` is retried with that.

Like the other AI settings, all five can be set through `PUT /api/settings` and are
persisted in `.grimoire/settings.json`, which wins over the environment.

## Credentials

Stored in a sealed file, never in the index. The broker injects them into
outbound calls so an agent can use one without receiving it.

| Var | Default | Meaning |
|---|---|---|
| `GRIMOIRE_VAULT_PASSPHRASE_FILE` | *(empty)* | Unlock at start from a `0600` file, for a service that must recover from a restart unattended |
| `GRIMOIRE_VAULT_IDLE_LOCK` | `900` (seconds) | Drop the key after this long idle; `0` disables |
| `GRIMOIRE_BROKER_ALLOW_PRIVATE` | `0` | Let brokered calls reach private-range hosts. Cloud metadata and link-local stay refused either way |

Metadata the vault understands on each secret — set it with
`grimoire secret add --expires/--note/--rotate-days`, or in `meta` on the API:

| Key | Meaning |
|---|---|
| `expires` | RFC3339 or a bare `YYYY-MM-DD`. Reported as expiring within 14 days, then expired |
| `rotate_days` | Remind this many days after the last change, for credentials with no fixed expiry |
| `note` | What it is for |

`created`, `updated`, `last_used` and `uses` are maintained by the vault.
`GET /api/secrets/details` returns all of it and **no values**; `doctor` reports
anything expired or overdue; `grimoire secret check` exits non-zero so it works
from cron.

**Namespaces.** A secret name may contain `/` — `prod/stripe`, `dev/stripe`.
There are no folder objects to create or delete; a namespace is the part of a
name before a slash, so it exists exactly as long as something is in it.
Prefixes are matched on whole segments, so `prod` never selects
`production/…`. `grimoire secret list prod`, `grimoire secret check prod`,
`GET /api/secrets/details?prefix=prod`, and — the one that matters —
`grimoire run --prefix prod -- cmd`, which is the bounded form of `--all`: a
build that needs the production keys has no business being handed the rest.
Environment variables drop the namespace, so `prod/stripe` arrives as `STRIPE`.
On routes that carry the name in the path (`DELETE /api/secrets/{name}`,
`POST /api/secrets/{name}/grant`) the slash must be percent-encoded as `%2F` —
it is one path segment. Note that Python's `urllib.parse.quote` leaves `/`
alone unless you pass `safe=""`.

**Grant limits.** `max_uses` on a grant (and on an agent's request) bounds how
many times it may be redeemed; `0` is unlimited within the TTL, which is what
every grant was before. A time window alone bounds nothing about volume — a
fifteen-minute grant is fifteen minutes in which an agent may make any number
of calls — so for "post this one webhook" the honest bound is `1`. The check
and the increment are one statement, so two concurrent redemptions of a
one-shot grant cannot both succeed. A spent grant is retired on its next use
and reports `grant has no uses left` rather than `unknown or revoked grant`:
an agent that used up its grant and an agent holding a bad token want opposite
responses.

Up to 10 previous values are retained per secret and sealed with the current
one. `GET /api/secrets/versions?name=X` lists when and why each changed, never
what it was; `POST /api/secrets/restore` makes one current again, keeping the
value it replaced so the rollback is itself undoable.

## Identifying callers on other devices

Off unless `GRIMOIRE_IDENTITY` names a backend. With it unset, callers are
attributed by the name they send about themselves and nothing changes.

An overlay network already authenticated the caller before Grimoire saw the
connection, so asking it turns attribution from a claim into a fact. That name
reaches the usage ledger, the read-audit trail, and the authority lattice that
decides whether a human's correction outranks an agent's rewrite.

| Var | Default | Meaning |
|---|---|---|
| `GRIMOIRE_IDENTITY` | *(empty)* | Comma-separated backends, in order: `tailscale` (or `headscale`), `zerotier`, `mtls`, `proxy`. First one able to answer wins |
| `GRIMOIRE_IDENTITY_TTL` | `5m` / `2m` | How long a lookup is cached, so identity is not a round-trip per request |
| `GRIMOIRE_TAILSCALE_ENDPOINT` | `unix:///var/run/tailscale/tailscaled.sock` | tailscaled's LocalAPI. An `http://` base for a containerised daemon |
| `GRIMOIRE_TAILSCALE_RANGES` | `100.64.0.0/10`, `fd7a:115c:a1e0::/48` | Which peers are looked up at all |
| `GRIMOIRE_ZEROTIER_API` | `https://api.zerotier.com/api/v1` | Central, or `http://localhost:9993` for a self-hosted controller |
| `GRIMOIRE_ZEROTIER_NETWORK` | *(empty)* | Network id. Required — without it the backend is inert |
| `GRIMOIRE_ZEROTIER_TOKEN` / `_FILE` | *(empty)* | Central API token, or a self-hosted controller's `authtoken.secret` |
| `GRIMOIRE_ZEROTIER_RANGES` | *(empty)* | The network's assigned pool. Strongly recommended: it is what establishes that a packet arrived over ZeroTier |
| `GRIMOIRE_MTLS_FIELD` | `cn` | Which certificate field names the caller: `cn`, `dns`, `email`, `uri` |
| `GRIMOIRE_IDENTITY_PROXY_FROM` | *(empty)* | Addresses entitled to assert identity. **Required** — with nobody trusted the proxy backend refuses to run, because a header anyone can set is worse than no identity |
| `GRIMOIRE_IDENTITY_PROXY_HEADER` | `Remote-User` | The header carrying the user |
| `GRIMOIRE_IDENTITY_PROXY_DEVICE_HEADER` | *(empty)* | Optional header carrying the machine |

`GET /api/identity` reports which backends are running, how this caller was
identified, what it *claimed* to be, and the name that will actually be
recorded. Check it after configuring: the failure mode here is silence — a
backend that never matches looks exactly like one that works.

**Two things this deliberately does not do.** It never reads a forwarded
header to decide who is calling, even with `GRIMOIRE_TRUST_PROXY=1`: every
backend is address-based, so a caller-supplied address would let anyone claim
any node. And a verified identity does not sign itself in — it authorizes
nothing until you map it to an account with
`grimoire user map <backend> <subject> <user>`. Knowing truthfully who is
calling is not a decision about what they may read.

## Diagnosing

## Terminal and MCP knowledge interfaces

`grimoire query QUESTION` is a one-shot knowledge query. With no question it
reads newline-separated questions from stdin, or opens a repeatable console on
a terminal; Ctrl-D, Ctrl-C, `:q`, and `:quit` exit cleanly. `--plain` prints
the answer, citations, and typed relationship/evidence navigation; `--json`
preserves one API response per question for pipes. `:source PATH` and
`:graph [SEED]` navigate cited evidence in the console. `--watch FOLDER` keeps
the imported corpus current while querying. `grimoire graph`, `grimoire source
PATH`, and `grimoire documents` provide one-shot navigation. `grimoire
documents watch FOLDER` performs recursive initial/update/delete reconciliation;
`documents refresh PATH` refreshes an importer-owned note. `grimoire
document-import FILE` uploads file bytes through the document API and never
treats a client-supplied path as a server path. Query filters include `depth`,
`limit`, `after`, `before`, and `expand`. Graph filters include `depth`,
`limit`, `seed`, `relation`, `q`, `min_degree`, `drop_noisy`,
`include_documents`, and `include_chunks`.

Semantic relationship extraction is explicit: `grimoire knowledge extract
PATH... [--force]` sends at most ten existing note paths to the configured LLM.
It reports `indexed`, `cached`, or `error` per path and exits non-zero when any
file fails. The equivalent MCP tool is `extract_relationships`; it is
model-spending and cache-writing. `knowledge_graph` never spends model calls:
it returns structural relationships and includes cached semantic relationships
when extraction has already populated them. `POST /api/knowledge/extract` is
the HTTP contract for the same operation.

The MCP server exposes the same backend through stdio by default. Set
`GRIMOIRE_MCP_TRANSPORT=http` for streamable HTTP, `GRIMOIRE_MCP_ADDR` (or
`GRIMOIRE_MCP_PORT`) for its bind address, and `GRIMOIRE_MCP_TOKEN` for a
non-loopback bind. To add `/mcp` as a claude.ai or ChatGPT connector — which
can only authenticate with OAuth, never a static token — set
`GRIMOIRE_PUBLIC_BASE` and `GRIMOIRE_OAUTH_AUTHORIZE_BASE`; see
[docs/web-connectors.md](web-connectors.md) for the full setup and the rest
of the `GRIMOIRE_OAUTH_*` variables. Knowledge parity tools are `query_knowledge`,
`knowledge_graph`, `read_source`, and `extract_relationships`; document parity tools are
`list_documents`, `refresh_document`, and `import_document`. The latter accepts a filename and
base64-encoded bytes, not an arbitrary local path.

`grimoire doctor` compares the vault, the index and what an agent can
reach, and reports the pairs that disagree. It exits non-zero on a failure, so
it works from a healthcheck or a unit file as well as from a terminal.

The checks exist because these failures are silent: `/api/health` returns
`ok: true` while memory is unqueryable, while the index has drifted from the
vault, or while the credential vault is locked and every `use_credential` call
is failing.


## AI usage accounting

Every model call Grimoire makes is recorded in the index — provider, model,
surface, agent, tokens, latency and cost — and reported at `GET /api/usage`
and `GET /api/usage/agents`.

This covers Grimoire's OWN calls only. It is mounted by agents and never sees
an agent's conversation with its provider, so it cannot and does not report
that spend.

Prices are a reference table checked on the date the API returns as
`prices_updated`. Reconcile against your provider's invoice; providers change
rates and negotiate them.

Connected accounts (mail, calendar, Drive, Slack, GitHub) and the agent tools that use them: [CONNECTORS.md](CONNECTORS.md).


## Agent memory, hooks and gate

Settings added by the agent-memory work, plus the hook-side variables read by the scripts `grimoire agent install --memory` places in `~/.grimoire/hooks/`. Server settings can also be set in the console; the environment wins.

| Variable | Default | What it does |
|---|---|---|
| `GRIMOIRE_CONTEXT_GATE_URL` | *(empty = off)* | Decision server (Jev wire format) that vets borderline memories before they are injected. Off by default; see [MEMORY_ADHERENCE.md](MEMORY_ADHERENCE.md#gate-when-not-to-inject) |
| `GRIMOIRE_CONTEXT_GATE_MODEL` | `jev-latest` | Model name sent to the gate |
| `GRIMOIRE_CONTEXT_GATE_BAND` | `0.5,0.75` | Relevance range that is asked: above it injects, below never does |
| `GRIMOIRE_CONTEXT_GATE_MAX` | `3` | Most candidates asked per request, highest first, in parallel under a 400 ms total budget |
| `GRIMOIRE_CONTEXT_GATE_THRESHOLD` | `0.16` | Yes-probability needed to keep a memory (calibrated for the strict question wording; 0.5 would drop most right memories) |
| `GRIMOIRE_RULES_MIN_PRECISION` | `0.9` | Precision a compiled rule check needs (with enough labelled matches) to become an active reminder ([MEMORY_RULES.md](MEMORY_RULES.md)) |
| `GRIMOIRE_RULES_ENFORCE_PRECISION` | `0.95` | Precision a check needs for `enforce: ask` automatically, when the rule text says never/always |
| `GRIMOIRE_RULES_MIN_LABELLED` | `5` | Labelled matches required before a check can act |
| `GRIMOIRE_RULES_MAX_MATCH_RATE` | `0.2` | A check flagging more than this share of the calls it covers is a topic detector and stays a suggestion |
| `GRIMOIRE_RULES_FAITHFUL_THRESHOLD` | `0.5` | P(yes) the labelling model must give to "does this check say what the rule says" before the check's matches are labelled; below it the check stays a suggestion |
| `GRIMOIRE_RULES_LLM_URL` | *(empty = deterministic only)* | Ollama that writes candidate checks (local; no hosted model) |
| `GRIMOIRE_RULES_LLM_MODEL` | `qwen3.6:35b-a3b` | Model asked for candidates (`num_ctx` 16384) |
| `GRIMOIRE_RULES_LABEL_URL` | *(falls back to the decision model)* | Decision server that labels sampled backtest matches |
| `GRIMOIRE_RULES_LABEL_MODEL` | `jev-latest` | Model name sent for labelling |
| `GRIMOIRE_RULES_LABEL_API_KEY` | *(falls back to the decision key)* | Key for the labelling server; keep it in the environment |
| `GRIMOIRE_RULES_LABEL_BUDGET` | `1500` | Most new labelling calls per backtest run (answers are cached on disk) |
| `GRIMOIRE_RULES_LABEL_THRESHOLD` | `0.5` | P(yes) at or above which a labelled match counts as a true violation |
| `GRIMOIRE_MEMORY_AUTHORITY` | *(on)* | `off` puts human and agent writes on one rung (recency-only supersession), as a benchmark control arm. Untrusted-source protection is not affected |
| `GRIMOIRE_MEMORY_CANONICAL_DIR` | *(empty)* | The shared agent-memory directory every agent's memory location links to; falls back to `GRIMOIRE_DREAM_CANONICAL_MEMORY`. See [MEMORY_STORE.md](MEMORY_STORE.md) |
| `GRIMOIRE_MEMORY_EXTRACT_PROMPT` | *(empty)* | Prefix added to the memory-extraction prompt (biases what is recorded); the output contract is appended and cannot be overridden |
| `GRIMOIRE_MEMORY_DECIDE_PROMPT` | *(empty)* | Same, for the add/update/ignore reconciliation prompt |
| `GRIMOIRE_MEMORY_VERIFY_PORTS` | *(empty)* | Comma-separated ports a procedure check may probe; empty probes none |
| `GRIMOIRE_MEMORY_OTHER_HOSTS` | *(empty)* | Other hosts a procedure check may probe on those ports |
| `GRIMOIRE_MEMORY_VERIFY_PER_DREAM` | `3` | How many due procedures one dream verifies |
| `GRIMOIRE_DREAM_REPORT_DIR` | `Dreams` | Vault folder the dream report note is written to |
| `GRIMOIRE_DREAM_PROJECTS_DIR` | *(empty)* | An agent's per-project directory (e.g. `~/.claude/projects`); with the canonical memory directory, a dream reports projects whose `memory/` is a separate real directory |
| `GRIMOIRE_DREAM_CANONICAL_MEMORY` | *(empty)* | The memory directory every project is meant to share |
| `GRIMOIRE_PROVENANCE_GATE` | *(on)* | `off` disables the credential broker's provenance check (a security control; leave on) |
| `GRIMOIRE_PROVIDER_CACHE_SECONDS` | `0` | How long a password-manager secret is cached after a fetch, unless the provider sets `cache_seconds`. `0` fetches every time. See [PASSWORD_MANAGERS.md](PASSWORD_MANAGERS.md) |
| `GRIMOIRE_CONNECT_TOKEN` | *(empty)* | Token for `grimoire connect slack|github` when `--token` is not given (keeps it out of shell history). See [CONNECTORS.md](CONNECTORS.md) |
| `GRIMOIRE_READ_AUDIT_DAYS` | `90` | Days of read-audit history kept; `0` keeps everything |
| `GRIMOIRE_RATE_EXPENSIVE` | `2` | Per-second limit for costly routes (see `GRIMOIRE_RATE_GENERAL`) |
| `GRIMOIRE_MCP_CORE` | *(on)* | `0` stops the MCP server appending the standing memory core to its instructions ([AGENTS_ANY.md](AGENTS_ANY.md)) |
| `GRIMOIRE_AGENT_PROFILE` | *(empty)* | Hook side: the agent profile `grimoire_context.py` loads from `~/.grimoire/agents/NAME.json` (same as `--agent NAME`) |
| `GRIMOIRE_AGENT_DIR` | `~/.grimoire/agents` | Hook side: where resolved agent profiles are read from |
| `GRIMOIRE_AUTO_CONTEXT` | `1` | Hook side: `0` turns off the context and outcome hooks |
| `GRIMOIRE_CONTEXT_MODE` | `manual` | Hook side: `all` or `scoped` enables injection (`grimoire agent install --memory` sets `all`); `manual` injects nothing |
| `GRIMOIRE_CONTEXT_PATHS` | `[]` | Hook side: JSON list of note paths/prefixes injection is limited to when the mode is `scoped` |
| `GRIMOIRE_CONTEXT_ACTIONS` | `1` | Hook side: `0` stops memory being restated before tool calls |
| `GRIMOIRE_CONTEXT_MAX_BYTES` | `2400` | Hook side: budget of one prompt-time injection (128-8000) |
| `GRIMOIRE_ACTION_MAX_BYTES` | `1200` | Hook side: budget of one action-time injection (128-4000) |
| `GRIMOIRE_CONTEXT_RANK` | `hybrid` | Hook side: `hybrid` (embeddings + keywords + cues) or `lexical` (word overlap) |
| `GRIMOIRE_CONTEXT_MIN_REL` | `0.5` | Hook side: relevance floor for prompt-time injection |
| `GRIMOIRE_ACTION_MIN_REL` | `0.7` | Hook side: relevance floor before a command |
| `GRIMOIRE_EDIT_MIN_REL` | `0.8` | Hook side: relevance floor before a file edit |
| `GRIMOIRE_CONTEXT_QUERY_PREFIX` | *(empty)* | Hook side: text prepended to the prompt as the retrieval query |
| `GRIMOIRE_CONTEXT_STATE_DIR` | `~/.cache/grimoire/context` | Hook side: dedupe state |
| `GRIMOIRE_CONTEXT_DEBUG` | *(off)* | Hook side: `1` prints a note to stderr when the hook cannot reach the server |
| `GRIMOIRE_OUTCOME` | `1` | Hook side: `0` turns off adherence reporting (`grimoire_outcome.py`) |
| `GRIMOIRE_OUTCOME_ENFORCE` | `0` | Hook side: `1` makes the outcome hook ask the server on `PreToolUse` and emit a permission `ask` for `enforce: ask` rules. The context hook already does this; prefer it ([MEMORY_ADHERENCE.md](MEMORY_ADHERENCE.md)) |
| `GRIMOIRE_BANK_SESSIONS` | *(off)* | Hook side: `1` retains session transcripts into the bank (set by `agent install`) |
| `GRIMOIRE_BANK_CONTEXT` | *(off)* | Hook side: `1` adds the bank's context at session start |
| `GRIMOIRE_BANK_RECALL` | *(off)* | Hook side: `1` recalls from the bank on each prompt |
| `GRIMOIRE_BANK_RECALL_TOKENS` | `1024` | Hook side: token budget of that recall (64-4096) |
| `GRIMOIRE_BANK_FILES` | *(off)* | Hook side: `1` adds what the bank knows about a file being read |
| `GRIMOIRE_BANK_TOOLS` | *(off)* | Hook side: `1` records tool use for the end-of-session digest |
| `GRIMOIRE_BANK_DIGEST` | `1` | Hook side: `0` skips the end-of-session digest |
| `GRIMOIRE_BANK_DIGEST_MODEL` | `1` | Hook side: `0` builds the digest without a model |
| `GRIMOIRE_BANK_HARNESS` | *(empty)* | Hook side: the agent profile name stamped on retained sessions |
| `GRIMOIRE_BANK_TIMEOUT` | `10` | Hook side: seconds before a bank request is abandoned (1-60) |
| `GRIMOIRE_BANK_STATE_DIR` | `~/.cache/grimoire/bank-sessions` | Hook side: per-session state |
| `GRIMOIRE_BANK_DEBUG` | *(off)* | Hook side: `1` prints hook errors to stderr |
| `GRIMOIRE_HOOK_MAX_CHARS` | `9000` | Hook side: most characters one bank injection may render |
| `GRIMOIRE_INSTALLED_BY` | `grimoire agent install` | Marker in an agent's MCP entry that tells uninstall the entry is ours; do not set by hand |
| `GRIMOIRE_EMBED_API_KEY` | *(empty)* | Key for `GRIMOIRE_EMBED_BASE_URL` |
| `GRIMOIRE_LLM_API_KEY` | *(empty)* | Key for `GRIMOIRE_LLM_BASE_URL` (see above) |
| `GRIMOIRE_LOCAL_EMBED_MODEL` | `minishlab/potion-base-8M` | Hub id of the local embedding model |
| `GRIMOIRE_RERANK_API_KEY` | *(empty)* | Key for the remote reranker |
| `GRIMOIRE_RERANK_MODEL_DIR` | *(unset)* | Tests only: a local cross-encoder snapshot for the parity test |
| `GRIMOIRE_WEB_SEARCH_KEY` | *(empty)* | Web search key; may be `vault:NAME` to name a vault credential |
| `GRIMOIRE_WEB_SEARCH_CX` | *(empty)* | Google programmable-search id (provider `google`) |
| `GRIMOIRE_PUBLISH` | *(off)* | Set to `1` to export and serve the published site |
| `GRIMOIRE_SYNC_TOKEN` | *(empty)* | Bearer token peers use on the peer-sync routes |
| `GRIMOIRE_SYNC_INTERVAL` | `0` | Seconds between automatic peer syncs; `0` is manual only |
| `GRIMOIRE_SYNC_PASSPHRASE_FILE` | *(empty)* | File holding the cloud-folder sync passphrase (else `GRIMOIRE_SYNC_PASSPHRASE`) |
| `GRIMOIRE_TLS_PORT` | *(off)* | Serve HTTPS on this port on the tailnet addresses with a Tailscale certificate |
| `GRIMOIRE_WEB_DIR` | *(auto)* | Directory of the built web console; searched beside the binary when unset |
| `GRIMOIRE_ZEROTIER_TOKEN_FILE` | *(empty)* | File holding the ZeroTier API token (else `GRIMOIRE_ZEROTIER_TOKEN`) |

`BW_SESSION` is not a Grimoire setting: it is the Bitwarden CLI's own variable, which a provider is pointed at with `--secret-env session=BW_SESSION` ([PASSWORD_MANAGERS.md](PASSWORD_MANAGERS.md)).
