# Selective automatic memory

Grimoire does not require Lectern, an agent framework, or automatic context.
Explicit MCP tools remain available in every mode. Automatic context is a
separate, opt-in read path: it does not record transcripts or write memories.

## Choose the policy

| Hook mode | Behavior |
|---|---|
| `manual` (default) | No automatic requests or injected context. Use MCP when wanted. |
| `off` | Same automatic behavior; useful as an explicit disable override. |
| `scoped` | Look only in an explicit list of vault-relative files/directories. |
| `all` | Search the whole corpus the caller can read, subject to the exclusions below. |

Set `GRIMOIRE_CONTEXT_MODE`. In scoped mode, set `GRIMOIRE_CONTEXT_PATHS` to a
JSON array, for example:

```bash
export GRIMOIRE_CONTEXT_MODE=scoped
export GRIMOIRE_CONTEXT_PATHS='["memory/kestrel.md","Projects/Kestrel/","Agent Memory/project_kestrel.md"]'
export GRIMOIRE_URL=http://127.0.0.1:9111
```

A path ending in `/` means a directory prefix on segment boundaries. Other
paths match exactly. There are no globs, substring project-name matches, or
fallback to all notes if the scope is empty or unavailable. Scopes narrow
retrieval; they never grant access. Configure aliases explicitly rather than
guessing a project from a working-directory basename.

The automatic path excludes private notes, imported/untrusted content, expired
and superseded memories, and disputed claims awaiting review. Ordinary trusted
notes have `authority: unknown`, not an invented human author. `all` does not
override these exclusions; explicit authorized tools cover broader retrieval.

## Native Claude Code / Codex hooks

`clients/hooks/grimoire_context.py` is a standalone Python 3 standard-library
script. Merge these entries with existing hooks; do not replace unrelated hooks:

```json
{
  "hooks": {
    "UserPromptSubmit": [{"hooks": [{
      "type": "command",
      "command": "python3 /absolute/path/to/grimoire/clients/hooks/grimoire_context.py",
      "timeout": 3
    }]}],
    "SessionStart": [{"hooks": [{
      "type": "command",
      "command": "python3 /absolute/path/to/grimoire/clients/hooks/grimoire_context.py",
      "timeout": 3
    }]}]
  }
}
```

Claude Code uses `.claude/settings.json`; Codex uses `.codex/hooks.json` (or the
corresponding user-level files). Supply the environment in the agent launcher
or a trusted wrapper. On Windows use the installed Python executable. A new
session and the host's hook trust/approval flow may be required. These files
are examples, not an installer; nothing changes the operator's configuration.

References: [Claude Code hooks](https://code.claude.com/docs/en/hooks),
[Codex hooks](https://developers.openai.com/codex/hooks).

### Cost controls

- No generation or embedding model calls on automatic retrieval.
- Obvious acknowledgements and continuation prompts cause no HTTP request.
- Other prompts get a lexical lookup; only sufficiently overlapping results
  enter context. No match means empty output, not a reminder to try again.
- Default **2,400 UTF-8 bytes**, including the reference wrapper and metadata,
  and at most five items. `GRIMOIRE_CONTEXT_MAX_BYTES` allows 128–8,000 bytes.
  This is an exact byte ceiling, **not** a tokenizer-exact token count; token
  costs depend on the model/language. Roughly 600 English tokens is an estimate.
- Fingerprints deduplicate facts within a session for 30 minutes. Changes to
  text/authority create a fresh fingerprint. Resume, clear, and compaction
  reset the native hook's cache. Identical queries are skipped for 30 seconds.
- Local state contains hashes/timestamps, never note bodies, prompts, or keys.
  `GRIMOIRE_CONTEXT_STATE_DIR` changes its directory; deleting it resets recall.
- HTTP timeout is 1.5 seconds; no redirects, ambient HTTP proxies, or remote
  plaintext HTTP. Use HTTPS for a remote server and `GRIMOIRE_AUTH_TOKEN` if
  authentication is required. Credentials are never printed.
- Failures are fail-open: work continues without automatic context. Set
  `GRIMOIRE_CONTEXT_DEBUG=1` for a content-free stderr warning.
- No Stop hook is needed, and no forced extra model turn is introduced.

This is conservative selection, not semantic omniscience. Paraphrases without
lexical overlap may be missed. Broad natural-language prompts may exceed the
relevance threshold or input bound. Explicit `search_notes`, `recall`, and
`ask_notes` remain the fallback. Injecting context cannot guarantee the agent
obeys it; previous context can remain stale after an expiry or deletion.

## API for any host

`GET /api/memory/context` accepts `q`, `scope=all|scoped|manual|off`, repeated
`path`, `max_bytes` (default 2400; maximum 8000), `limit` (default 5; maximum 10),
and comma-separated `exclude` fingerprints. It returns `context`, `keys`,
`bytes`, `max_bytes`, `mode: lexical`, and `model_calls: 0`.

The endpoint defaults to `scope=all` because invoking it is itself an explicit
read. Hosts choose whether to invoke it; the native hook defaults to manual.
An empty query with explicit scoped paths requests a small project-start
briefing. An empty query in all mode does not dump the vault. Scope filters
run before candidate limits. ACLs and spaces are still enforced.

## Reliable correction targets

For a correction to an identified fact, MCP `remember` and `POST /api/memory`
accept `target_id`, `target_path`, and `expected_text` together. Supply exactly
the previously recalled text and one complete replacement fact in `text`.
The replacement stays in the target note (`topic` does not redirect it).

This avoids fuzzy recognition and model extraction entirely. A changed target
returns 409; a missing/unreadable/non-current target is rejected. Protected
human or immutable facts still create a challenge instead of being overwritten.
It is not an authorization bypass: the normal read/write boundaries apply.
The filesystem is re-read so a pending index update cannot conceal a hand edit.
It is not a general filesystem transaction against arbitrary concurrent writers.

Default fact recall now omits unresolved challenges. Use
`include_challenges=true` to inspect them, or the challenge review endpoints to
uphold/concede. Exports and explicit historical retrieval retain the record.
Recognized protected disagreements cannot be discarded by a later model verdict.

These changes improve correction delivery and selective recall; they do not
establish a new LongMemEval score or solve arbitrary contradiction recognition.

## Lectern integration

New Lectern projects automatically receive a unique managed note at
`memory/lectern-<random-id>.md` when automatic memory is enabled. The association
is stored in Lectern's database, survives renames, and is also created for
imports and promoted sessions. Lectern reports setup status and provides an
idempotent retry endpoint; failed setup does not overwrite notes or block project
creation. Deleting the project does not delete its accumulated memory.

New projects read this managed note by default, plus any explicitly configured
reference paths. Launch/task prompts include a short destination hint so agents
can write durable facts to the same topic; that hint is additional to the
retrieval byte budget. Requested handoffs use the same topic and topic-scoped
reconciliation. Manual/off modes do not provision notes or make automatic reads.

Lectern's optional Grimoire provider owns the mapping from an assigned
Lectern project to this generic path-scoped API. It automatically supplies
bounded context for task dispatch, interactive launch/resume, and messages
submitted through Lectern's send API. No project assignment means no lookup.

`LECTERN_GRIMOIRE_CONTEXT_MODE` selects `project` (default with a configured
provider), `all`, `manual`, or `off`. `LECTERN_GRIMOIRE_CONTEXT_PROJECTS` is a
JSON object keyed by the exact Lectern project name:

```json
{
  "kestrel": {
    "mode": "scoped",
    "paths": ["memory/kestrel.md", "Projects/Kestrel/"],
    "max_bytes": 1800
  },
  "private-experiment": {"mode": "manual"}
}
```

For existing projects without a managed topic or override, project mode uses `memory/<slug>.md`, `memory/<slug>/`, and
`Agent Memory/project_<underscore_slug>.md`. These are explicit conventions,
not discovery of every related note. Map additional project documents yourself.
Older servers fail open with no automatic context, never an unscoped fallback.

Lectern keeps per-session fingerprints in memory and deduplicates sent context
for 30 minutes. A service restart resets this cache. It does not observe text
typed directly into an attached terminal or the host's compaction events; for
those paths install the native hook with the same project scope. Avoid enabling
both paths for the same messages unless you accept separate deduplication caches.
Enablement is controlled by the host configuration, not by installing Grimoire.
