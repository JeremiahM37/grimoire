# Recognising an update nobody phrased like a database write — results

Method: [PROTOCOL-slots.md](PROTOCOL-slots.md). Raw per-pair output:
`results_memory/slotprobe-*.json`, `results_memory/falsepos-*.json`.
Reproduce with `python3 slot_probe.py --binary <build>` and
`python3 slot_falsepos.py --binary <build>`.

**Zero sampling variance.** No reader, no judge, no retrieval — the same two
statements produce the same operation forever. A difference between these
columns is a difference in the code.

---

## The result

72 marked update pairs, whole dataset. 400 unmarked same-haystack pairs as the
false-positive control.

| build | updates recognised | held-out half | false positives |
|---|---|---|---|
| `rules-only` — what shipped | 1/72 (1.4%) | 1/37 (2.7%) | 2/400 (0.50%) |
| **`+ value slots + word numerals`** | **20/72 (27.8%)** | **14/37 (37.8%)** | 3/400 (0.75%) |

**19 more real updates caught, at the cost of one more wrong supersession in
400 pairs.** The new rule's own precision is 19/20 = 95%.

The held-out half — never inspected while thresholds were chosen — improved
**more** than the development half (2.7% → 37.8% against 0% → 17.1%). That is
the opposite of overfitting; the halves simply differ in difficulty.

The pre-declared gate was "false positives must rise by less than 1 percentage
point". They rose by 0.25.

## What was broken

The shipped engine recognises `SUBJECT PREDICATE VALUE`:

```
the user prefers spaces  →  the user prefers tabs        UPDATE ✓
```

That is the shape an agent writes *after* it has decided what the fact is. It
is not the shape a fact arrives in. Here is a real pair from the dataset,
forty sessions apart:

```
"I've been doing some running lately, and I'm happy to say that I recently
 set a personal best time in a charity 5K run with a time of 27:12"

"I'm training for another charity 5K run … By the way, I'm hoping to beat my
 personal best time of 25:50 this time around"
```

Neither parses. Subject, predicate and value all come back empty, so the engine
stored both, recall returned both, and the reader was left to work out which
number was current — which is precisely the failure LongMemEval's
`knowledge-update` category exists to expose.

**On 72 such pairs the shipped rules fired once.** Not 1%-of-writes once — once
in seventy-two chances, on the pairs the dataset marks as being the same fact.

## What was added

A second rule, tried after `Attribute` and before the similarity fallback
(`internal/memory/slots.go`):

> Two statements are the same fact updated when they share at least three
> discriminative terms and carry **different values of the same kind**.

A *value* is a typed literal — money, a duration, a percentage, a count — with
the value tokens removed from the terms that identify the slot, so the number
cannot be part of what makes two statements look different.

Two details that mattered more than the idea:

**Spelled-out numbers.** Six of thirty development-half misses were a count
stated in words: "three different ones" becoming "four", "twice a week"
becoming "three times". A value parser that reads only digits sees none of
them. Reading them lifted 22.2% → 27.8% overall.

**Word numerals are values, not terms.** Leaving "three" and "four" in the slot
was worse than useless — they counted as terms that *differ*, pushing two
statements apart exactly when they were the same fact.

## Why the thresholds are where they are

A missed update leaves two facts on file: recall returns both, the reader can
still resolve it, and a person can see it in the note. A false update strikes a
true fact through. The two errors are not comparable, so the rule requires an
**absolute** count of shared discriminative terms rather than only a ratio (a
ratio lets two three-word fragments collide), refuses to fire across value
kinds, and refuses when either statement carries a *range* rather than a value.

Years are excluded from the count kind outright. "I visited Rome in 2019" and
"…in 2021" are two events, not one fact updated, and treating them as an update
was the largest source of wrong supersessions in the first version.

`internal/memory/slots_test.go` is mostly the cases the rule must NOT fire on,
and was written before the benchmark ran.

## Prior art, stated plainly

mem0 and Zep detect contradictions too — with a model call per write. That
works, and it puts an LLM on the agent's hot path. This codebase already
refused that once, for entity extraction, on the grounds that *"a write that
waits on a model is one agents learn not to make"*. The same argument applies
here and the same answer follows.

So the contribution is not "contradiction detection". It is **contradiction
detection at zero marginal cost on the write path**, deterministic enough to
unit-test, measured against the LLM-free baseline it replaces. When a model
*is* configured the existing model path still runs and can catch cases no rule
will; this raises the floor, it does not replace the ceiling.

## What it costs

The whole argument for a deterministic rule is that it is free on the write
path, so here is the number rather than the adjective
(`go test ./internal/memory -bench .`, AMD Ryzen AI Max+ 395):

| | |
|---|---|
| one value-slot comparison | **48 µs** |
| a full `Decide` against 40 candidates | **1.18 ms** |

A model call on the write path is 200–2000 ms. The rule is three to four
orders of magnitude cheaper than the thing it is standing in for, and it is
dwarfed by the file write and reindex that follow it — which is what "zero
marginal cost" means here: the write is not measurably slower.

## What this does not show

- **Nothing about answer accuracy.** Recognising the update is necessary for a
  reader to see one value instead of two. It is not sufficient. The end-to-end
  arm is measured separately, with all the reader and judge variance this study
  avoids, in [REPORT-memory-arm.md](REPORT-memory-arm.md).
- **Nothing about facts whose value is not a literal.** A preference, a name, a
  place — the value grammar does not cover them, and they still depend on the
  older path. 72.2% of these pairs are still missed.
- **Nothing about English in general.** 72 pairs, one dataset, synthetic-but-
  realistic chat.
- The false-positive control is an **upper bound**: some unmarked pairs are
  genuinely updates the dataset had no reason to mark.

---

## Round two, 2026-08-24 — 20/72 → 28/72

Recognition was the ceiling on everything downstream: the authority lattice can
only protect a correction on a fact the engine recognises as changing, so at
20/72 it did nothing for three quarters of this set. This round is an error
analysis of the 52 misses and what came of it.

**Where the 52 went.** Per-fact instrumentation, not guessing: 17 were
extraction failures (the changed value never reached a fact at all) and 35 were
matching failures (the fact was extracted and no prior fact was connected to
it). Of those 35 — 4 blocked by the one-value guard, 11 by `SameSlot`, and 20
carried no parsed value at all.

| build | updates recognised | share |
|---|---|---|
| before | 20/72 | 27.8% |
| **after** | **28/72** | **38.9%** |

Measured end to end by `slot_probe.py` against a real server, not by the
harness that designed the change.

### A bug that was costing retrieval, not just reconciliation

`properRE` keeps the apostrophe inside a token, so `I've` never matched the
`i` already in `sentenceStart` and was extracted as an **entity**. Since almost
every conversational sentence opens with one, `i've`, `i'm` and `i'll`
accounted for **1,098 of 1,107** spurious entity matches across the corpus,
with `by` — from "By the way" — supplying most of the rest.

This was not confined to this experiment. `EntityOverlap` is the third
retrieval signal, so every fact beginning "I've" was scoring an entity hit
against every other one. Contractions are now split before the stoplist check
and the common discourse openers are in it.

### The null: entity overlap is not a slot anchor

The first hypothesis was that a **shared entity** would be a better anchor than
shared slot terms for conversational paraphrase. Measured, it looked like a
clear win: 20/72 → 29/72.

It was an artifact. With the entity bug fixed, the same rule scores **20/72 →
20/72** — every one of those nine catches had been made through `i've` matching
`i've`. The rule is not in the build. It is written down here because the
version of this report that did not run the second measurement would have
claimed a 45% improvement from a rule that does nothing.

### What actually worked

- **Disjoint multi-value.** The one-value guard refused any statement carrying
  more than one value of a kind, and people quote comparisons in the same
  breath — "120 stars, not 300", "a $325,000 house, pre-approved for $350,000".
  Disjoint sets are the safe half: unambiguous change. Overlapping sets are
  still refused, and so are ranges — `TestItRefusesWhenAStatementHasARangeRatherThanAValue`
  is an older invariant that outranks this rule, and honouring it cost one of
  the nine pairs.
- **Categorical updates.** A third of the misses changed a *name*, not a
  number: "moved to Chicago" → "moved to Denver". `parseValues` sees nothing in
  either sentence. The rule is a shared entity (the subject) plus an entity on
  each side the other lacks (the value that moved), gated by `SameSlot`.

### Precision

5 false positives in **49,240** cross-pair trials — **0.010%** — against the
~0.25% the existing value-slot rule was accepted at. The `SameSlot` gate is
what buys that: without it the categorical rule fires on any two sentences
sharing a name.

### What this does not show

The ceiling is still 39%. 15 misses are extraction failures and 20 carry no
parsed value and no entity pair, so a value-and-entity approach cannot reach
them at all — they need either a different representation or a model call, and
a model call on the write path is the thing this engine exists to avoid.
