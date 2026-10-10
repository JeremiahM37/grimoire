# Disputes

A dispute is a fact a person recorded that an agent contradicted and was not
allowed to replace. Reconciliation (`go/internal/memory/reconcile.go`) refuses
the overwrite, keeps the agent's claim as a new entry, and points it at the
contested one through `Entry.Challenges`. Until now that showed as a `disputed`
badge that nobody could act on. This document describes how a person settles
one.

## The rule

A human edit always wins, and a resolution always writes a human-authority
entry. So settling a dispute is a person's act. An agent caller may read the
disputes and may not resolve them, against any entry, because a resolution is
by definition a human-authority write and the lattice (`authority.go`) does not
entitle an agent to make one. This is stricter than the lattice requires, and
it is deliberate.

Agents are identified the way a forget is: a request that carries an agent
identity (`X-Grimoire-Agent`, or a verified agent name) is an agent; a request
with none, or the CLI, is the person. See `isAgentInitiated` in
`go/internal/api/memory_cascade.go`.

## Listing

`GET /api/memory/disputes`

```json
[{
  "id": "3f9c2a1b0d4e",
  "path": "memory/ops.md",
  "disputed":    {"id": "...", "path": "memory/ops.md", "text": "Billing Postgres runs on port 6432",
                  "agent": "me", "stamp": "2026-10-01 09:12", "authority": "human", "evidence": []},
  "challengers": [{"id": "...", "path": "memory/ops.md", "text": "Billing Postgres runs on port 5432",
                   "agent": "claude", "stamp": "2026-10-09 17:40", "authority": "agent", "evidence": ["ops/runbook.md"]}]
}]
```

- A dispute is listed only while both sides are believed (neither retracted,
  superseded nor expired), and only to a caller who may see both.
- `challengers` lists every live entry that contests the disputed one.
- Each side carries its text, authority (`human`, `agent` or `pulled`), stamp
  and evidence. Evidence is always an array, possibly empty.

## Resolving

`POST /api/memory/disputes/resolve`

| field | meaning |
|---|---|
| `id` | the disputed entry's id (from the list) |
| `path` | the note it lives in; needed only if the id is ambiguous (409 otherwise) |
| `resolution` | `keep`, `accept_challenger` or `merge` |
| `challenger` | the contesting entry to accept; needed when more than one contests it |
| `text` | `merge` only: the new fact, 1 to 20000 characters |

| resolution | what is written |
|---|---|
| `keep` | the original stands and is marked human-confirmed. Each challenger is retracted (`retracted:human`). |
| `accept_challenger` | the original is superseded by the chosen challenger, which becomes human-authored. Other challengers are retracted. |
| `merge` | a new human-authored entry is appended to the original's note. The original and every challenger are superseded by it. |

Every resolution clears the challenge on the entries it touches, so nothing is
asked twice.

Status codes: 400 for a bad resolution, missing or unusable text, an ambiguous
accept, or a challenger that does not contest the entry; 403 for any agent
caller; 404 for an id the caller cannot see or that is not believed; 409 for an
ambiguous id or an entry that is not disputed.

A refused request writes nothing. Each note touched is checked for write access
before the first write. A resolution across several notes is not one atomic
transaction; if a later write fails, the earlier ones stand and the response
says so by status.

## The audit trail

Every resolution is a belief change, so it appears in `GET /api/memory/changes`
(and the memory stream) with no extra storage:

- `keep` produces a `retracted` row for each challenger, with `agent: "human"`.
- `accept_challenger` produces a `changed` row whose text is the accepted fact
  and whose `replaced_text` is the fact it replaced.
- `merge` produces a `changed` row per replaced entry, each pointing at the
  merged text.

`keep` also sets the human flag on the original. That is a change to who stands
behind the fact, not to what is believed, so it has no row of its own.

## Surfaces

- **HTTP**: the two routes above, classified in `route_audit_test.go` and probed
  in `leakprobe_test.go`.
- **MCP**: `memory_disputes` (read) and `memory_resolve_dispute` (write, marked
  destructive). Both forward the MCP server's configured agent name, so an
  agent-configured MCP client gets the agent refusal.
- **CLI**: `grimoire memory disputes`, and
  `grimoire memory resolve ID keep|accept|merge [text] [--challenger ID] [--path P]`.
- **Web**: a Disputes panel in the React frontend, opened from the command
  palette ("Memory disputes").

## Relation to challenges

`/api/memory/challenges` and `POST /api/memory/challenge` (uphold or concede)
predate this and read the same data. Disputes are the fuller form: keep,
accept, or merge, several challengers at once, and both texts with their
evidence. The challenge routes are unchanged.

## Limits

- Settling is a person's act and there is no way to hand it to an agent, even
  an agent the operator trusts. If that is ever wanted it needs a new authority
  rung, not a relaxed check here.
- The id is the entry's content hash, so two byte-identical facts in different
  notes share an id. The `path` field disambiguates; without it the request is
  refused rather than guessed.
