# Memory oracle: a reference model of the write path (N8)

Status: test-only. There is no experiment flag. The package is
`go/internal/modelcheck` (model, generator, shrinker) and the runner and
checks are in `go/internal/api/memory_oracle_test.go`,
`memory_oracle_findings_test.go` and `memory_supersession_bench_test.go`.

## What it does

1. **Reference model** (`modelcheck/model.go`). A few hundred lines written
   from the documented rules, not from the implementation: `MEMORY_WRITING.md`
   (update, do not add), `DISPUTES.md` (keep, accept, merge), `ARCHITECTURE.md`
   (a person's entry is never altered by an agent-initiated forget), and the
   lattice comment in `internal/memory/authority.go` (a write may supersede an
   entry at its own rung or below; an outranked write is recorded alongside as
   a challenge). Authority is pulled < agent < human. Each fact is live,
   superseded (by a named fact), retracted (with attribution) or forgotten
   (hard, tombstoned). Visibility: private facts are hidden from default reads.
2. **Generator** (`modelcheck/gen.go`). Seeded, deterministic sequences of
   writes (50%), soft forgets, hard forgets, dispute resolutions (keep or
   accept, by a person or an agent), reindex, restart, and as_of probes. Ops
   name facts by ledger ordinal modulo the ledger size, so a sequence stays
   meaningful after shrinking.
3. **Runner** (`api/memory_oracle_test.go`). Applies each sequence to a fresh
   server over a temporary vault and to the model. Time is virtual: each write
   advances `vault.Now` by one minute, so stamps and as_of instants are exact.
   After every step it compares the note file (every ledger fact's state,
   human flag, challenger pointer, private visibility), the default recall, the
   private recall, and the store-only invariants below.
4. **Store-only invariants** (checked on the store alone):
   - conservation: the number of tombstone lines equals the number of hard
     forgets; each entry is live, superseded, retracted or absent;
   - a supersession chain never cycles, and an entry id appears once in a note
     (a fork would show as a duplicate);
   - a human entry is never superseded by, or retracted by, a non-human writer;
   - after a hard forget the text is not in the note, not in the export, and
     recall with every include flag does not return the entry's id.
5. **Shrinker** (`modelcheck/gen.go`, `Shrink`). Removes chunks, then single
   operations, while the failure signature is unchanged. `RegressionTest`
   renders the result as a paste-ready Go test.
6. **Supersession replay** (`memory_supersession_bench_test.go`). Ordered
   updates to one slot, each with a `valid_from`. After every update: the top
   default recall, the current valid_at answer, and the combined question
   "as_of just after update j, valid_at day j" for every j so far.

Default budget: 120 sequences of 30 operations, about 11 s, one go test run.
`GRIMOIRE_ORACLE_SEQS=N` raises the count. Divergences are grouped by signature;
a signature listed in `oracleKnownFindings` is logged and the run continues,
anything else fails the run with a minimal sequence.

## Measurements

All numbers come from this branch. Commands are below each table.

Oracle sweep, 30 operations per sequence. Each count is the number of
sequences whose first divergence is that finding (F4 has two shapes, shown as
one column). Sequences with no divergence are the remainder.

| universe | sequences | diverging | F1 | F2 | F3 | F4 | F5 | F6 |
|---|---|---|---|---|---|---|---|---|
| two subjects (default run) | 120 | 119 | 67 | 23 | 19 | 9 | 1 | 0 |
| two subjects | 1500 | 1479 | 895 | 305 | 194 | 80 | 3 | 2 |
| one subject (`GRIMOIRE_ORACLE_ONE_SUBJECT=1`) | 1500 | 1423 | 0 | 524 | 522 | 354 | 11 | 12 |

Here F1 is `[subject-slot]`, F2 `[lattice-forget]`, F3 `[dead-forget]`, F4
`[supersession]` plus `[multi-contender]`, F5 `[resolve-mismatch]`, F6
`[retraction]`. The default run has all 119 divergences known and passes. The
one-subject run removes F1, so the later classes are reached; it is a
measurement, not a product configuration. Sequences clean in the 1500-seed runs:
21 of two-subject, 77 of one-subject.

Commands:

```
cd go && env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP go test ./internal/api -run TestMemoryOracle -v
cd go && env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP GRIMOIRE_ORACLE_SEQS=1500 go test ./internal/api -run TestMemoryOracle -v
cd go && env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP GRIMOIRE_ORACLE_ONE_SUBJECT=1 GRIMOIRE_ORACLE_SEQS=1500 go test ./internal/api -run TestMemoryOracle -v
```

Supersession replay (`TestSupersessionReplay*`):

| arm | updates | checks | failures |
|---|---|---|---|
| chain: agent updates only, values cycle, 8 updates | 8 | 52 | 0 |
| correction: person writes at step 2, agent writes after, 5 updates | 5 | 25 | 0 |

A check is one comparison between the store and the arm's expectation. The
correction arm expects every agent update after the person's write to be a
believed challenger, valid on each later day, so the combined query returns
the person's value plus every challenger since. These numbers say the replay
passes; they are not a benchmark score.

## Findings

Each finding is one class of divergence. For each: the minimal sequence (ops
use the model's writers: `me` a person, `claude` an agent, `jira` pulled
connector text), what the store did, what the documents say, and which side is
wrong by my reading. Each has a skipped regression test in
`memory_oracle_findings_test.go` that asserts the documented behaviour, and a
guard test (`TestOracleFindingsStillReproduce`) that fails when the sequence
stops reproducing, so a fix is noticed. No product code was changed.

**F1, the value rule ignores the subject (product wrong; high confidence).**
`S0 = Billing Postgres`, `S1 = Analytics Postgres`. Sequence: an agent writes
`Billing Postgres runs on port 5432`; an agent writes `Analytics Postgres runs
on port 6432`. The store reports UPDATE and strikes the Billing fact through.
The subject-blind `ValueUpdate`/`SameSlot` path (slots.go) matches on the shared
word "postgres" and the predicate. `reconcile.go` says the case reconciliation
exists for is "two facts that share a subject and predicate but disagree on the
value". Same for a challenge across subjects.
Suggested fix: require the subject to match in the value-slot path. Not made
here, because it changes slot identity in the shared rules.

**F2, an outranked writer may forget a fact above its rung (product wrong;
high confidence on the rule, medium on which forget).** Sequence: a person
writes `Analytics Postgres runs on port 6543`; a pulled writer hard-forgets it.
The store answers 200 and removes the bullet. An agent's soft forget of a
person's fact is also accepted and attributed `retracted:claude`. The authority
lattice (`authority.go`, `outranked`) forbids an outranked writer from
"superseding, retracting or otherwise rewriting" an entry, and ARCHITECTURE.md
says "a person's entry is never altered by an agent-initiated forget". The
store checks neither in `forgetOne`/`removeEntry`.
Suggested fix: refuse, or challenge as the cascade path does, when the caller is
below the entry's rung. Not made here, because the documented outcome for a
cascade is a challenge entry, which is more than a few lines.

**F3, forgetting a fact that is no longer believed rewrites its supersession
pointer (product wrong; high confidence).** Sequence: a pulled writer records
`Billing ... port 6432`; an agent records `Billing ... port 5432` (supersedes
it); a person soft-forgets the first fact. The store answers 200 and sets its
`superseded_by` to `retracted:human`, so the chain no longer records that it
was replaced. The conservation rule (superseded by an existing fact) is broken.
Suggested fix: a forget of a fact that is not live is a no-op or a refusal.

**F4, the store decides on one contender, not on every contender (product
wrong by my reading; medium).** Two shapes, one cause. The store's value rule
returns one decision for the first candidate it reaches, so the rest of the
slot is not considered.

- Shape A: an agent writes `6543`; a pulled writer writes `6432` (recorded as
  a challenger of the agent fact); an agent writes `5432`. The store reports
  UPDATE for one fact, so the pulled `6432` stays believed beside `5432`. The
  model supersedes every live fact at or below the writer's rung.
- Shape B: an agent writes `5432`; a pulled writer writes `6432` (challenger of
  the agent fact); a pulled writer writes `6543`. The store supersedes the
  same-rung `6432` and never checks the agent fact above it, so the new fact is
  not recorded alongside the agent fact.

The documents do not state the multi-contender case. This is my reading of
MEMORY_WRITING.md ("update, do not add") and the lattice comment ("a write may
supersede an entry at its own rung or below; a write from below is recorded
alongside"). Shape B is the sharper case: a pulled write is never allowed to
supersede the agent fact, and the store's answer does not say so.

**F5, a dispute with a private challenger cannot be settled by a person
(product wrong by my reading; low to medium).** Sequence: a person writes
`5432`; an agent writes `6543` with `visibility=private` (recorded as a
challenger); the person keeps the original. The store answers 409 "that entry is
not disputed", because the dispute listing applies default visibility. DISPUTES.md
lists a dispute "to a caller who may see both"; the person is that caller.

**F6, accepting one challenger leaves the others believed (product wrong;
high confidence, DISPUTES.md is explicit).** DISPUTES.md says `accept_challenger`
"Other challengers are retracted". With two challengers, the store retracts the
chosen one and the other stays believed. The same check fires for `keep` with
several challengers, not separately shown.

Reproduce each with its findings test and `TestOracleFindingsStillReproduce`.

Two model corrections were made during the one-subject run, both recorded
because they change what the oracle would have reported. A person's `keep` now
lifts the fact to the person's rung (the store does this through the human
flag, `Entry.Authority`); the earlier model left it at the agent rung and
reported a correct refusal as a divergence. And where several live facts
outrank a write, the rule does not say which a challenge names, so the runner
accepts any outranking fact and adopts the store's choice for later checks.

## Harness bugs found on the way (and fixed)

These were mine, not the product's, and are recorded so the next reader does
not repeat them: the virtual clock must be local time (the store compares stamps
against `time.Local`; a UTC clock makes every as_of answer empty); the NOOP
target is in the `target` field, not `id`; a person's forget is attributed to
`human`, not to the name; tombstone lines are counted from the report, because
a locked vault's lines are not returned by `Load`; a recall's `superseded_by`
names a forgotten successor's id, so forgotten-text checks compare `id` fields.

## Limits

- Not modelled: sealed visibility (MEMORY-SEAL.md), retraction by negation
  text ("no longer"), immutable facts, validity closures outside the replay,
  multi-note behaviour, extraction, and pulled origins other than `connector:`.
- Hard forget leaves the text in the note's history snapshot
  (`.grimoire/history/…`). ARCHITECTURE.md says only the cascade rewrites
  snapshots, so this is the documented default, not a divergence, and the
  oracle does not gate on it. The text also sits in `index.db-wal` until a
  checkpoint.
- The model encodes my reading of the documents. F4 and F5 are judgement calls
  about cases the documents do not state; F2 rests on the lattice comment and a
  cascade-scoped sentence in ARCHITECTURE.md. If those readings are wrong, the
  finding is wrong too.
- A signature groups by class, so a second cause inside one class is hidden.
  The one-subject run is the check on that for F1.
- Shrinking keeps the signature, not the root cause; the minimal sequences show
  one route to each class, not every route.
- No ranking or retrieval is measured here. The replay is seven-to-eight
  updates per slot; it is a check, not an estimate of accuracy at scale.

## Verdict

GRADUATE (as tests). The package has no flag to graduate. It ships as tests
that run by default in about 11 s. The default run is green only because its
six known classes (F1 to F6, F4 in two shapes) are listed. Each listed class is
an open product finding, not a passing case, and the skipped regression tests
stay skipped until the product changes.
