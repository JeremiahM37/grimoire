# grimoire-client

Python client for [Grimoire](https://github.com/JeremiahM37/grimoire) — self-hosted
memory, knowledge and credentials for AI agents.

No dependencies. The server is one static binary with no runtime; a client that
drags in a stack to reach it undoes the property people install it for.

```bash
pip install grimoire-client
```

```python
from grimoire_client import Grimoire

g = Grimoire("http://localhost:9111", token="...", agent="my-agent")

g.add("the user prefers tabs", topic="prefs", session="run-1")
g.add("the user prefers spaces")        # → Result(op='UPDATE', target=...)

[m.text for m in g.search("indentation")]
# ['the user prefers spaces']           # only what is currently believed
```

## Writes reconcile

This is the one behavioural difference from a plain memory store, and the reason
recall stays sharp as memory grows:

| the new fact | what happens | `result.op` |
|---|---|---|
| says something new | stored | `ADD` |
| contradicts one on file | the old one is superseded | `UPDATE` |
| retracts one on file | the old one is struck through, nothing stored | `DELETE` |
| is already recorded | nothing is written | `NOOP` |

`result.why` says which fact was involved and why. Pass `infer=False` to store
verbatim with no extraction and no reconciliation.

A superseded fact is **struck through, not deleted** — it stays in the markdown
note, so you can see what an agent used to believe:

```python
g.search("indentation", include_superseded=True)   # both, with superseded_by set
g.history("2026-08-14T09:00:00Z")                  # what was believed then
```

## Scoping and lifetime

```python
g.add("the staging box is smaller", session="run-42", category="infra")
g.add("priya is on call", expires_in="72h")    # stops being recalled by itself
g.add("never touch prod", immutable=True)      # reconciliation can never remove it

g.search(session="run-42")                     # what this run learned
g.scopes()                                     # agents / sessions / categories in use
```

## Coming from mem0

`add`, `search`, `get_all` and `delete` line up, so the switch is an import
change. The differences: scope is `session=` / `agent=` rather than `run_id=` /
`agent_id=`, `delete` takes the note path alongside the id (facts live in files
you own), and `add` reconciles rather than accumulating.

## LangGraph

`GrimoireStore` is a real `BaseStore`, so LangGraph's cross-thread memory lands
in markdown files you can open:

```bash
pip install 'grimoire-client[langgraph]'
```

```python
from grimoire_client import Grimoire
from grimoire_client.langgraph import GrimoireStore

store = GrimoireStore(Grimoire("http://localhost:9111"))
store.put(("memories", "alice"), "pref", {"text": "prefers tabs"})
store.search(("memories", "alice"), query="indentation")
```

A namespace becomes a note (`memory/memories-alice.md`), a key becomes the
fact's provenance field, and a plain `{"text": ...}` value is written as
readable text rather than JSON. Writes are verbatim by default — a key-value
store has to return what was put — and `GrimoireStore(client, reconcile=True)`
opts into reconciled writes instead. Either way reconciliation is confined to
the namespace, so one namespace can never supersede another's facts.

## CrewAI

CrewAI's storage backend is *vector-in*: it hands the storage a query embedding,
never the query text. That only produces meaningful results if CrewAI's vectors
are in the same space as the stored ones — so point its embedder at the server:

```bash
pip install 'grimoire-client[crewai]'
```

```python
from crewai.memory.unified_memory import Memory
from grimoire_client import Grimoire
from grimoire_client.crewai import GrimoireEmbedder, GrimoireStorage

client = Grimoire("http://localhost:9111")
memory = Memory(storage=GrimoireStorage(client), embedder=GrimoireEmbedder(client))
```

Scopes become notes (`memory/crew-researcher.md`), record ids become the fact's
provenance field, and a record with no metadata is stored as readable text. If
you point CrewAI at a *different* embedder, the server refuses the vector on
width rather than scoring it — a cosine between two models' vectors is a number
with no meaning, and silently returning one is worse than an error.

## Memory banks

A bank takes raw content — a transcript, a document — extracts the facts
itself, and recalls them by meaning, words, entities and time. Facts a person
corrected outrank what a model extracted (`authority: "human"`; a model fact
that contradicts one carries `disputed_by`). See
[docs/MEMORY_BANKS.md](../../docs/MEMORY_BANKS.md).

```python
bank = g.bank("support")
bank.retain([{"speaker": "Dana", "text": "Move the migration to May."}],
            document_id="chat-42", timestamp="2024-04-02T10:00:00Z")
hits = bank.recall("when is the migration?", budget="mid", max_tokens=2048)
[f["text"] for f in hits["results"]]

g.banks.list(); g.banks.create("research", mission="papers I read")
bank.list_memories(authority="human"); bank.entities(); bank.documents()
await g.async_bank("support").recall("...")    # same calls, awaitable
```

Reasoning over a bank:

```python
answer = bank.reflect("what does Dana think of the migration?")
answer["text"], answer["mode"], answer["based_on"]["memories"]   # mode: llm | extractive

op = bank.retain(transcript, document_id="s-1", async_=True)      # 202: queued
bank.wait_operation(op["operation_id"])                           # completed | failed | cancelled

bank.consolidate()                                  # facts -> observations
bank.observations(include_history=True)["items"]
m = bank.create_mental_model("Dana", "Who is Dana?", folder="people")
bank.refresh_mental_model(m["mental_model_id"])     # "people/dana"
bank.update_mental_model("people/dana", body="My own words.")   # a person's edit wins
bank.mental_model("people/dana").get("pending_proposal")        # a later refresh waits here
bank.accept_proposal("people/dana")                 # or reject_proposal
bank.mental_model_tree(); bank.export_mental_models(markdown=True)
bank.create_directive("Answer in one sentence.", name="Brief")
bank.import_template(template="coding-agent"); g.banks.templates()
```

Consolidation and mental-model refreshes need a language model on the server;
without one they raise `ModelRequired` (409, `code: model_required`), while
reflect answers extractively. Moving a mental model to another folder changes
its id. On a server from before these routes they raise `NotAvailable` (a
`NotFound` subclass), so a caller can hide the feature; `retain(...,
async_=True)` there falls back to a synchronous retain and says so with
`"async_fallback": True`.

### Memory for any OpenAI-compatible chat client

```python
from openai import OpenAI          # or AsyncOpenAI, or any client with the same shape
from grimoire_client import Grimoire, with_memory

llm = with_memory(OpenAI(), Grimoire().bank("user-42"),
                  session_id="chat-7", max_tokens=1024, background=True)
llm.chat.completions.create(model="...", messages=[{"role": "user", "content": "..."}])
```

Before each `chat.completions.create` it recalls with the last user message and
adds a `# Relevant memories` block to the system prompt; afterwards it retains
`USER: … / ASSISTANT: …` into the session's document (appending). Options:
`inject`, `store`, `budget`, `max_tokens`, `types`, `recall_tags`,
`max_memories`, `tags`, `session_id`, `query`, `background`. Override any of
them for one call with a `grimoire_` keyword (`grimoire_inject=False`); those
are removed before the request reaches the model. A failed recall never breaks
the call; background retain errors are kept in `llm.pending_errors()`, and
`llm.flush()` waits for them. Streams are retained once exhausted.

## Knowing when the notes don't say

`ask` returns a `supported` verdict alongside the answer:

```
g.ask("what port does the deploy use?")
# {"answer": "...", "supported": "ungrounded", "citations": [...]}
```

`grounded` means the notes state what you asked for, `ungrounded` means they
are about the right topic but do not contain the answer, `unknown` means no
reader judged it. Acting on `ungrounded` is the point — retrieved passages
being on-topic is not evidence that they answer the question, and the
similarity scores cannot tell you the difference (measured: a top-cosine
threshold scores AUC 0.55 on that task, and inverts on multi-hop questions). The verdict
costs no extra model call; it rides in the same completion as the answer.

## Beyond memory

`ask`, `search_notes`, `read_note`, `write_note`, `get_fact`, `briefing`,
`health` — the same client reaches the knowledge base. Errors are typed:
`NotFound`, `Unauthorized`, `VaultLocked`, and `GrimoireError` for the rest.

## Development

```bash
PYTHONPATH=. pytest tests -q
```
