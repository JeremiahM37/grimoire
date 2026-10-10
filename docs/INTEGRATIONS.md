# Integrations

Which agent runtimes Grimoire reaches, by what mechanism, and what is
automatic versus left to the agent. There is no universal adapter and no
universal enforcement: the strongest mechanism differs by runtime, and in
every case Grimoire can put information in front of a model but cannot make
it use that information.

## Two kinds of integration

- **Automatic (host-driven).** Something outside the model runs a lookup
  before each request and places the result in the prompt: a hook, a dynamic
  instructions callable, or your own call to `context_for`. This is read-only,
  bounded (default 2,400 bytes), model-free, and opt-in. The model did not
  decide to look; whether it *uses* what it was given is still up to it.
- **Agent-initiated.** The agent is given tools (MCP, function tools, a store
  interface) and calls them if it chooses to. Reliable writes and corrections
  live here; nothing guarantees the agent reaches for them.

## Support matrix

| Runtime | Strongest mechanism | Automatic | Agent-initiated | Status |
|---|---|---|---|---|
| **Claude Code** | `UserPromptSubmit` hook (`clients/hooks/grimoire_context.py`) + MCP | Bounded context before each prompt; session reset on clear/compact/resume | All MCP tools (`recall`, `remember`, `search_notes`, ...) | Hook and MCP shipped. Hook unit-tested; installer: `clients/hooks/install.py` |
| **Codex CLI** | Same hook via `.codex/hooks.json`, plus an `AGENTS.md` snippet (`grimoire agent-setup`) + MCP | Same as Claude Code | MCP tools; the snippet only *asks* the agent to use them | Hook protocol checked against Codex's published hook docs and the installed `hooks.json` format (see [AUTOMATIC_MEMORY.md](AUTOMATIC_MEMORY.md#codex-protocol-check)); not exercised against a live Codex turn in this repo's tests |
| **LangGraph** | `GrimoireStore`, a `BaseStore` (`grimoire_client.langgraph`) | None: the graph code decides when to read the store | `put` / `get` / `search` as the graph calls them | Tested against real `langgraph` and a live server (skipped when either is absent) |
| **CrewAI** | `GrimoireStorage` + `GrimoireEmbedder` (`grimoire_client.crewai`) | CrewAI's own memory recall, using the server's embedding space | CrewAI decides what to save | Tested against real `crewai` and a live server (skipped when either is absent) |
| **OpenAI Agents SDK** | `grimoire_instructions` + `grimoire_tools` (`grimoire_client.openai_agents`) | Context appended to the instructions each model call | `recall`, `remember`, `search_notes` function tools | Fakes plus a signature/schema check against the SDK; no live model run |
| **Plain chatbots / custom loops** | `context_for` / `ContextSession` (Python), `contextFor` (JS) | You call it before the model; it returns a string | Whatever tools you wire up | Unit-tested against a stub of the endpoint |
| **Obsidian** | Plugin in `clients/obsidian` | Badges and panels in the editor | You act on disputes and corrections | Developed on a separate branch (`feat/obsidian-plugin`); not part of this tree |
| **LibreChat and other MCP chat UIs** | MCP server | None unless the host calls `/api/memory/context` | MCP tools, subject to the host's tool-approval UI | MCP works with any compliant client; no first-party automatic injection |
| **Anything else** | Python / JS clients, or HTTP | You choose | You choose | Python and JS clients in `clients/`; there is no Go client package in this tree, use the HTTP API |

All of the automatic rows use one endpoint, `GET /api/memory/context`
(see [AUTOMATIC_MEMORY.md](AUTOMATIC_MEMORY.md#api-for-any-host)), and the same
rules: acknowledgements ("thanks") cost no request, an identical query is
skipped for 30 seconds, a fact is not repeated for 30 minutes, the response is
checked against the byte budget and at most 10 keys of 32 hex characters, and
any failure yields no context rather than an error.

## Limits, stated plainly

- **Hooks inject; they do not compel.** Context in the prompt is a suggestion
  to the model. An agent can ignore it, misread it, or act on stale text that
  was injected earlier in the session.
- **Lexical selection.** Automatic retrieval is word-overlap, not semantic. A
  paraphrase with no shared words is missed. Explicit tools are the fallback.
- **Opt-in by design.** The hook does nothing until `GRIMOIRE_CONTEXT_MODE` is
  `all` or `scoped`; `context_for` only runs when you call it.
- **Read path only.** Automatic context never writes memory. Recording facts
  stays an explicit tool call (or your own code).
- **MCP hosts vary.** Some hosts defer or hide tools, or gate writes behind
  approval. A tool being available is not the same as the agent calling it.
- **Native hooks are host-specific.** Only Claude Code and Codex have the hook
  protocol checked; other CLIs with similar hooks would need their own check.
- **Trust prompts.** Both hosts may require you to approve a new hook, and a
  new session, before it runs.

## What is measured and what is not

Measured, and published in [benchmarks/](../benchmarks/): retrieval quality of
the server (LongMemEval, correction durability, and the other rows in the
README's *Measured* table).

Covered by tests but not benchmarked: that each adapter builds the right
request, applies the same dedup and validation rules, and fails open.

**Not measured:** whether injected context changes what any of these agents
actually does, in any runtime. No claim is made here about task success,
token savings or latency for the hook or the adapters; the 2,400-byte default
is a ceiling on what is added, and the roughly-600-token figure in
[AUTOMATIC_MEMORY.md](AUTOMATIC_MEMORY.md) is an estimate, not a tokenizer
count.
