# Memory privacy: the `vis=` tag

Each remembered fact can carry a visibility. It is written in the bullet's
trailer like every other per-fact field, so the note file is still the source
of truth and a person can read it:

```
- **2026-10-10 09:00 · claude** — the staging token rotates weekly <!--m id=… vis=private-->
```

| value | meaning |
|---|---|
| `normal` (default) | ordinary fact. Not written to the bullet; a bullet with no `vis=` is normal. |
| `private` | hidden from default reads. |
| `sensitive` | hidden from default reads, and its text is redacted wherever an explanation or receipt would repeat it. |

Set it with `remember` (`visibility` in the API and MCP, `--visibility` in the
CLI). A replacement inherits its predecessor's visibility unless the writer
sets one, so a restatement cannot quietly publish a private fact. A value the
parser does not recognise is treated as `private`: a hand-edited typo hides a
fact rather than publishing it.

## What the tag does

A hidden fact is excluded in the index, in SQL, before ranking and before the
scan bound. Nothing downstream can count it, rank it or name it.

On a default read (no `include_private`):

- **recall**, including `expand`, `hops`, `explain` and the note-level view
  `shape=notes` (whose bodies have the hidden bullets removed)
- **profile**, **context** (agent injection), **briefing**, **changes**,
  **graph**, **facets**, vector search, challenges, timeline, adherence
- **dream** analysis reads the memory notes with hidden bullets removed, so no
  dream finding or report carries one
- **receipts**: a receipt for a hidden fact is listed only with
  `include_private`
- **prune** never proposes a hidden fact for retraction

`include_private=1` (API and MCP; `--include-private` in the CLI) opens recall,
notes, profile, context, briefing and changes to that request. It does not
change who may read the memory at all: see below.

Two surfaces never carry a hidden fact, whatever the request says:

- **export** (JSON, markdown and JSONL)
- **the stream** (`/api/memory/stream`)

A sensitive fact's text is also redacted from **explain** output, and its
receipt keeps no salted hash, salt or word count, since a salted hash of a
short secret is a guess target. The receipt says only that a forget happened.

## What the tag does not guarantee

- **It is not an owner boundary.** Agent memory lives in the shared commons
  by design: `remember` writes `memory/<topic>.md`, which every account can
  read (`auth.PrincipalFor`). A member who passes `include_private=1` reads
  another member's private fact. Anonymous callers still read no commons
  memory at all. `private` and `vis=` are retrieval filters, not access
  control; see `SECURITY-LEAKPROBE.md`, design finding D1 and D3.
- **It does not encrypt or hide the file.** The bullet is plaintext in
  `memory/<topic>.md`. Anyone who can read that note by path (`GET
  /api/notes/memory/…`), or open the vault directly, reads the fact. The vault
  syncs to the phone, and the vault export, backups and git history carry it
  too. The note-level index is the exception: hidden bullets are left out of
  its full-text rows, chunk embeddings, extracted facts and blocks, so search,
  context and ask do not return them.
- **It is not a secret store.** A secret value belongs in the credential vault
  (`use_credential`), where the agent uses it without reading it. `sensitive`
  stops a fact from being repeated by explanations and receipts; it does not
  keep the value out of the vault.
- **Existence can still leak in a few places.** A supersession or retraction
  by a hidden fact can show as a change to the visible fact it replaced (the
  visible fact is listed as replaced, with no hidden text). The admin health
  check's entry count includes hidden facts; usage counts do not.
- **Learned cues are not filtered.** A cue (`context`) is stored against a fact
  and is not hidden with it.
- **The note-level `private` flag is a different thing.** `private` in a note's
  frontmatter hides the note from automatic and agent retrieval. `vis=private`
  hides one fact. Neither is an owner boundary.

## Where it is enforced

- `go/internal/memory/entry.go`: the trailer (`vis=`), `NormVisibility`,
  `Hidden`, `Redacted`, `DropHidden`
- `go/internal/index/memory.go`: the `visibility` column and the SQL predicate
  `visibility=''` unless `MemoryQuery.IncludePrivate`
- `go/internal/index/index.go`: the note-level rows (body, full-text, chunks,
  facts, blocks) are written from the note with hidden bullets removed
- `go/internal/api/memory.go`, `memory_cascade.go`, `memory_stream.go`,
  `memory_portability.go`, `dream.go`: the per-surface choices above
- `go/internal/api/leakprobe_test.go`: `TestLeakprobeHiddenMemoryStaysHidden`
  and the seeded canaries `vis-private` and `vis-sensitive`
