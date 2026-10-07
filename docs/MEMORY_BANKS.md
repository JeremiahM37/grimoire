# Memory banks

A memory bank is a named, isolated memory that an agent fills by handing over
raw content — a conversation transcript, a document, a day's notes — and
queries with a hybrid recall. It sits beside the per-fact `remember`/`recall`
memory rather than replacing it, and differs in who does the distilling:
`remember` stores what an agent already decided to keep; a bank takes the
transcript itself and extracts the facts worth keeping.

```
retain(content) ─► chunk ─► extract facts ─► resolve entities ─► write markdown ─► index
recall(query)   ─► meaning + words + entity graph + time ─► fuse ─► boost ─► pack to a token budget
```

## Files are the truth

A bank is a folder of ordinary notes:

```
banks/<bank>/bank.md               profile: name, mission, retain mission, disposition, directives, settings
banks/<bank>/documents/<doc>.md    each source exactly as retained, with its chunk hashes
banks/<bank>/facts/<doc>.md        one bullet per extracted fact
```

A fact is a bullet with a machine trailer:

```
- Alice moved to Lyon | Involving: Alice | she could not pay rent <!--f id=f3a… sum=9c0d1e2f type=world kind=event chunk=0 occ=2023-05-20..2023-05-20 men=2023-05-24T10:00:00Z ent=Alice;Lyon cause=f81… -->
```

| field | meaning |
|---|---|
| `id` | stable id, derived from the document, chunk, position and text |
| `sum` | hash of the text as written by Grimoire — see *Authorship* |
| `by=human` | a person wrote or corrected this fact |
| `type` | `world` (about the world, including the user's own preferences and plans) or `experience` (something the assistant itself did) |
| `kind` | `event` (happened at a time) or `conversation` (an ongoing state or trait) |
| `chunk` | which chunk of the document it came from |
| `occ` | when it happened, start..end; a vague date covers its whole period (`2023-05-01..2023-05-31`) |
| `men` | when the source said it |
| `ent` | the entities it is about, by canonical name |
| `cause` | ids of facts this one was caused by |
| `tags` | tags, for filtering recall |
| `chal` | the human fact this one contradicts (see below) |
| `doc_removed` | a person's fact whose source text no longer exists |

Everything in SQLite (`bank_*` tables) is a cache built from these files by the
ordinary index pass. A reindex rebuilds a bank without calling a model;
embeddings are cached by content, so an unchanged fact is not embedded again.
A bank folder is covered by spaces and reader lists like any other folder: a
caller sees a bank only if they can read its `bank.md`, and writes to it only
if they can write there.

## Authorship: what a person writes wins

A fact is a person's when its trailer says `by=human`, when the bullet has no
trailer at all (typed by hand), or when its text no longer matches its `sum`
(edited in an editor). Nothing has to be declared: correct a fact in your
editor, save, and from then on it is yours. When Grimoire next rewrites the
file it writes `by=human` so the claim no longer depends on the mismatch.

- **Re-retain and document replacement never delete or rewrite a person's
  fact.** If the chunk it came from is gone, it is kept and marked
  `doc_removed`.
- **Deleting a document** removes the model's facts from it and keeps the
  person's (marked `doc_removed`) unless `force=true`. Deleting a person's fact
  directly also needs `force=true`.
- **When a model re-extracts a fact a person corrected** — same chunk, the
  original text again or the same thing in other words — the new fact carries
  `chal=<the person's fact id>`.
- **In recall, a person's fact ranks above any model fact that contradicts
  it** (a recorded challenge, a different value for the same thing, or a
  re-derivation from the same source). The model fact is still returned —
  the disagreement is information — marked `disputed_by`, directly below.
- Profiles, directives and missions change only through explicit calls.
- All of this is read from the files, so a reindex preserves it.

Consolidated observations and mental models (a later phase) follow the same
rule: a model may challenge a person's text, never supersede it.

## Retain

`POST /api/banks/{bank}/memories` (MCP `retain`). The bank is created on first
use.

```jsonc
{ "items": [{
    "content": "…" ,                      // text, or a JSON array of {speaker, text, timestamp} turns
    "timestamp": "2023-05-08T13:56:00Z",  // when it was said; null = now, "unset" = no time
    "document_id": "session-42",          // optional; same id again updates that document
    "context": "support chat with Dana",
    "metadata": {"channel": "sms"},
    "entities": [{"text": "Dana"}],       // hints added to every fact; resolve_entities=false keeps them literal
    "tags": ["support"],
    "update_mode": "replace"              // or "append"
  }],
  "document_tags": ["…"] }
```

1. **Chunking.** Up to 3000 characters a chunk (`retain_chunk_size`).
   Conversations split between turns, JSON Lines between lines, prose on
   paragraphs, then lines, sentences, clauses and words. Chunking is
   deterministic and idempotent, and each chunk is identified by its SHA-256.
2. **Delta.** Retaining a document id again re-extracts only chunks whose
   hash changed; facts from unchanged chunks are kept as they are. Identical
   content changes nothing. `append` extends the stored text.
3. **Extraction.** One model call per chunk asks for the facts worth keeping
   months later, each with what / when / who / why, an event-or-state kind,
   dates resolved against the item's timestamp, entities and causes. The
   bank's retain mission is included. Replies are parsed leniently, retried
   when malformed, and a chunk whose reply hits the output budget is split in
   two and retried. Modes (`retain_extraction_mode`): `concise` (default),
   `verbatim` (one fact per chunk, the chunk itself, annotated) and `chunks`
   (one fact per chunk, no model). **With no model configured**, each sentence
   becomes a fact, capitalised names become entities, and a date written in
   the sentence makes it an event.
4. **Entities.** A name is reused when it is the same words, or when it is
   similar *and* corroborated — it appears with the same companions, or was
   seen within the week. Names carrying different numbers never merge. New
   spellings of one name within a retain are merged together.
5. **Write and index.** The document and facts files are written (previous
   versions go to history), then indexed.

The response reports per document how many chunks were extracted and reused
and how many of a person's facts were kept, and the model tokens spent
(booked in usage as `bank.retain`).

## Recall

`POST /api/banks/{bank}/memories/recall` (MCP `bank_recall`).

```jsonc
{ "query": "what did Dana say about the migration last spring?",
  "budget": "mid",              // low | mid | high: candidates per arm 100 | 300 | 1000
  "max_tokens": 4096,           // budget for fact text; 0 returns no facts
  "types": ["world", "experience"],
  "tags": ["support"], "tags_match": "any",   // any | all | any_strict | all_strict
  "query_timestamp": "2024-06-01T00:00:00Z",  // "now" for relative dates and recency
  "include": { "entities": {}, "chunks": {"max_tokens": 8192}, "source_facts": {} },
  "trace": true }
```

Four arms run over the bank, each ranking on its own terms:

| arm | finds |
|---|---|
| semantic | facts closest in meaning to the query (cosine ≥ 0.3) |
| keyword | BM25 over fact text, entity names, context and dates, using the 16 rarest query words |
| graph | one hop from the 20 best semantic hits: facts sharing their entities, their nearest neighbours in meaning (≥ 0.7), and their causes |
| temporal | when the query names a time — dates, months, seasons, quarters, "last week", "three days ago", "early May", ranges — facts inside that window, spread across it, plus facts linked to them in time or by cause |

The lists are fused by reciprocal rank (k = 60) and the top 300 go to an
optional reranker. Each score is then nudged — by at most about ±10 % each —
for recency, closeness to the window the query named, and supporting
evidence. A person's facts are placed above model facts that contradict them.
The top facts are packed into `max_tokens` in rank order, skipping one that
does not fit rather than stopping; the source chunks of the top facts can be
returned within their own budget.

Each result carries `authority` (`human` or `agent`), its dates, entities,
document and chunk id, tags, metadata, and its scores. With `trace`, the
response shows every arm's ranked hits, the window the query was read as,
the tokens used and per-stage timings.

At 20,000 facts recall takes about 5 ms at the median on a 32-core host
(10 ms at the 95th percentile, up to ~40 ms for the first query after a
write). Vectors are held per bank in one contiguous array and scanned in
parallel; the cache is rebuilt after any write to that bank.

## Other endpoints

| | |
|---|---|
| `GET/POST /api/banks` | list readable banks / create one (`bank_id`, `name`, `mission`, `retain_mission`, `disposition`, `directives`, `config`) |
| `GET/PATCH/DELETE /api/banks/{bank}` | profile; a patch merges `config` and an empty value removes a setting |
| `GET /api/banks/{bank}/memories` | list facts (`q`, `type`, `document_id`, `authority=human`, `limit`, `offset`) |
| `GET/DELETE /api/banks/{bank}/memories/{id}` | one fact; deleting a person's fact needs `force=true` |
| `GET /api/banks/{bank}/entities[/{id or name}]` | entities with mention counts; one entity with its facts and companions |
| `GET /api/banks/{bank}/documents[/{id}]`, `DELETE …/documents/{id}` | sources |
| `GET /api/banks/{bank}/chunks/{chunk_id}` | one source chunk |

Bank ids are lowercase letters, digits and `._:-` (at most 64); `:` names a
family of banks (`coding-agent:grimoire`) and is stored as `__` on disk.

Per-bank settings (`config` in `bank.md`): `retain_extraction_mode`,
`retain_chunk_size`, `retain_extract_causal`, `enable_text_search`,
`enable_graph`, `enable_temporal`, `enable_reranking`, `consolidation`.

## Models

Extraction uses the configured LLM (`GRIMOIRE_LLM`), at temperature 0.1, in
JSON mode, with system prompt and content kept apart. For a provider that
thinks before answering, `GRIMOIRE_LLM_REASONING_EFFORT=off` turns thinking
off where the provider has a switch for it, and `GRIMOIRE_LLM_EXTRA_BODY`
passes any vendor-specific field. Recall never calls a model.

## Coding agents

One bank per repository, MCP setup for Claude Code and Codex, seeding from git
history and an optional session-transcript hook: see [CODING_AGENTS.md](CODING_AGENTS.md).

## From the shell, the web app and code

- **CLI**: `grimoire bank help`. The commands are an HTTP client of a running
  server (`GRIMOIRE_URL`, `GRIMOIRE_AUTH_TOKEN`, or `--url`/`--token`):
  `list`, `create --template`, `show`, `update`, `delete --yes`,
  `retain` (text, `--file`, `--dir` or stdin), `recall --trace`, `memories ls|rm`,
  `entities`, `documents`, `import-git`, and `reflect`, `observations`, `models`,
  `ops`, `templates` for the surfaces still being built — those print
  "not available on this server" until the server has them.
- **Web app**: *Memory banks* in the sidebar or the command palette — profile
  editor, memories, documents and chunks, entities, and a recall playground
  that shows each arm's ranks and the trace.
- **Python / JavaScript**: `Grimoire(...).bank("name")` in `clients/python`, and
  `@jeremiahm37/grimoire/banks` in `clients/js`, each with a wrapper that adds
  recalled memories to an OpenAI-compatible chat call and retains the exchange.
