<div align="center">

# ✦ Grimoire

**You already wrote it down. Your agent still can't see it.**

Point it at the markdown vault you already have. Your agents read what you know,
remember what they learn back into the same files, and act with credentials they
can use but never see. One self-hosted Go binary, mounted over MCP.

[![CI](https://github.com/JeremiahM37/grimoire/actions/workflows/ci.yml/badge.svg)](https://github.com/JeremiahM37/grimoire/actions/workflows/ci.yml)
![license](https://img.shields.io/badge/license-MIT-blue)
![go](https://img.shields.io/badge/go-1.26%2B-00add8)
[![benchmarks](https://img.shields.io/badge/benchmarks-pre--registered%2C%20nulls%20included-b4741a)](benchmarks/)

![Grimoire console](docs/screenshots/hero.png)

</div>

```bash
go install github.com/JeremiahM37/grimoire/go/cmd/grimoire@latest
go install github.com/JeremiahM37/grimoire/go/cmd/grimoire-mcp@latest

GRIMOIRE_VAULT=~/obsidian-vault grimoire serve &   # the folder you already have
claude mcp add grimoire -- grimoire-mcp            # your agent now has all of it
```

Or `docker run -p 9111:9111 -v grimoire-vault:/vault ghcr.io/jeremiahm37/grimoire:latest`.
Releases ship static binaries for Linux, macOS and Windows on amd64/arm64.

## Point your agent at the notes you already have

Every agent-memory layer starts empty. mem0, Zep and Letta accumulate what an
agent learns from talking to you — useful, and not the problem. The runbooks and
decisions you have been writing for years already answer most of what your agent
asks, and it cannot see any of them. So you paste. Again.

Grimoire's substrate is a folder of markdown you already own — an **Obsidian**
vault, a Logseq graph, a plain `~/notes`. It needs no plugin and does not need
Obsidian running, because it reads the files, not the app. Nothing is copied
or converted; the watcher picks up edits you make in your own editor, and writes
through Grimoire preserve foreign frontmatter byte-for-byte, so whatever notes
app you use keeps working on the same files.

Everything else follows from that one decision.

## Agent memory that lives in your own markdown

What an agent learns lands in those files too, as ordinary bullets with
provenance. When it gets something wrong you fix the line — and the fix
**outranks the agent's next write**, which is not true elsewhere.

![Agent memory corrected by hand](docs/screenshots/memory-demo.gif)

Most memory layers let you edit; Letta has a block editor, mem0 an update API.
But an edit with no recorded *author* has no standing, so it holds only until
the next write lands on that slot. Reconciliation here compares authority before
recency — `human` > `agent` > `pulled` — and a refused overwrite becomes a
challenge you settle rather than a silent revert.

```bash
grimoire challenges                                    # what your agents dispute
grimoire challenges --note memory/ops.md --uphold ID   # your fact stands
grimoire challenges --note memory/ops.md --concede ID  # the agent was right
```

Hand edits need no marker: an entry's id is a hash of its own content, so text
that changed after the id was minted is text another hand changed.

## Credentials it can use but never read

An encrypted vault (Argon2id + Fernet). You mint a scoped, time-boxed grant; the
server injects the secret into the outbound call and returns the response. The
key never enters the agent's context, so it cannot be logged, memorised or
extracted by prompt injection — and revoking is one row, not a key rotation.
Agents without a grant can *ask*; asking grants nothing.

## MCP tools: what Claude gets in one mount

<!-- tools:begin (checked against the server by a test) -->

| | tools |
|---|---|
| **Credentials — use, never read** | **`use_credential`** · **`list_grants`** · **`request_credential`** · **`check_credential_request`** |
| **Agent memory** | **`remember`** · **`recall`** · **`forget`** · **`memory_changes`** · **`memory_graph`** · **`memory_feedback`** · **`memory_scopes`** · **`consolidate_memory`** |
| Knowledge | `search_notes` · `ask_notes` · `read_note` · `list_notes` · `backlinks` · `list_tags` · `stale_notes` |
| The web | `search_web` · `open_urls` |
| Writing | `create_note` · `update_note` · `append_daily` |
| Exact values | `get_fact` · **`set_fact`** |
| Orientation | `get_briefing` · `kb_info` |

<!-- tools:end -->

Any MCP client works. `grimoire agent-setup` prints the config plus a
CLAUDE.md/AGENTS.md snippet, since agents read context files more reliably than
they browse tool lists.

```jsonc
{ "mcpServers": { "grimoire": {
    "command": "/path/to/grimoire-mcp",
    "env": { "GRIMOIRE_URL": "http://localhost:9111",
             "GRIMOIRE_AGENT_NAME": "my-agent" } } } }
```

Retrieval is inspectable — *"what would the agent see for X?"* returns the exact
chunks. Untrusted content (connectors, web pages) carries an origin, is fenced
before a reader sees it, and may not supersede something you wrote.

## Cloud agents too, not just local ones

A local agent launches `grimoire-mcp` over stdio. A hosted one — Claude.ai,
ChatGPT, Codex, DeepSeek — cannot, so the same server speaks **streamable HTTP**:

```bash
GRIMOIRE_MCP_TRANSPORT=http \
GRIMOIRE_MCP_ADDR=0.0.0.0:9112 \
GRIMOIRE_MCP_TOKEN=$(openssl rand -hex 32) grimoire-mcp
```

One implementation, two doors — a test asserts the transports answer
identically, so they cannot drift.

**It refuses to bind anything but loopback without a token.** That transport
carries `remember`, `create_note` and the credential broker, so an
unauthenticated public bind would publish the vault *and* the ability to spend
its secrets. Put it behind your own TLS (a reverse proxy, `tailscale serve`, or
a tunnel) and give the client the URL plus the token.

## Know which agent is actually asking

Once agents run on more than one machine, the name on a memory stops being a
detail. The authority lattice, the read-audit trail and the cost report are all
keyed on who said something — and that name was a header the caller set about
itself.

An overlay network already authenticated the caller before Grimoire saw the
connection, so ask it:

```bash
GRIMOIRE_IDENTITY=tailscale grimoire      # or zerotier, mtls, proxy
```

`GET /api/identity` then reports the verified caller, what it *claimed* to be,
and the name that will actually be recorded — the three things you need to tell
a working configuration from one that silently never matches.

Off unless you set it, and it is deliberately two separate decisions. A
verified identity always replaces the self-asserted name for **attribution**.
It grants **access** only where you mapped it to an account:

```bash
grimoire user map tailscale jam@github jam
```

Identity never comes from a forwarded header, even behind a trusted proxy — a
caller that could name its own address could claim any node on the overlay.

## Run the credential vault, don't just fill it

The broker is the point: an agent gets a scoped, expiring grant and the server
makes the call, so the value never reaches the agent. But a store you cannot
operate is a store nobody rotates, and an unrotated credential is the one that
leaks. So the operations are there too:

```bash
grimoire secret add stripe --expires 2026-11-30 --note "billing"
grimoire secret check          # non-zero if anything expired or is due
grimoire secret history stripe # what it used to be, and why it changed
grimoire secret restore stripe # put it back
grimoire secret scan           # credentials pasted into notes instead of stored
```

Every write keeps the value it replaced, so **rotation is no longer a one-way
door** — paste the new key, find out the service was not ready, put the old one
back. History is sealed with everything else and is never returned: you can see
*when* and *why* a value changed, never what it was.

`grimoire secret scan` reads your notes, not the vault. A key pasted into a
note while debugging is the likeliest way a credential escapes a system whose
substrate is markdown you sync to your phone, and findings are masked — a
report that quoted the key would copy the leak somewhere new.

Grants are bounded in count as well as time: `max_uses: 1` for "post this one
webhook" is a tighter thing to hand out than fifteen minutes in which an agent
may make any number of calls. Names can carry a namespace (`prod/stripe`), and
`grimoire run --prefix prod -- cmd` is the bounded form of `--all` — a build
that needs the production keys has no business being handed the rest.

`grimoire run NAME -- cmd` puts a value in a child's environment. That hands
over the value, which is exactly what the broker avoids, so it is for your own
commands — agents get grants.

## Pull in what you already wrote elsewhere

Ten connectors write into the vault as ordinary markdown with provenance in the
frontmatter — not a parallel document store, so search, retrieval and the editor
work on them for free and they survive Grimoire being uninstalled.

| | |
|---|---|
| **Chat** | Slack · Discord |
| **Docs** | Notion · Confluence · Google Drive |
| **Tickets** | Linear · Jira · GitHub issues |
| **Reading** | Readwise · RSS/Atom |

Pulled content carries `trust: untrusted`, is fenced before a reader sees it,
and may not supersede something you wrote.

## What the AI here has cost

`grimoire doctor` tells you the vault is healthy; **AI usage** (command palette,
or `GET /api/usage`) tells you what it spent getting there — by provider, by
model, by which part asked, and by which agent triggered it.

**Read the scope before the number.** This is *not* your total AI spend.
Grimoire is mounted **by** agents and never sees the conversation an agent has
with its own provider, so it cannot know what your coding agent costs. What it
reports exactly is the calls **it** made: answering, reranking, classifying, on
a key you configured. Anything else would be invented.

Seventeen providers are priced — OpenAI, Anthropic, Google, Groq, Together,
Fireworks, DeepSeek, Mistral, Perplexity, xAI, Cerebras, DeepInfra, Azure,
OpenRouter — plus Ollama, LM Studio and vLLM, which are free because they run on
your hardware. The provider is identified from the API base URL, not the
configured backend name, because pointing the OpenAI-compatible backend at Groq
means Groq is billing you.

A model with no price on file reports **unknown**, never `$0.00`, and the total
reads "at least" — a zero presented as a total makes an unmetered provider look
free, which is the expensive direction to be wrong in.

## Also a self-hosted notes app

Not wiring up agents yet? It is a full offline PWA in its own right — CodeMirror
live preview, wiki-links, backlinks, graph, daily notes, transclusion, canvas,
query blocks, templates. Mount an existing vault and daily-drive it; the agent
substrate is there when you want it.

## Measured

Pre-registered protocols, nulls and corrections published alongside — including
one that cost a feature its default. Full methods and per-question data in
[benchmarks/](benchmarks/).

| | result |
|---|---|
| **LongMemEval** — hybrid retrieval | **77.5%**, +8.5 over dense-only (p=0.0005) and over full-context at **15× fewer tokens** |
| **Correction durability** | recency-only loses **20/20** hand corrections; authority lattice keeps **20/20** |
| **Update recognition** | 17/37 held-out knowledge updates, up from 14/37, at no cost in false supersessions |
| **Prompt injection** | 0/40 injected instructions obeyed when fenced — *but the pre-declared bar was not met; see the report* |

## Config, security and docs

- **Config** — every knob is an env var: [docs/CONFIG.md](docs/CONFIG.md).
  Nothing is required; an empty environment gives a working server.
- **Security** — threat model, what is and is not defended: [SECURITY.md](SECURITY.md).
- **Architecture** — [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) ·
  **design decisions** — [DESIGN.md](DESIGN.md) ·
  **plugins** — [docs/PLUGINS.md](docs/PLUGINS.md)
- **Diagnosing** — `grimoire doctor` compares the vault, the index and what an
  agent can actually reach, and names the fix for whatever disagrees. Exits
  non-zero, so it works from a healthcheck too.
- **Tests** — `cd go && go test ./...`, plus a `verify` suite that drives a real
  headless browser against a live server.
- `grimoire help` lists the CLI. `grimoire eval` measures retrieval on *your*
  vault rather than on a public corpus.

MIT.
