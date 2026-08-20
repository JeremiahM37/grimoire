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

## Beyond memory

`ask`, `search_notes`, `read_note`, `write_note`, `get_fact`, `briefing`,
`health` — the same client reaches the knowledge base. Errors are typed:
`NotFound`, `Unauthorized`, `VaultLocked`, and `GrimoireError` for the rest.

## Development

```bash
PYTHONPATH=. pytest tests -q
```
