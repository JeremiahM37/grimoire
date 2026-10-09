# Procedures as skills, mined skills, and impact

Part of "memory for any agent" (see AGENTS_ANY.md and MEMORY_STORE.md).

## Export: procedure memories as native skills

    grimoire skills export --agent claude-code [--dry-run] [--force] [--dest DIR]
    grimoire skills export --agent claude-code --remove

Every procedure memory in the store becomes a folder in the agent's skills
directory (profile `skills_dir`: `~/.claude/skills`, `~/.codex/skills`):

- `SKILL.md`: `name` (slug of the title), `description` (title plus
  "Use when ..." built from the memory's cues in its frontmatter: `cues`,
  `when` or `applies_when`; the memory's description when it has none), the body.
- `scripts/steps.sh` when the procedure holds fenced `bash`/`sh` blocks. It is
  not executable; the SKILL.md tells the reader to look before running.
- `.grimoire-managed.json`: the marker (agent, source note, hash, files,
  `exported_at`, `updated_at`).

Rules: a folder without the marker is never touched; a marked skill whose files
were edited by hand is skipped unless `--force`; a second run changes nothing
(not even the dates); a procedure that looks like it holds a secret is not
exported; skills whose procedure is gone are reported as orphans and stay until
`--remove`, which deletes only the files the marker lists.

## Mine: recurring command sequences become drafts

    grimoire skills mine [--agent NAME]... [--since 30d] [--min-sessions 3] [--title]
    grimoire skills accept ID

Reads session transcripts of every profile with a `transcripts` section, reduces
each session to its successful shell commands (normalised to `git push`,
`go test`, `systemctl restart`; trivial commands and heredoc bodies dropped),
and finds contiguous runs of 3 to 8 steps that recur in at least N distinct
sessions. A longer run absorbs a shorter one that recurs as often. Drafts go to
`<vault>/.grimoire/skill-candidates.md` (and `.json`): nothing becomes a memory
until `skills accept` saves one into the store. `--title` lets the configured
model title and describe a candidate; it decides nothing else and a failure of
it changes nothing.

## Impact: friction before and after

    grimoire memory impact [--since 90d] [--agent NAME] [--json]
    GET /api/memory/impact

Per session: tool errors, prompts, user pushback ("no, ...", "I told you"),
repeated prompts, duration. Per memory and per exported skill: the sessions
before and after the date it landed (frontmatter `created`, else file mtime;
for a skill the export date), with session counts and medians, all sessions and
the on-topic subset. **Correlation only**: a memory is usually written because
something went wrong, so "before" is expected to look worse. Needs at least 5
sessions on each side or it says so.

### Re-tells per 100 prompts

    grimoire memory impact --retells [--cutoff 2026-10-09] [--budget 2000]
    GET /api/memory/impact?retells=1

The headline metric: how often the user had to say again what memory already
held, by ISO week, over the whole transcript history. For each user prompt the
best-matching memory is found by shared terms; the configured re-tell judge
(`retell_url`, the same question the live path asks) or, with none, a strict
lexical rule says whether the prompt restates it. Judged pairs are cached in
`.grimoire/retell-judged.jsonl`, so re-runs call the judge only for new pairs.
Over `--budget` new pairs a fixed sample is judged and counts are scaled (the
report says SAMPLED). The rate is a floor (re-tells in other words are missed).
BEFORE is everything before the cutoff, AFTER everything since; a column shows
the re-tells where the memory already existed at that time.
