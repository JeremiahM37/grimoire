# One memory store for every agent

Each agent keeps memory its own way: Claude Code a directory of notes with a
`MEMORY.md` index, Codex and others an instruction file (`AGENTS.md`,
`GEMINI.md`). Left alone, each gets its own copy, and a lesson learned in one is
invisible in the next. Grimoire keeps **one canonical store** and points every
agent at it.

## Kinds

Every memory has a kind, in frontmatter `metadata.kind` (or the `kind` argument
of `remember`, kept as the fact's category): `rule`, `procedure`, `preference`,
`fact`, `reference`. When it is missing it is inferred:

| source | kind |
|---|---|
| Claude `type: feedback` | rule |
| `type: user` | preference |
| `type: reference` | reference |
| `type: project` | fact |
| numbered steps (three or more) or a "how to" opening | procedure (wins over fact) |
| a stored fact starting "never/always/must" | rule |

Recall and `/api/memory/context` items carry `kind`.

## The canonical store and links

The store is a directory: setting `memory_canonical_dir` (env
`GRIMOIRE_MEMORY_CANONICAL_DIR`; falls back to `dream_canonical_memory`;
`grimoire memory` defaults to `~/.grimoire/memory`). Here it is
`~/.claude/projects/-home-admin/memory`.

```
grimoire memory link   --agent claude-code --path ~/.claude/projects/-x/memory
grimoire memory link   --agent codex --path ~/.codex/AGENTS.md
grimoire memory status
grimoire memory unlink --agent codex
```

- **Directory memory** becomes a symlink to the store. The old directory is
  renamed to `<path>.grimoire-bak-<time>`. If it holds files the store lacks or
  has different versions of, `link` refuses and changes nothing; `--merge`
  copies the missing files in, skips identical ones, keeps a differing file
  under `name.from-<agent>.md`, then swaps in the link. Nothing is lost: the
  backup keeps the whole original.
- **Single-file memory** gets a managed block between
  `<!-- grimoire:memory:start ... -->` and `<!-- grimoire:memory:end -->`
  holding the core (below, about 6 KB). Text outside the block is never
  touched; the file is backed up first. Re-run `link`, or `memory index --write`,
  to refresh it.
- `unlink` restores the newest backup, or leaves a real copy of the store, so an
  agent never ends up with no memory. It removes only a block it manages.
- Links are recorded in `<vault>/.grimoire/memory-links.json`; `status` shows
  each agent as `linked`, `directory`, `block`, `block-stale`, ...
- `--dry-run` shows what would happen. Agent paths are given explicitly; the
  agent profiles (WS1) will supply them later through `memstore.Link(kind, path, ...)`.

## Core plus pointers

MEMORY.md is loaded whole into every session, so it must stop growing with the
store. `grimoire memory index` builds:

1. every `rule`, one line each, most useful first (net helpful/unhelpful
   feedback in `metadata`, then newest);
2. one pointer line per topic: `<topic terms>: N notes — recall '<query>'`.
   Topics come from spherical k-means (about sqrt(n) clusters) over the notes'
   embeddings, falling back to a hashing embedder; the label is the cluster's
   most distinctive title words.

It fits 200 lines and 25 KB (`--budget` to change); pointers get up to 55% of the
budget, surplus rules collapse into "N more rules not shown". Without `--write`
it prints. `--write` backs up the old `MEMORY.md` first and refreshes linked file
blocks. The index is generated, the notes are the truth: nothing is edited or
deleted, and the dream's "note not linked" check is skipped for a generated
index (it starts with `<!-- grimoire:generated-index -->`).

`GET /api/memory/core?budget=BYTES&format=md|text` returns the same core as JSON
(`core`, `lines`, `bytes`, `rules`, `rules_shown`, `pointers`); `raw=1` returns
plain text. The store's files are included only for administrators; stored
facts of kind `rule` are included for everyone, filtered like recall.
`grimoire memory core` prints it. An MCP server can use it as its `instructions`.

## Procedure verification

A procedure goes stale quietly: the script moved, the unit was renamed. The
dream checks a few per run, cheaply:

- schedule (state in `.grimoire/procedure-verify.json`, or `verified_at` /
  `verify_every` in the note's metadata): first check at once, then due after 14
  days, doubling per pass to 120; a fail resets to 14 and is re-checked every run
  until it passes;
- at most `memory_verify_per_dream` (default 3) procedures per run, failed first
  then least recently checked;
- deterministic, read-only checkers (`memstore.Checker`, add your own): absolute
  paths exist; `*.service` names exist via `systemctl cat` (skipped without
  systemd); ports named in the text that are in `memory_verify_ports` are
  listening.

A failure becomes a `procedure_verify` dream finding and the item shows
`verify: procedure check failed: ...` on recall and in injected context. Nothing
is deleted or rewritten. A path on another machine will fail here: treat the
tag as "look before you rely on it".

## Writing quality

See MEMORY_WRITING.md: `remember` returns non-blocking `warnings`, the dream lists
`memory_quality` findings.


### Procedures that name another machine

A path, unit or port that belongs to another machine is reported as
"unverifiable here" and is neither a pass nor a failure: a path under another
user's home or `/root`, or anything on a line that carries an `ssh` / `scp` /
`pct exec` / `docker exec` style context, a `user@host` or `host:/path`, an IP
address that is not this host's, a `.ts.net` / `.internal` / `.local` name that
is not this host's, or a name listed in the `memory_other_hosts` setting. A
procedure with only such references is counted as seen, not proven.
