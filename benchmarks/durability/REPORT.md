# Correction durability — results

Protocol: [PROTOCOL.md](PROTOCOL.md). Raw rows: [results/](results/).
Run 2026-08-22 against `main` at the authority-lattice commit, LongMemEval
`knowledge-update`, 72 evidence pairs.

## Headline

Under recency-only supersession — the behaviour before the authority lattice —
**every** correction a person made was destroyed by the agent's next write on
that slot, and in **every** case the history recorded the person as the party
who had been corrected.

| arm | survived | resurrected | inverted | challenges opened |
|---|---|---|---|---|
| `handedit` · authority **off** (control) | 0/20 | **20/20 (100%)** | **20/20 (100%)** | 0/20 |
| `handedit` · authority **on** | **20/20 (100%)** | 0/20 | 0/20 | 20/20 |
| `declared` · authority **off** | 0/20 | **20/20 (100%)** | **20/20 (100%)** | 0/20 |
| `declared` · authority **on** | **20/20 (100%)** | 0/20 | 0/20 | 20/20 |

Separation is total and the measurement is deterministic — no reader, no judge,
no model — so this is not a sample that happened to fall this way. It is also
not a surprise: the lattice refuses the write by construction. What the run
establishes is that the refusal actually reaches the running server, which is
[precisely what the first implementation got wrong](#what-this-run-found-in-itself).

Inferred and declared authorship behave identically, which matters more than the
identical numbers suggest: the `handedit` arm never tells the server that a
person did anything. It rewrites a line in a file. The engine recovers
authorship from the fact that the entry's id no longer hashes to its own
content, and that inference is doing all the work in the column that describes
how people actually correct things.

## The denominator, and why it is 20 and not 72

**52 of 72 pairs are dropped**, because the engine does not recognise them as the
same fact changing value, so no supersession is attempted and nothing can be
resurrected. Scoring them would have added 52 free "survived" rows to every arm,
including the broken one.

20/72 is the same recognition rate `longmemeval/slot_probe.py` measures
independently (20 of 72 pairs recognised). That agreement is reassuring about the
extraction and damning about the ceiling: **durability is only enforceable where
the update is recognised in the first place**, and three quarters of this set is
not. The lattice protects corrections on the facts the engine sees; the facts it
misses are missed the same way they were before, and closing that gap is
`slot_probe`'s problem, not this one's.

## The cost, quantified rather than waved at

**100% of refused overwrites become an open challenge** — 20/20 in both treatment
arms. That is the honest trade. The lattice does not make the disagreement go
away; it converts a silent overwrite into a decision a person has to make, and at
this rate a busy agent generates one challenge per contested fact.

Two things keep that from being a queue nobody drains, and neither is measured
here:

- a challenge whose contested fact is retracted or superseded by the person
  stops being listed, because the question it asked has been answered by other
  means;
- `concede` exists so a person can rule for the agent, which is how they correct
  *themselves*. Without it the lattice would make an operator's first answer
  permanent — a worse failure than the one it fixes.

## What this run found in itself

Both of these produced clean-looking results that meant nothing, and both were
caught only by disbelieving a control that scored 100%.

1. **An empty denominator reading as a pass.** The first harness took the first
   fact extracted from each raw dataset turn. Those are usually about different
   things, so the agent's second write contradicted nothing and *both arms scored
   100% survived*. A result that looks like the treatment working and is
   measuring an empty set.
2. **A readiness check that matched the wrong note.** The wait for a hand edit to
   reach the index searched the whole vault, and the selection step had written
   the same later value into a scratch topic — so it reported ready while the
   note under test still held the old text. Reconciliation compared against stale
   text and answered `NOOP: already recorded`, which again scored as "survived"
   in both arms.

A third defect was in the server rather than the harness: `GRIMOIRE_NO_WATCHER=0`
**disabled** the watcher, because the check was for presence rather than value
while every other switch in the codebase spells disablement `=off`. External
edits were therefore never reindexed. That is fixed, and falsey values now mean
what they say.

## What this does not show

- **Whether it matters in practice.** Every correction here is synthetic and the
  person is right by construction, because `v2` is the dataset's gold value. How
  often real users correct their agents, and how often they are the ones who are
  out of date, is not something this benchmark can answer. It measures a
  mechanism, not a frequency.
- **Anything about unrecognised updates.** See the denominator above.
- **That challenges get resolved.** The rate at which a person actually settles
  them is a usage question, and an unresolved challenge leaves both facts
  standing — better than the person's being deleted, but not a resolution.

## Reproducing

```bash
go build -o /tmp/grimoire-dur ./go/cmd/grimoire
python3 benchmarks/durability/durability_probe.py \
  --binary /tmp/grimoire-dur --mode handedit --authority off   # control
python3 benchmarks/durability/durability_probe.py \
  --binary /tmp/grimoire-dur --mode handedit --authority on
```

`--mode declared` swaps the hand edit for an API write with `human: true`;
`--limit N` cuts the pair list for a smoke run; `--out FILE` writes the per-pair
rows.
