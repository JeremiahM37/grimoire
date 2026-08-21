# Recognising an update nobody phrased like a database write — protocol

## What is being measured, and why it is not the usual thing

Every published memory-system number is *accuracy on questions*: retrieve,
read, judge. That measurement has a floor — this project measured it at an
8–12% answer flip rate from reader and judge sampling on byte-identical input —
and it conflates four systems (extraction, storage, retrieval, reader) into one
score.

This measures a **mechanism**, directly, with no reader and no judge in it:

> Given the earlier statement of a fact and then the later one, does write-time
> reconciliation recognise the second as superseding the first?

It has **zero sampling variance**. The same two statements produce the same
operation forever, so a difference between two builds is a difference in the
code and nothing else.

## The data, and why it is fair

LongMemEval's `knowledge-update` questions are constructed from exactly this
shape: a user states a fact in one session and restates it with a different
value in a later one. The dataset marks both — `answer_session_ids` names the
sessions, `has_answer` marks the turns.

- **All 72 evidence pairs** from all 72 non-abstention `knowledge-update`
  entries in `longmemeval_s`. Not a sample: nothing here costs a model call, so
  there is no reason to take one.
- Turns are used **verbatim**, as the user wrote them, in date order. Only
  `has_answer` turns are paired: using whole sessions would measure needle-
  finding inside a session, which is a different question.
- Each pair is written into **its own topic with `scope: topic`**. Without it,
  reconciliation is vault-wide and pair 300 can be superseded by pair 12 — the
  first version of this harness did that, and both its numbers were
  contamination. The topic id is a counter, not a hash of the text, because
  Python's string hash is per-process randomized and two pairs could collide.

## The two numbers, and why one alone is worthless

**Recall** — of the 72 marked updates, how many produce `UPDATE`/`DELETE`.
A rule that superseded everything would score 100%.

**False positives** — of 400 pairs drawn from the SAME haystacks that the
dataset does *not* mark as evidence for each other, how many produce
`UPDATE`/`DELETE`. Every one of those is a true fact struck through.
Deterministic sample: seed 20260821, same pairs for every build.

The control is an **upper bound**, not a clean negative set: a haystack is one
person's life, and two unmarked turns can genuinely update each other — the
dataset only marks what its own question needs. So the honest reading is "at
most this bad".

**The asymmetry sets the bars.** A missed update leaves two facts on file:
recall still returns both, the reader can resolve it, and a person can see it.
A false update strikes a true fact through. So recall may be low and must
improve; false positives must stay near zero.

## Builds compared

| build | write-time rules |
|---|---|
| `rules-only` | the shipped `Attribute` path: SUBJECT PREDICATE VALUE, plus the similarity and negation fallbacks |
| `slots+words` | the same, plus `ValueUpdate`: same discriminative terms, different value of the same kind, including values spelled as words |

Same binary tree, same everything else.

## Development split, fixed before any tuning

The 72 pairs are split by `sha256(qid) % 2` into a **dev** half (35) and a
**held-out** half (37). The split was written down and computed before the
first threshold was chosen.

- Misses were read, and thresholds and the value grammar changed, **on dev
  only**.
- The held-out half was scored once per build and never inspected.

This is stated because the alternative — tuning on all 72 and reporting all
72 — would produce a number that means nothing, and the temptation to do it is
strong when the measurement is this cheap to re-run.

## Declared in advance

- **Primary:** recall on the **held-out** half. Dev is reported beside it for
  honesty about the gap, not as the result.
- **Gate:** the change is only worth shipping if false positives rise by less
  than **1 percentage point** over the 400-pair control. A mechanism that
  catches more updates by superseding more things has not improved anything.
- The unit tests in `internal/memory/slots_test.go` — chiefly the cases the
  rule must NOT fire on — are part of the claim, and were written before the
  benchmark was run.

## What this cannot show

- **Nothing about answer accuracy.** Recognising the update is necessary for
  the reader to see one value instead of two; it is not sufficient for a right
  answer. The end-to-end arm (`run_memory_arm.py`) measures that separately and
  has all the variance this study avoids.
- **Nothing about English in general.** These are 72 pairs from one dataset of
  synthetic-but-realistic chat. The value grammar covers money, durations,
  percentages, counts and spelled-out small numbers; a fact whose value is a
  name, a place or a preference is not covered by it at all and still depends
  on the older path.
- **Nothing about a model-assisted engine.** With an LLM configured the
  decision goes through `DecideMemoryFrom`, which can catch cases no rule will.
  This study is about what happens with no model on the write path — which is
  the default, and the configuration an agent writing on its hot path actually
  wants.
