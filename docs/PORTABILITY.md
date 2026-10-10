# Memory portability

Grimoire can write every agent-memory fact it holds to a file, and read facts
back from that file or from other memory exports. The export is open and
versioned, so it can be read without Grimoire.

```bash
grimoire memory export --out memory.jsonl            # whole store, JSONL
grimoire memory export --format markdown --out memory.md
grimoire memory import memory.jsonl --dry-run        # preview, writes nothing
grimoire memory import memory.jsonl                  # write; re-running is safe
```

## Who sees what

Export and import use the same access rules as recall. An export holds only the
facts the caller may read: a fact in a note whose reader list excludes you is
not in your file and is not reported as a duplicate on import. Import needs
write access to the note each fact lands in, and every fact goes through the
normal write path, so nothing is written around the access checks.

Exports contain the whole memory, so the CLI writes the file with mode `0600`.
Treat it like the vault itself.

## Export format: `grimoire-memory`, version 1

A JSONL file. The first line is a header; every other line is one fact.

```json
{"format":"grimoire-memory","version":1,"exported":"2026-10-10T07:52:22Z","count":2}
{"id":"5a9f6170496a","text":"Backups run at 03:00","path":"memory/ops.md","agent":"cli","stamp":"2026-10-09 03:00","authority":"agent"}
{"id":"ffee00112233","text":"The box is fast","path":"memory/ops.md","agent":"cli","stamp":"2026-08-13 08:30","origin":"connector:slack:C1","superseded_by":"5a9f6170496a","immutable":true,"expires":"2027-01-01T00:00:00Z"}
```

Header fields:

| field | meaning |
|---|---|
| `format` | always `grimoire-memory` |
| `version` | `1`. A reader refuses any other version rather than guessing. |
| `exported` | RFC 3339 UTC time of the export |
| `count` | number of fact lines that follow |

Fact fields. Everything except `id` and `text` is optional and omitted when empty.

| field | meaning |
|---|---|
| `id` | stable fact id, derived from stamp, agent and text |
| `text` | the fact |
| `path` | the vault note the fact lives in, e.g. `memory/ops.md` |
| `agent` | who wrote it |
| `task` | what they were doing |
| `session` | the session or run it was learned in |
| `category` | free-form bucket, e.g. `fact`, `preference`, `procedure` |
| `stamp` | when it was written, `YYYY-MM-DD HH:MM` in the store's local time |
| `expires` | RFC 3339 time after which the fact lapses |
| `immutable` | `true` if reconciliation may not supersede or delete it |
| `authority` | `human`, `agent` or `pulled` (see below). Informational on import. |
| `origin` | where the fact came from, when not asserted by the agent itself |
| `superseded_by` | id of the fact that replaced this one |
| `challenges` | id of a fact this one contradicts but could not supersede |

Readers should ignore fields they do not know. Writers must not add a field
under an existing name with a different meaning; a meaning change is a new
version.

### Markdown export

`--format markdown` writes one bullet per fact, grouped by category, for people
to read and search. It is a view, not an import format. Superseded facts are
struck through and name what replaced them.

```markdown
## fact

- ~~The box is fast~~ (superseded by 5a9f6170496a)  _(2026-08-13 08:30 · cli)_
- Backups run at 03:00  _(2026-10-09 03:00 · cli · agent)_
```

## Import

```
grimoire memory import FILE [--from auto|grimoire|mem0|letta|zep|jsonl-generic] [--dry-run]
POST /api/memory/import?from=auto&dry_run=1      body: the raw file
```

`--from auto` (the default) detects the format from the file's shape, not its
name. Name a source with `--from` only to override a wrong detection.

| `--from` | Reads | Notes |
|---|---|---|
| `grimoire` | `grimoire-memory` v1 export | restores stamp, id, path, origin, supersession, challenge, expiry |
| `mem0` | a list of `{memory, metadata?, created_at?, user_id?, run_id?}` objects (also a `results` wrapper) | `metadata.category` becomes the category; `run_id` becomes the session; `user_id` is kept in the task text |
| `letta` | agent-file JSON (`agents[].memory.blocks`, `agents[].archival_passages`) or a bare `{"blocks": [...]}` | each block with a value is one fact; its `label` is the category; archival passages are category `archival` |
| `zep` | a list of `{fact, valid_at?, invalid_at?, created_at?}` objects (also `facts`/`edges` wrappers) | `valid_at`/`invalid_at` are kept as provenance in the task text, not as expiry |
| `jsonl-generic` | one JSON object per line with `text` (or `memory` or `fact`) and optional `agent`, `task`, `session`, `category`, `stamp`, `expires` | the fallback for anything else |

A file that matches none of these is refused with the reason, not imported as
an empty set.

### Trust and provenance

Every imported fact is written as an **untrusted** fact:

- its `origin` is `import:<source>` (for example `import:mem0`), and its agent
  is the same label when the file names no agent;
- its authority is `pulled`, which is not `human`, so a person's later edit
  wins and an agent may not overwrite a person's fact by re-importing it;
- a file cannot claim to be a person. `"authority": "human"` in an export is
  ignored on import, and the fact is written as `agent` or `pulled`.

A grimoire export is the one case that restores `origin`. That is still
untrusted content from the store's own history: a fact that was `pulled` stays
`pulled`, and one that was agent-written stays agent-written.

### Idempotency and dry runs

A fact is a **duplicate** when its text is already on file, including superseded
and expired facts the caller can read, or when the same text appears twice in
the file. Duplicates are counted and not written, so running an import again
writes nothing.

`--dry-run` (or `dry_run=1`) parses the file, applies the same duplicate check,
and reports what a real run would write, with a sample. It writes nothing.

### Report

```json
{"format":"mem0","dry_run":false,"total":3,"new":2,"written":2,"duplicates":0,"failed":0,
 "skipped":[{"index":3,"reason":"no memory text"}],"sample":["Prefers dark mode in every editor"]}
```

`total` counts records in the file. `skipped` names records that could not be
read at all; `failed` counts records the write path refused (for example a
fact over 20,000 characters). Neither stops the rest of the import.

### Limits

- 16 MiB per import body.
- 5,000 facts per import.
- A fact may be 20,000 characters, as for any write.

## Not carried

These are deliberately outside version 1. Each is listed so that nobody mistakes
an export for a full backup of the store.

- The freshness estimates (`fresh`, `check`, `verified`, change counts). They are
  learned from the store's own history and are re-learned on import.
- Usefulness counts (`helpful`, `unhelpful`).
- The `superseded_at` time, so "what was believed at time T" over an imported
  supersession answers from the import time.
- The human-authorship marker on import (see *Trust and provenance*).
- Memory banks: their facts and observations are separate stores with their own
  access rules and are not in this export.
- The validity window of `zep` facts, which is provenance text only until the
  store can represent it.

## Field mapping from other products

Source-format labels are the only place a product name appears in Grimoire. The
mapping is the list above; a field with no equivalent is dropped, and the list of
what is dropped is the list under *Not carried*.
