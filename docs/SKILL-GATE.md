# Skill gate: structured skills and an offline test before accept

Addresses two limits of mined skills. (#9) A skill mined from failed sessions
used to enter the review queue with no automated test, so it could compound the
failure. (#15) Skills were prose, with no checkable preconditions or
postconditions.

## What is built

- **Spec** (`go/internal/skillgate/spec.go`). A skill note carries
  `metadata.skill_trigger`, `skill_pre`, `skill_post`, `skill_sessions` and,
  after a gate run, `skill_gate` (`pass`, `fail` or `override`) and
  `skill_gate_override`. Preconditions and postconditions are `;`-separated
  predicates. Ordered steps are the numbered lines of the note body. A note
  without these keys loads as before (`SpecOf` returns `ok=false`), and the
  existing skill exporter reads only its cue keys, so it ignores them.
- **Predicates** (`predicates.go`).
  - Preconditions, checked by `Applicable(spec, ctx)`: `file:<path>` (relative
    to the working directory), `command:<name>` (on PATH), `env:<NAME>` (set and
    non-empty), `fact:<text>` (a recorded fact contains it, case-insensitive).
    An unknown predicate is unmet, so a check that cannot be read never lets a
    skill inject blindly.
  - Postconditions, checked by `CheckPost(spec, session)`: `ran:<unit>` (the
    session ran that step and the last run was not an error), `last-ok` (the
    session's final shell call succeeded).
- **Gate** (`gate.go`). `Check(spec, pattern, sessions, opt)` treats the sessions
  that contain the candidate's command run as its sources, and every other
  session as held out. The trigger is replayed against each held-out session
  with the existing replay engine (`replay.Prepare` and `Snapshot.Score`). It
  reports:
  - **support**: independent sessions (distinct agent and id) that contain the
    pattern, must be at least 3;
  - **false fires**: held-out sessions the trigger fired on, must be at most 20%,
    and at least 5 held-out sessions are needed to measure it at all;
  - **failure contamination**: sources whose last shell call errored. Flagged
    when more than half of the sources with a recorded outcome failed.
  All thresholds are `skillgate.Options` fields.
- **Where it runs.** `grimoire skills mine` gates every candidate over the
  sessions it read and stores the verdict in the review queue
  (`skill-candidates.json`, field `gate`).
  - `grimoire skills gate ID` prints the verdict and its numbers.
  - `grimoire skills accept ID` refuses a failed gate unless
    `--override "REASON"` is given. The reason is written into the saved note.
  - `grimoire skills check [--cwd DIR]` lists stored skills as `applicable` or
    `unmet`, with the unmet predicates.
- **Replay use.** The replay engine has no embeddings here, so its relevance is
  `0.4 x` the share of the situation's terms found in the memory's text. The gate
  sets the trigger as the situation and each session as the memory, with a floor
  of 0.4, so **every trigger term must appear in the session text**.

## Measured

Only what the offline tests show, on fabricated sessions:

- one source session fails support;
- a skill with four sources and six unrelated held-out sessions passes, with
  zero false fires;
- a skill whose sources mostly ended in a failed shell call is flagged;
- the trigger `test` fires on half of the held-out sessions and fails
  over-breadth;
- two held-out sessions cannot measure false fires, so the gate fails;
- an unmet precondition is reported by name.

Reproduce: `cd go && go test ./internal/skillgate/ ./cmd/grimoire/ -run 'Skill|Gate'`.

No run against real transcripts has been made.

## Limits

- This is a statistical sanity check over recorded sessions. It is not proof
  that a skill works. A passing gate means the trigger is not obviously
  over-broad and the pattern recurs. It does not show the steps are right.
- The trigger matcher is lexical. Words under three letters and the replay
  engine's noise words are dropped, so a trigger of `go test` matches on `test`.
  Triggers are only as precise as their words.
- Failure is read from the shell tool's error flag. A session that failed for
  another reason, or a skill whose steps ran but did not achieve their goal, is
  not caught.
- Held-out sessions are only the sessions in the same mining window. A small
  window fails the false-fire test by design rather than passing silently.
- Not done: an MCP tool for the applicability listing (only the CLI has it); a
  gate re-run on demand without `skills mine`; a gate on the skill-export path
  for skills already accepted before this change (they have no `skill_gate` key
  and load unchanged).
- Designed, not measured: whether the 20% false-fire limit and the support
  threshold of 3 are the right values. Chosen to be conservative, not tuned.
