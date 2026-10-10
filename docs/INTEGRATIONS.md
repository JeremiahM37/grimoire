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
| **LangChain** | `GrimoireChatMessageHistory`, `GrimoireRetriever`, `make_remember_tool` / `make_recall_tool` (`grimoire_client.langchain`) | Retriever and history run when your chain calls them | Tools the model calls; history is what the chain writes | Fakes and a stub server; not run against a live `langchain_core` in this repo |
| **LlamaIndex** | `GrimoireRetriever` (nodes) and `GrimoireMemoryBlock` with `get`/`put` (`grimoire_client.llamaindex`) | Block `get` recalls for the latest user turn | Your agent calls `retrieve`; `put` records turns | Duck-typed fakes and a stub server; not run against `llama_index.core` here |
| **Pydantic AI** | `grimoire_tools` and `grimoire_context` (`grimoire_client.pydantic_ai`) | `grimoire_context` in a dynamic system prompt | `remember`, `recall`, `search_notes` tools | Signatures checked with `get_type_hints`; stub server; not run against a live model |
| **Vercel AI SDK (JS)** | `aiSdkTools` and `withGrimoireMemory` (`@jeremiahm37/grimoire/ai-sdk`) | Wrapper recalls before and retains after each call | `remember`, `recall`, `search_notes` tool definitions | Stub HTTP server; no `ai` package dependency, so not run against it |
| **AutoGen** | `GrimoireMemory` (`grimoire_client.autogen`) | `update_context` recalls for the latest user message and adds the facts as a system message | `add` / `query` as your agents call them | Duck-typed fakes against AutoGen's `Memory` shape and a stub server; not run against `autogen_core` here |
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

## Framework adapters

Every adapter writes with the agent's name and `category: agent-authored`, so
a fact can be traced to the agent that recorded it. Recall passes through the
client-side fence (`grimoire_client.fencing`, JS `fence` in `ai-sdk.js`), a
mirror of `go/internal/trust/fence.go`: text the server marks `untrusted` is
wrapped in the same markers, and the preamble is added whenever a fence is
present. Python adapters import without their framework; LangChain and
LlamaIndex use the real types when `langchain-core` and `llama-index-core` are
installed (`pip install 'grimoire-client[langchain]'`, `[llamaindex]`).

| Adapter | Module | What it gives you |
|---|---|---|
| LangChain chat history | `grimoire_client.langchain.GrimoireChatMessageHistory` | Messages as session facts, in write order; `clear` hard-deletes |
| LangChain retriever | `grimoire_client.langchain.GrimoireRetriever` | `invoke` / `ainvoke` returning documents with trust, authority and freshness in metadata |
| LangChain tools | `make_remember_tool`, `make_recall_tool` | Named callables; `StructuredTool` when `langchain_core` is present |
| LlamaIndex retriever | `grimoire_client.llamaindex.GrimoireRetriever` | `retrieve` / `aretrieve` returning nodes with scores |
| LlamaIndex memory block | `grimoire_client.llamaindex.GrimoireMemoryBlock` | `get` recalls, `put` records each turn once; async variants |
| Pydantic AI tools | `grimoire_client.pydantic_ai.grimoire_tools` | Typed, documented functions for `Agent(tools=[...])` |
| Pydantic AI context | `grimoire_client.pydantic_ai.grimoire_context` | Bounded context string for a dynamic system prompt |
| AutoGen memory | `grimoire_client.autogen.GrimoireMemory` | `add` remembers, `query` recalls, `update_context` injects recalled facts as a system message; async |
| Vercel AI SDK tools | `@jeremiahm37/grimoire/ai-sdk` `aiSdkTools` | `{description, parameters, execute}` for `remember`, `recall`, `search_notes` |
| Vercel AI SDK memory | `withGrimoireMemory(generate, {client})` | Recall before and retain after each `generateText`-style call |

**LangChain**

```python
from grimoire_client.langchain import GrimoireChatMessageHistory, GrimoireRetriever, make_recall_tool
history = GrimoireChatMessageHistory(client, session_id="chat-7", agent="support-bot")
docs = GrimoireRetriever(client, agent="support-bot").invoke("indentation")  # untrusted text fenced
tools = [make_recall_tool(client)]              # plain callables with .name and .description
```

**LlamaIndex**

```python
from grimoire_client.llamaindex import GrimoireMemoryBlock, GrimoireRetriever
nodes = GrimoireRetriever(client, agent="research-bot").retrieve("kestrel deploy")
memory = GrimoireMemoryBlock(client, agent="research-bot", session="run-12")
context = memory.get(messages)                  # recalled facts for the latest user turn
memory.put(messages)                            # records each turn once
```

**Pydantic AI**

```python
from grimoire_client.pydantic_ai import grimoire_context, grimoire_tools
agent = Agent("anthropic:claude-sonnet-4-5", tools=grimoire_tools(client, agent_name="support-bot"))
@agent.system_prompt
def memory(ctx) -> str:
    return grimoire_context(client, ctx.prompt or "")
```

**AutoGen**

```python
from grimoire_client.autogen import GrimoireMemory
memory = GrimoireMemory(client, agent="support-bot")
assistant = AssistantAgent("support", model_client=model, memory=[memory])  # AutoGen's Memory protocol
await memory.add(MemoryContent(content="prefers tabs", metadata={"topic": "prefs"}))
```

`clear` and `close` do not touch the server: Grimoire memory is durable, and forgetting a fact is a deliberate `forget`.
`update_context` drops nothing when the server is down; `add` and `query` raise, because the caller asked for them.

**Vercel AI SDK (JS)**

```js
import { aiSdkTools, withGrimoireMemory } from '@jeremiahm37/grimoire/ai-sdk'
const tools = aiSdkTools(client, { agent: 'support-bot' })   // remember, recall, search_notes
const generate = withGrimoireMemory((p) => generateText({ model, ...p }), { client, session: 'chat-7' })
const { text } = await generate({ prompt: 'what indentation do I like?' })
```

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
