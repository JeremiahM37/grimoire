# Injection-stream health

`stream.py` reports how the stream of automatically injected memory behaves,
from the adherence trace and, optionally, Claude Code transcripts. Counts and
shares only: it never prints a path, a fact text or a session id. By default it
reads rows from before the memory holdout started (2026-10-11), so no holdout
arm is involved; `--all` reads every row and still never splits by arm.

```
python3 stream.py --db <copy of adherence.db or a daily snapshot>
python3 stream.py --db <db> --transcripts ~/.claude/projects
python3 stream.py --selftest
```

What it reports:

- `repeat_share`: injections of an item already injected earlier in the same
  session, at either stage. Per stage, a prompt-stage item counts as seen for
  the action stage and the other way round (the definition window dedup uses).
- `top_share`: share of all injections taken by the top 1, 10 and 50 items.
- `by_kind`: stored facts, agent memory files, project notes, other notes.
- With `--transcripts`: whether each logged pre-tool injection reached the
  model (a `hook_additional_context` attachment on that tool call), and, per
  transcript, whether a repeat came in the same context window or after a
  compaction.

## Result on the owner's trace (2026-10-14)

Snapshot of 2026-10-13, rows before the holdout started: 3,297 injections in
79 sessions, 2,812 of them before a tool call.

| Measure | Value |
|---|---|
| Repeat an item already injected in the session | 68.7% (62.3% without the largest session) |
| ... before a tool call / at a prompt | 76.9% / 21.0% |
| Top item / top 10 / top 50 share | 14.1% / 35.1% / 58.0% |
| Agent memory files / other notes / project notes / stored facts | 64.2% / 23.6% / 7.9% / 4.0% |
| Pre-tool injections that reached the model (Claude Code) | 2,689 of 2,698 (99.7%) |
| Per transcript: first showing / repeat in same window / repeat after compaction | 55% / 41% / 4% |

The per-transcript figures count each subagent transcript separately, which is
why they are lower than the per-session repeat share. These numbers motivate
the window dedup mode (`GRIMOIRE_CONTEXT_DEDUP=window`, docs/MEMORY_USE.md)
and skipping always-loaded content (`GRIMOIRE_EXP=loaded`).
