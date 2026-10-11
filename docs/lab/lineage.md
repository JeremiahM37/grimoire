# Lab: `lineage` (source independence for corroboration)

Flag: `GRIMOIRE_EXP=lineage`. Off by default. Off, dispute scoring and explain
output are unchanged (tests and goldens pass with no edits).

## What it does

Dispute scoring (`internal/api/memory_disputes_score.go`) gives corroboration
from the number of distinct sources asserting the same text. Before this lab,
a source was the declared origin (`org=`) when present, otherwise the writer.
Ten agents that each cite one page with `ev=` and set no `org=` therefore
counted as ten.

With the flag on (`internal/api/memory_lineage.go`):

- **Lineage** of a fact, in order:
  1. a cited fact in the same dispute population (`ev=<entry id>`): derived,
     inherits that fact's lineage;
  2. declared origin `org=`;
  3. first cited source, sorted (`ev=` note path or URL);
  4. session it was first stated in;
  5. writer.
- **Corroboration** = distinct lineages asserting the same text. The side
  itself always counts. A side from the same writer as the side being scored
  never adds a lineage, so a writer does not corroborate its own claim.
- **Hop depth**: 0 for a human statement or a verified citation; a declared
  origin or an unverified citation is +1; a derivation is parent + 1. It is
  shown as `hops` in the dispute breakdown and in explain output. It is not
  stored, not written to the index, and not a trailer.
- **Ordering**: hop depth breaks only an exact tie in total, after the human
  rule (`internal/conflict/conflict.go`). It never changes a total and never
  outranks authority.

## Tests

`go test ./internal/api -run Lineage` and `./internal/conflict` (hops_test.go):
one page quoted by ten agents gives corroboration 1 (flag off: 10 writers);
two pages give 2; self-repetition over three origins gives 1; a derived
summary does not corroborate its premise and has hop depth 1; a human still
ranks above five agents on one page; flag-off JSON has no `hops` key;
explain shows hops only under the flag.

## Measurement: copy simulation

Stream per trial: a right claim from two independent pages (two writers, two
`ev=` pages), and a wrong claim written by k writers that all cite one wrong
page. All sides verified, same authority, stamps drawn at random within 60
days. 200 trials per k, seed 1000+k, so the numbers reproduce with
`go test ./internal/api -run LineageSimulation -v`.

| k | flag off: right | flag on: right |
|---|---|---|
| 1 | 200/200 | 200/200 |
| 2 | 104/200 | 200/200 |
| 3 | 0/200 | 200/200 |
| 4-10 | 0/200 | 200/200 |

At k=2 the off-case is a tie on corroboration and the stamp decides it, which
is why it is about half. Ties are counted as not-right.

Benchmark protocol (`benchmarks/latency/bench.py`, sizes 1000 and 10000):
**not run.** This idea does not touch recall ranking (`rankMemory`,
candidates) or the write path, so the latency and gold-in-top-10 numbers
cannot change by construction. The brief's protocol is for ranking ideas.

## Limits

- The simulation is a synthetic stream built to exercise the case the idea
  targets. It is not evidence about real agent behaviour. The wrong copies
  all cite one page, and the right claim has exactly two sources.
- Real-world effect depends on agents writing `ev=`. An agent that declares
  `org=` was already counted once before this change.
- Multi-source citations collapse to the first sorted reference, so two
  agents citing {a,b} and {b} count as two lineages.
- Derivation is followed only inside the dispute population (live, visible
  facts). A parent outside it is an opaque source. Duplicate entry ids across
  notes resolve to whichever hit comes first.
- A derived summary is detected only when it cites the premise by entry id.
  An uncited restatement is counted as independent.
- Hop depth for a fact with no evidence and no origin is 0 (first-hand agent
  statement). This is a choice, not a measurement.
- Not done: hop depth at index time (the brief asked for it; it was computed
  at scoring time instead, to keep the index and the write path untouched).

## Verdict

**KEEP BEHIND FLAG.** The mechanism does what it is designed to do on a
stream built for it, and flag-off behaviour is unchanged. Nothing measured
here, on real data or on the recall benchmark, supports graduating it.
