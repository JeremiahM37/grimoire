# Lab: `justify` (justified derived facts with dependency invalidation)

Flag: `GRIMOIRE_EXP=justify`. Off by default; with the flag off no table is
created, no output gains a field, and every existing test and golden file
passes unchanged.

## What it does

`go/internal/justify` is a truth-maintenance store. Each derived item (a bank
observation, a mental model's answer) records one or more justifications; a
justification is a set of premises, each named by id and the content hash it
had when the item was derived from it. An item is valid while it is present
and at least one justification is valid. A justification is valid while every
premise is valid and still has the hash it was derived from.

Operations (all in one transaction under the database write lock):

- `Invalidate(ids...)`: forgets premises; returns the derived items that lose
  their last valid justification, transitively. Items with another valid
  justification survive. `Plan` is the same without the write.
- `Recompute(id, hash)`: early cutoff. An unchanged hash touches nothing. A
  changed hash makes the justifications that cited the old hash invalid,
  propagating from there.
- `Derive(id, kind, hash, justs...)`: records justifications (deduplicated),
  refuses cycles, and can revive an item.
- `Stale(current)`: items that are invalid, or have a justification whose
  premise is gone or has a different hash, with reasons.

Nothing is deleted. An invalid item keeps its row and a reason.

Wiring (all behind the flag, in `go/internal/bank/justify_hooks.go`):

- **Observations.** Consolidation records every support set an observation
  received in a batch as one justification (premises = those facts). Listings
  and `GET` of an observation carry `stale` and `stale_reason` when the store or
  the current fact texts disagree with the observation.
- **Mental models.** A full refresh records the units it was based on
  (`justify_stale` / `justify_reason` on list and get). Delta refreshes are not
  recorded.
- **`forget --cascade`.** The store's view is added to the report. Facts the
  cascade removes are forgotten in the store (or planned on a dry run); each
  observation or model the store says loses its last justification is reported
  as `justification` / `justify_lost`. An observation or model the cascade
  retracts or re-derives that the store still holds valid is reported as
  `justify_kept`. The cascade's own behaviour is unchanged.

Namespacing: `u:<bank>:<unit id>` for facts and observations, `m:<bank>:<model>`
for models. Premise hashes are the hash of the unit's text.

## Verification

Run from `go/` with `env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP`.

| Command | Result |
|---|---|
| `gofmt -l .` | clean |
| `go vet ./internal/justify/ ./internal/bank/` | clean |
| `staticcheck ./internal/justify/... ./internal/bank/...` | clean |
| `go build ./...` | ok |
| `go test ./internal/justify/ -count=1` | ok (~10 s) |
| `go test ./internal/bank/... ./internal/index/... ./internal/api/... -count=1` | ok (api 38 s, index 37 s) |

Property test `TestInvalidationMatchesRecompute` (seed 20261011): 2000 random
derivation graphs (2-5 base, 2-15 derived, 1-3 justifications of 1-3 premises
each), up to 25 random operations per graph (forget, content change to one of
three hashes, plan). After every operation the stored validity of every item,
the lost set of the operation, and the stale set must equal a from-scratch
least fixpoint computed from the recorded graph and the current world.

- 11,867 operations; 2,208 of them lost at least one item; 6,038 items lost in
  total; 7,557 re-evaluations.
- Zero mismatches.

Deterministic tests: `TestAlternativeJustificationSurvives` (forgetting one of
two justifications loses nothing; forgetting the second loses the item and its
dependent), `TestEarlyCutoff` (same-hash recompute re-evaluates nothing; a
changed hash reaches dependents; re-deriving on the new hash revives them),
`TestCycleRefused`, `TestPlanDoesNotWrite`.

Bank tests (`justify_hooks_test.go`): with the flag off, no store table exists
and listings carry no stale fields. With it on, the consolidated observation is
recorded valid, a dry-run cascade reports it as `justify_lost` without writing,
and a real forget records the loss.

## Measurements

Graph size and timing, `go test ./internal/justify -run xxx -bench Invalidate`:

| Graph | Items | Derived | Justifications | Premise edges | Forget (Plan) | Items lost per forget |
|---|---|---|---|---|---|---|
| layered DAG, seed 1 | 2,200 | 2,000 | 4,073 | 8,036 | 0.81 ms/op | 3.26 mean |

Not measured: a from-scratch recompute of the same graph through the store.
The from-scratch oracle exists only in the tests, so no speed comparison is
claimed. Not measured: any real bank. The lab protocol (`bench.py`, gold in
top 10) does not apply: this is not a ranking idea and changes no retrieval
result.

## Limits

- **No backfill.** Banks consolidated before the flag was on have no rows; a
  forget reports nothing about their observations until they are re-derived.
- **Fact edits are not pushed.** A person's edit to a fact changes the unit
  text, which is seen by the read path (`stale`, `premise_changed`) but does
  not update the store's stored premise hash. The store's own validity can
  therefore disagree with the read path until something re-derives.
- **Deleted items.** Deleting an observation or model does not tell the store.
  The cascade report filters to items still in the bank's files, but the rows
  stay valid in the store.
- **Delta refresh and cascade layer 3.** Delta-mode model refreshes are not
  recorded. Models the cascade deletes are reported through the store only when
  they still exist as files.
- **Early cutoff is at the hash boundary.** A dependent whose own content would
  come out the same is still invalidated when a premise's text changed, because
  it cites the old hash. This is stricter than a Salsa-style output comparison.
  It is the right default for "is this still supported by what it cites", and
  the wrong one if an item's output is what matters.
- **Cost.** Each operation runs SQL per touched item (a worklist over the
  dependency edges). Reads that list observations or models load the bank cache
  and the whole store's stale set.

## Verdict

**KEEP BEHIND FLAG.** The invalidation logic is correct against the
from-scratch oracle over 2,000 random graphs, and the wiring is small and
inert when off. But nothing has measured that the precise dependent set helps
on a real bank. By construction the cascade and the store agree on items with a
single justification, and disagree only when an item has an alternative
justification (the store keeps it, the cascade removes it); that count has not
been taken on real data. Graduating needs (1) a backfill so existing banks have rows,
(2) fact-edit hooks so the store's premise hashes do not lag the text, and (3) a
comparison against the cascade on a real bank showing a difference that matters
to someone.
