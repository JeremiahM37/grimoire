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
banks/<bank>/observations.md       consolidated observations, with a "## History" of what they replaced
banks/<bank>/models/<id>.md        mental models; folders under models/ are the knowledge-page tree
banks/<bank>/proposals/<id>.md     an answer a refresh could not write over a person's edit
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

Observations and mental models follow the same rule — see below.

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
  "tag_groups": [{"tags": ["team:a"]}, {"not": {"tags": ["draft"]}}],   // boolean filter, AND-ed
  "temporal_window": {"start": "2024-03-01", "end": "2024-05-31"},       // instead of reading one from the query
  "min_scores": {"reranker": 0.2, "final": 0.1},
  "prefer_observations": false,  // drop facts an included observation was built from
  "include": { "entities": {}, "chunks": {"max_tokens": 8192},
               "source_facts": {"max_tokens": 4096, "max_tokens_per_observation": -1} },
  "trace": true }
```

`types` defaults to all three: `world`, `experience` and `observation`. A
`tag_groups` leaf is `{"tags": [...], "match": "any_strict"}` (the default
match; also `any`, `all`, `all_strict`, `exact`); inner nodes are `{"and":
[...]}`, `{"or": [...]}` and `{"not": {...}}`. `tags_match: "exact"` means the
fact's tags are exactly the set given, and with no tags selects the untagged.

Four arms run over the bank, each ranking on its own terms:

| arm | finds |
|---|---|
| semantic | facts closest in meaning to the query (cosine ≥ 0.3) |
| keyword | BM25 over fact text, entity names, context and dates, using the 16 rarest query words |
| graph | one hop from the 20 best semantic hits: facts sharing their entities, their nearest neighbours in meaning (≥ 0.7), and their causes |
| temporal | when the query names a time — dates, months, seasons, quarters, "last week", "three days ago", "early May", ranges — facts inside that window, spread across it, plus facts linked to them in time or by cause |

The lists are fused by reciprocal rank (k = 60) and the top 300 go to the
reranker the settings select (`GRIMOIRE_RERANK`, see CONFIG.md), each scored
as `[Date: May 02, 2023 (2023-05-02)] <context>: <text>`. A bank turns it off
with `enable_reranking: false`; a reranker that fails costs the query its
reordering, never its answer, and the trace says why (`rerank_error`) or
lists every candidate's reranker score (`rerank`). Each score is then nudged — by at most about ±10 % each —
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
| `GET /api/banks/{bank}/stats` | counts: facts (by type, human), observations, documents, entities, mental models, `pending_consolidation`, `operations_by_status`, `consolidation`, `model_available` |
| `GET /api/banks/{bank}/memories` | list facts (`q`, `type`, `document_id`, `authority=human`, `limit`, `offset`) |
| `GET/DELETE /api/banks/{bank}/memories/{id}` | one fact; deleting a person's fact needs `force=true` |
| `GET /api/banks/{bank}/entities[/{id or name}]` | entities with mention counts; one entity with its facts and companions |
| `GET /api/banks/{bank}/documents[/{id}]`, `DELETE …/documents/{id}` | sources |
| `GET /api/banks/{bank}/chunks/{chunk_id}` | one source chunk |

Bank ids are lowercase letters, digits and `._:-` (at most 64); `:` names a
family of banks (`coding-agent:grimoire`) and is stored as `__` on disk.

Per-bank settings (`config` in `bank.md`): `retain_extraction_mode`,
`retain_chunk_size`, `retain_extract_causal`, `enable_text_search`,
`enable_graph`, `enable_temporal`, `enable_reranking`, `consolidation`
(`auto` | `manual` | `off`), `observations_mission`, `consolidation_batch_size`
(default 8), `mcp_tools` (comma-separated allowlist), `reflect_max_tokens`.

## Language models

Extraction uses the configured LLM (`GRIMOIRE_LLM`), at temperature 0.1, in
JSON mode, with system prompt and content kept apart. For a provider that
thinks before answering, `GRIMOIRE_LLM_REASONING_EFFORT=off` turns thinking
off where the provider has a switch for it, and `GRIMOIRE_LLM_EXTRA_BODY`
passes any vendor-specific field. Recall never calls a model.

## Asynchronous retain and operations

`POST /api/banks/{bank}/memories` with `"async": true` queues the retain and
answers `202 {"success": true, "async": true, "operation_id": "op-…",
"operation_ids": [...], "items_count": N}` (up to 10,000 items, split into
operations of 500). MCP `retain` is always asynchronous.

Operations are background work: `retain`, `consolidation`,
`refresh_mental_model`. Workers run in the server (`GRIMOIRE_BANK_WORKERS`,
default 2), one operation per bank at a time, the bank served least recently
first. An operation interrupted by a restart is queued again on start.

| | |
|---|---|
| `GET /api/banks/{bank}/operations?status=&type=&limit=&offset=` | `{bank_id, operations: [Operation], total}`, newest first, payloads left out |
| `GET /api/banks/{bank}/operations/{id}` | one `Operation`, with `payload` and `result` |
| `DELETE /api/banks/{bank}/operations/{id}` | cancel: a queued one never starts, a running one is marked cancelled at once and stops at its next check; `409` when already finished |

`Operation` is `{id, bank_id, kind, type (= kind), status: queued | running |
completed | failed | cancelled, payload?, result?, error?, attempts,
cancel_requested?, created_at, started_at?, finished_at?, updated_at}`. A
retain's `result` is the synchronous retain response; a consolidation's is
the consolidation result below; a refresh's is `{mental_model_id, outcome,
version, based_on, usage}`. A failure that needs a model says
`model_required` in `error`.

## Reflect

`POST /api/banks/{bank}/reflect` (MCP `reflect`) answers a question by
reasoning over the bank. A model drives a short loop of retrieval calls and
finishes with an answer; the loop speaks plain JSON, so it works the same on
Ollama, OpenAI-compatible servers and Claude.

```jsonc
{ "query": "What does Dana think of the migration?",
  "budget": "low",                  // turns: low 5, mid 10, high 20
  "max_tokens": 4096,               // answer length target; longer answers are rewritten to fit
  "context": "…",                   // optional
  "tags": ["team:a"], "tags_match": "any", "tag_groups": [...],
  "fact_types": ["world", "experience", "observation"],
  "apply_all_directives": false,
  "exclude_mental_models": false, "exclude_mental_model_ids": ["…"],
  "response_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]},
  "query_timestamp": "…",
  "include": {"facts": {}, "tool_calls": {"output": true}} }   // tool_calls (or "trace": true) returns the trace
```

```jsonc
{ "text": "markdown answer",
  "mode": "llm",                    // or "extractive" with no model
  "based_on": { "memories": [RecallFact], "observations": [RecallFact],
                "mental_models": [{"id", "name", "text"}], "directives": [Directive] },
  "structured_output": {...}, "structured_output_error": "…",
  "usage": {"input_tokens", "output_tokens", "total_tokens"},
  "iterations": 3, "directives_checked": true,
  "trace": { "levels": ["search_mental_models", "search_observations", "recall"],
             "tool_calls": [{"tool", "input", "output", "duration_ms", "iteration", "forced"}],
             "llm_calls": [{"scope", "duration_ms"}], "rejected_citations": ["…"] } }
```

- **Levels in order.** Mental models are searched first, then observations,
  then facts (only the levels the bank has). The engine runs each level in
  turn whatever the model asks for (`forced` in the trace); only when every
  mental model found is fresh and has an answer (and the budget is not
  `high`) may it skip the rest. An answer before anything was retrieved is
  refused.
- **Tools the model has:** `search_mental_models`, `read_mental_model`,
  `search_observations`, `recall`, `expand` (a memory's source chunk or
  document) and `done`.
- **A person's word is authoritative.** Every memory reaches the model with
  its `authority`; the instructions say a human memory overrides a model's,
  and a `disputed_by` memory is only a disputed claim.
- **Mission, disposition and directives** go into the instructions; a
  disposition trait other than 3 adds one sentence each. When directives
  apply, the finished answer is checked against them and corrected.
  Directives with tags apply only when the reflect's tags reach them (or with
  `apply_all_directives`).
- **Citations are checked**: ids the model cites that no tool returned are
  dropped (`rejected_citations`). Citing nothing means resting on everything
  retrieved.
- **Without a model** reflect quotes what recall finds (`mode: extractive`).

A finished reflect fires the `reflect.completed` webhook.

## Observations and consolidation

Consolidation reads facts it has not read yet — eight to a model call, never
mixing tag sets — together with the observations they may bear on, and the
model decides to create, revise or retire observations. Each observation
cites its evidence facts (`src`), with short quotes (`ev`), and its proof
count is how many there are. It runs after every retain when the bank's
`consolidation` is `auto` and a model is configured; `manual` runs it only on
request; `off` never.

```
- Alice lives in Lyon and works nights <!--o id=o3f… sum=1a2b3c4d src=f1…,f2… proof=2 ev=f1…:moved%20to%20Lyon tags=family occ=… men=… upd=… -->

## History

- ~~Alice lives in Paris~~ <!--o of=o3f… at=2024-05-02T10:00:00Z src=f0… proof=1 -->
```

- A revised or retired model observation moves to `## History`, struck
  through, so what the bank used to believe stays readable.
- A batch the model fails on is halved and retried down to one fact; a fact
  that fails alone is recorded and not retried. A new observation whose
  meaning is ≥ 0.97 cosine to one in the same tag scope is merged into it.
- **A person's observation is never revised or retired by a model.** The
  model's version is filed beside it as a challenge (`chal=<id>`), which recall
  ranks below the person's and marks `disputed_by`. A model observation a
  person edits becomes theirs. All of it survives a reindex.
- Which facts were read is kept per fact text, so a fact a person edits is
  read again.

| | |
|---|---|
| `POST /api/banks/{bank}/consolidate` | queue a consolidation: `202 {operation_id, deduplicated}`; `409 {"code": "model_required"}` with no model |
| `GET /api/banks/{bank}/observations?q=&authority=human&tags=&tags_match=&include_history=1&limit=&offset=` | `{items: [Observation], total, history?}` |
| `GET /api/banks/{bank}/observations/{id}` | `{observation, history}` |
| `DELETE /api/banks/{bank}/observations/{id}` | retire one into history; a person's needs `force=true` |
| `DELETE /api/banks/{bank}/observations` | retire every model observation and start consolidation over; a person's stay |

`Observation` is `{id, text, authority, source_fact_ids, proof_count,
evidence: [{fact_id, quote}], tags, occurred_start?, occurred_end?,
mentioned_at?, updated_at?, challenges?}`. The consolidation result is
`{status: completed | no_new_facts | disabled, facts_processed, facts_failed,
batches, llm_failures, observations_created, observations_updated,
observations_deleted, observations_merged, challenges_recorded,
unresolved_actions, mental_model_refreshes_queued, usage, observations_total}`.

Observations are a recall type (`types: ["observation"]`), boosted by proof
count, and inherit their sources' entities.

## Mental models and knowledge pages

A mental model is a standing question whose answer the bank keeps written
down: `banks/<bank>/models/<id>.md`, with the question and settings in the
frontmatter and the answer as the body. An id may contain folders —
`people/dana` is the page `dana` in the folder `people` — and the folders are
the knowledge-page tree. URL-encode it in a path (`people%2Fdana`). On create,
a bare `id` with a `folder` names the page in that folder (`id: dana, folder:
people` is `people/dana`); with no `id` one is made from the name.

Answers are written by reflect (budget `mid` by default), on create (when no
body is given and a model is configured), after a consolidation for models
with `refresh: auto` whose memories changed, on request, or through an
operation. **A body a person wrote or edited is never overwritten**: the
frontmatter records a hash of the body the model last wrote (`body_sum`), and
when the body no longer matches, a refresh files its answer as a pending
proposal instead (`banks/<bank>/proposals/<id>.md`), which the person accepts
or rejects. Every version goes to note history.

A model is stale when the memories in its scope (its tags, `all_strict`, and
`fact_types`) are not those its answer was written from.

| | |
|---|---|
| `GET /api/banks/{bank}/mental-models?tags=&tags_match=&folder=&detail=full` | `{items: [MentalModel], total}`; bodies with `detail=full` |
| `POST /api/banks/{bank}/mental-models` | `{id?, name, question (or source_query), folder?, tags?, refresh: auto, max_tokens: 2048, budget: mid, fact_types?, body (or content)?}` → `201 {mental_model, mental_model_id, operation_id}` |
| `GET /api/banks/{bank}/mental-models/{id}` | one `MentalModel` with its body |
| `PATCH /api/banks/{bank}/mental-models/{id}` | any create field; `folder` moves it (the id changes); `body` is a person's edit |
| `DELETE /api/banks/{bank}/mental-models/{id}` | delete it and its proposal |
| `POST /api/banks/{bank}/mental-models/{id}/refresh` | `202 {operation_id, status: "queued", deduplicated}`; `409 model_required` with no model |
| `POST …/mental-models/{id}/proposal/accept` · `…/proposal/reject` | make the proposal the answer (handing the body back to the model) · discard it |
| `GET …/mental-models/{id}/history` · `…/history/{version}` | `{mental_model_id, path, version, versions: [{id, ts, size}]}` · `{content}` |
| `GET /api/banks/{bank}/mental-models-tree` | `{roots: [{kind: folder | page, name, path, model?, children?}]}` |
| `GET /api/banks/{bank}/mental-models-export[?format=markdown]` | `{files: [{path, content}]}` — `index.md` plus one file per page — or one markdown document |

`MentalModel` is `{id, bank_id, name, question, folder, path, tags, refresh,
max_tokens, budget, fact_types?, body?, version, last_refreshed?, based_on,
authority: human | agent, is_stale, stale_reason?: never_refreshed |
memories_changed, pending_proposal?: {content, based_on, created_at,
base_version}}`.

## Directives

Standing rules for every reflect answer, kept as bullets in bank.md's
`## Directives`:

```
- Answer in French. <!--d id=d1a2b3c4d5 name=Lang prio=2 tags=fr -->
```

| | |
|---|---|
| `GET /api/banks/{bank}/directives?tags=&active_only=false` | `{items: [Directive], total}` |
| `POST /api/banks/{bank}/directives` | `{text (or content), name?, tags?, priority?, is_active?}` → `201 Directive`; names are unique |
| `PATCH /api/banks/{bank}/directives/{id}` | any of the create fields |
| `DELETE /api/banks/{bank}/directives/{id}` | |

`Directive` is `{id, name?, text, tags?, priority?, inactive?}`. Only these
calls and a person editing bank.md change directives.

## Webhooks

| | |
|---|---|
| `GET/POST /api/banks/{bank}/webhooks` | `{items: [Webhook], total}` / `{url, secret?, events (or event_types)?, enabled?}` → `201 Webhook`, with the secret shown this once |
| `PATCH/DELETE /api/banks/{bank}/webhooks/{id}` | patch any create field / delete it and its log |
| `GET /api/banks/{bank}/webhooks/{id}/deliveries?limit=` | `{items: [Delivery]}`, newest first |
| `/api/webhooks…` | the same for webhooks on every bank (bank `""`); administrators only |

Events: `retain.completed`, `consolidation.completed` (the default pair),
`reflect.completed`, or `*`. A webhook receives `POST` with
`{event, bank_id, operation_id?, status: completed | failed, timestamp, data}`
— retain `data` is `{document_ids, memory_unit_count, items_count}`,
consolidation's `{observations_created, observations_updated,
observations_deleted, challenges_recorded, facts_processed}`, and either
carries `error_message` on failure. Headers: `X-Grimoire-Event`,
`X-Grimoire-Delivery`, `X-Grimoire-Timestamp`, `X-Grimoire-Signature:
t=<unix>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>` (re-signed per attempt;
check it and refuse old timestamps) and `X-Grimoire-Signature-256:
sha256=<hex HMAC-SHA256(secret, body)>`.

A non-2xx answer or a network error is retried 5 s, 5 min, 30 min, 2 h and
5 h later, then the delivery is `failed`. Deliveries are stored, so retries
survive a restart. URLs must be `http(s)`; loopback, private, CGNAT and
numeric-spelled addresses are refused at registration and again at connect
time unless `GRIMOIRE_WEBHOOK_ALLOW_PRIVATE` is on, and link-local/metadata
addresses always.

`Webhook` is `{id, bank_id, url, events, has_secret, enabled, created_at,
updated_at}`; `Delivery` is `{id, webhook_id, bank_id, event, status:
pending | delivered | failed, attempts, next_attempt_at?, last_error?,
last_response_status?, created_at, updated_at, payload}`.

## Templates

A template is a bank's configuration as data:

```jsonc
{ "version": "1",
  "bank": {"name", "mission", "retain_mission", "disposition": {...}, "tags": [...], "config": {...}},
  "mental_models": [{"id", "name", "question", "tags", "refresh", "max_tokens", "budget", "fact_types"}],
  "directives": [{"name", "text", "tags", "priority", "inactive"}] }
```

Built-ins: `assistant`, `coding-agent`, `support`, `research`,
`plain-retrieval` (no model in the loop).

| | |
|---|---|
| `GET /api/bank-templates` | `{templates: [{id, name, description, manifest}]}` |
| `GET /api/bank-templates/{id}` | one template |
| `GET /api/banks/{bank}/export` | the bank's manifest: profile fields as set, settings, models' questions, directives |
| `POST /api/banks/{bank}/import[?dry_run=1]` | body: a manifest, `{"manifest": …}` or `{"template": "<id>"}` → `{bank_id, bank_created, config_applied, mental_models_created, mental_models_updated, directives_created, directives_updated, operation_ids, dry_run}` |

Import is additive: it creates the bank if needed, sets what the manifest
names, and removes nothing. Models are matched by id (their question and
settings updated, their answer never touched), directives by name (or text).

## MCP

`grimoire-mcp` serves the bank tools over stdio (bank from the `bank`
argument or `GRIMOIRE_BANK`) and over HTTP:

- `/mcp/{bank}` — one bank. Tools take no `bank` argument and `list_banks` /
  `create_bank` are not offered. The way to hand an agent exactly one bank.
- `/mcp` with an `X-Bank-Id` header — that bank is the default; a `bank`
  argument still overrides it.

A bank's `mcp_tools` setting (e.g. `bank_recall,reflect`) narrows what an
agent may call on it: on `/mcp/{bank}` it is the whole tool list, on `/mcp`
it covers the bank tools. Tools: `retain` (asynchronous; returns an
`operation_id`), `bank_recall`, `reflect`, `list_banks`, `create_bank`,
`bank_profile`, `list_bank_memories`, `get_bank_memory`, `delete_bank_memory`,
`list_entities`, `list_bank_documents`, `get_bank_document`,
`delete_bank_document`, `consolidate`, `list_observations`,
`list_mental_models`, `get_mental_model`, `create_mental_model`,
`update_mental_model`, `delete_mental_model`, `refresh_mental_model`,
`list_directives`, `create_directive`, `delete_directive`, `list_operations`,
`get_operation`, `cancel_operation`, `list_bank_templates`,
`import_bank_template`.

## Coding agents

One bank per repository, MCP setup for Claude Code and Codex, seeding from git
history and an optional session-transcript hook: see [CODING_AGENTS.md](CODING_AGENTS.md).

## From the shell, the web app and code

- **CLI**: `grimoire bank help`. The commands are an HTTP client of a running
  server (`GRIMOIRE_URL`, `GRIMOIRE_AUTH_TOKEN`, or `--url`/`--token`):
  `list`, `create --template`, `show`, `stats`, `update`, `delete --yes`,
  `export`, `import`, `retain` (text, `--file`, `--dir` or stdin; `--async
  --wait`), `recall --trace`, `reflect --trace`, `memories ls|rm`, `entities`,
  `documents`, `import-git`, `observations [show|rm|consolidate]`, `models
  ls|tree|show|create|edit|move|refresh|accept|reject|history|export|rm`,
  `directives ls|add|set|rm`, `ops ls|show|wait|cancel` and `templates [show]`.
  `create --template` creates the bank and then imports the template, so its
  mental models and directives come with it. Against an older server that
  lacks a route, a command prints "not available on this server"; a
  `model_required` answer is reported as needing a language model.
- **Web app**: *Memory banks* in the sidebar or the command palette. Per bank:
  a recall and reflect playground (each arm's ranks and the trace; the
  reflect answer with the memories, observations and models it cites, and
  optionally its tool calls), memories, documents and chunks, entities,
  observations (evidence, history, consolidate), mental models (the folder
  tree, refresh that follows its operation, editing the answer, moving it,
  and accepting or rejecting a pending proposal), directives, background
  operations, and the profile. New banks can start from a template, whose
  models and directives are listed before it is applied.
- **Python / JavaScript**: `Grimoire(...).bank("name")` in `clients/python`, and
  `@jeremiahm37/grimoire/banks` in `clients/js`, each with a wrapper that adds
  recalled memories to an OpenAI-compatible chat call and retains the exchange.
