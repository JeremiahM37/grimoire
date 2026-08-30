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
| `GRIMOIRE_OLLAMA_URL` | *(empty)* | Reachable Ollama → generative ask/summarize |
| `GRIMOIRE_LLM` / `GRIMOIRE_LLM_MODEL` | auto / `qwen3.5:4b` | Answer backend (`ollama` · `claude` · `openai`) + model |
| `GRIMOIRE_LLM_BASE_URL` / `_API_KEY` | *(empty)* | Any OpenAI-compatible endpoint (OpenAI, OpenRouter, Together, Groq, vLLM, LM Studio, LiteLLM…); key can also live in the vault as `llm-api-key` |
| `GRIMOIRE_EMBED_MODEL` | `nomic-embed-text` | Embeddings (offline hashing fallback built in) |
| `GRIMOIRE_LOCAL_EMBED` / `_MODEL` | `auto` / `potion-base-8M` | Local semantic embeddings — the ~30 MB model is fetched once on first start (`grimoire fetch-model` to pre-seed); `off` to stay on the hashing embedder |
| `GRIMOIRE_WHISPER_URL` / `_MODEL` | *(empty)* | Audio-memo transcription |
| `GRIMOIRE_WEB_SEARCH_PROVIDER` | *(off)* | `searxng` · `brave` · `serper` · `google` — enables `search_web` / `open_urls` |
| `GRIMOIRE_WEB_SEARCH_URL` / `_KEY` / `_CX` | *(empty)* | SearXNG base URL · provider key (or `vault:name` to read it from the credential vault) · Google engine id |
| `GRIMOIRE_DAILY_DIR` / `GRIMOIRE_INBOX_DIR` | `journal` / `inbox` | Vault sub-folders |
| `GRIMOIRE_SYNC_PEER` / `_TOKEN` / `_INTERVAL` | *(off)* | Background sync with a peer |
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
| `GRIMOIRE_PLUGIN_DIR` | `plugins` | Where plugin bundles are loaded from |
| `GRIMOIRE_MODEL_DIR` | *(cache dir)* | Where the local embedding model is stored |
| `GRIMOIRE_EMBED_BASE_URL` / `_API_KEY` | *(empty)* | OpenAI-compatible embeddings endpoint |
| `GRIMOIRE_CONTEXT_BUDGET` | `100000` | Characters under which `ask`/`/api/context` hand over the WHOLE vault instead of retrieving (0 disables) |
| `GRIMOIRE_STALE_AFTER_DAYS` | `180` | When a note starts being reported stale and appears in the review queue. `0` turns the signal off, for a vault of writing that does not go stale |
| `GRIMOIRE_FOLLOW_SYMLINKS` | `0` | Index through directory symlinks, so a folder of markdown that lives elsewhere is part of the vault without being copied into it |
| `GRIMOIRE_NO_WATCHER` | `0` | Disable the filesystem watcher (tests/CI) |

AI/model settings can also be changed live in ⚙ Settings (persisted in the
vault, no restart). Editor mode (live/classic) and theme are per-device.


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
