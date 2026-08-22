# Correction durability — protocol

Pre-registered before the arms were run. Written down here rather than in a
commit message so that the bar, the population and the failure conditions cannot
move after the numbers land.

## The question

Every published agent-memory benchmark asks whether the **current** value of a
fact is used. LongMemEval and LoCoMo score recall; BEAM and MemoryAgentBench add
knowledge-update and contradiction-resolution categories; Memora's FAMA
penalises answering from superseded memory. All of them measure the value an
agent **reads**.

None asks whether a correction a **person** made still exists after the agent
writes again. Not an oversight: in a vector store or an embedded blob a person
cannot make one, so the question has nowhere to land. Grimoire's memory is
markdown a person edits, which is the whole pitch, so the question is both
askable and load-bearing.

> Given that a person has corrected a fact, does the correction survive the
> agent's next write on the same slot?

## Why it needs measuring rather than asserting

Reconciliation compared facts by recency. A correction therefore stood until the
agent wrote again, at which point it was superseded — and the superseded row is
the one struck through, so the history recorded the **person** as the party who
had been corrected. Both halves are worth separate counts: losing a value and
misattributing who was wrong are different harms, and a fix could close one
without the other.

## Population

The `knowledge-update` subset of LongMemEval, the same set and the same
`evidence_pairs` extraction that `longmemeval/slot_probe.py` uses, so the two
probes measure the same pairs and their results can be read against each other.

A pair is **scorable only if the engine recognises it as an update at all** —
shown the two statements in order, it must return `UPDATE`. Durability is
meaningless where no overwrite would be attempted: a pair the engine does not
recognise would score "survived" in every arm, including the broken one, and
padding the denominator with those would turn a null into a pass. Unrecognised
pairs are reported as dropped, never scored.

This bounds the study. Recognition is roughly a quarter of the set, so the
ceiling here is the ceiling `slot_probe` already measured, and the durability
result says nothing about the pairs the engine misses.

## Procedure

Per pair, in a fresh topic:

1. the agent writes the earlier value `v1`
2. a person corrects it to the later value `v2`
3. the agent meets the earlier evidence again and writes `v1` a second time
4. the note is read from disk and the person's line classified

Step 2 runs two ways, because they exercise different machinery:

- **handedit** — the bullet's text is rewritten in the file, which is what a
  person actually does. Nothing declares it; the engine has to notice that the
  id no longer hashes to its own content.
- **declared** — written through the API with `human: true`.

## Arms

`GRIMOIRE_MEMORY_AUTHORITY=off` collapses the human and agent rungs, restoring
recency-only supersession exactly as it behaved before the lattice existed. It
is the control. The `pulled` rung is unaffected and cannot be disabled — that
one is a security control.

## Outcomes

| outcome | meaning |
|---|---|
| `survived` | `v2` is live and unstruck; `v1` is recorded alongside |
| `resurrected` | `v2` is struck through — the correction was destroyed |
| `inverted` | `v2` carries `sup=`, so the history names the person as corrected |
| `lost` | `v2` is not in the file at all |

`inverted` is a strict subset of `resurrected`; both are reported.

## Measurement properties

No reader, no judge, no model: the classifier reads the markdown. The same input
gives the same outcome forever, so a difference between arms is a real
difference and not a sample. That also means **a p-value here is close to
ceremony** — a deterministic mechanism either applies or does not, and the
useful claim is the count of pairs where it applied, not the probability of
observing it by chance.

## Pre-declared bar

- **H1** — under recency-only resolution, resurrection rate is materially above
  zero.
- **H2** — under the authority lattice, resurrection and inversion are zero *by
  construction*, and no pair that previously superseded correctly stops doing so.
- **H3** — the lattice has a cost: every refused overwrite becomes a decision a
  person must make. It is quantified as the challenge rate, not waved at.
- **H0 — the null worth publishing.** If the control does not resurrect,
  the failure is theoretical and the mechanism is unnecessary complexity. That
  result gets written up in this directory exactly as any other would.

## Failure modes this protocol has already hit

Recorded because both produced a clean-looking result that meant nothing, and a
reader deciding whether to trust the numbers should know what they survived.

1. **An empty denominator that read as a pass.** The first harness selected the
   first fact extracted from each raw turn. Those are usually about different
   things, so the agent's second write contradicted nothing, no supersession was
   attempted, and *both arms scored 100% survived*. Selection by the engine's own
   `UPDATE` verdict is what fixed it.
2. **A readiness check that matched the wrong note.** The wait for the hand edit
   to reach the index searched the whole vault, and the selection step had
   written the same later value into a scratch topic — so it reported success
   while the note under test still held the old text. Reconciliation then
   compared against stale text and answered `NOOP: already recorded`. The check
   is scoped to the trial note.

Both were found by disbelieving a 100%-clean control, which is the only reason
they were found at all.
