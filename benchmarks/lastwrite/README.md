# Last-write floor for knowledge-update recognition

A zero-learning "newest statement wins" resolver, scored on the same items as
Grimoire's knowledge-update numbers, so those numbers have a floor to be
compared against.

## What it measures, and why

Grimoire reports how often its write-time reconciliation recognises that a later
statement supersedes an earlier one (`benchmarks/longmemeval/REPORT-slots.md`:
26/72 overall and 17/37 held-out in the best row, 28/72 in the `both` row, with
3 to 5 false positives in 400 unmarked pairs). Nothing in the repo says what a
trivial policy scores on the same pairs. This harness supplies that floor.

The resolver keeps, for each (subject, attribute), the latest dated statement
and nothing else. It learns nothing and calls no model.

A related paper reports a zero-learning last-write resolver at 80.25% on the
conflict split of MemoryAgentBench (arXiv 2610.09193). That number is cited from
the paper and was not re-checked here. It is on a different benchmark and is not
directly comparable to the LongMemEval figures.

## Two questions

- `recognise`: for each evidence pair (earlier statement, later statement) from
  the 72 non-abstention knowledge-update items, does the resolver call the later
  one an UPDATE of the earlier? Also run on 400 unmarked same-haystack pairs,
  where every UPDATE is a false positive. Same pair construction, dev/held-out
  split and control seed (20260821) as `slot_probe.py` and `slot_falsepos.py`.
- `answer`: for each question, answer from the latest statement that fills the
  question's slot, then score against the gold answer.

## Cue ablation

`--ablate-cues` strips `now`, `changed`, `moved`, `switched`, `updated`,
`no longer` and `instead` from every statement before the slot table is built.
If the floor's score falls when cues are removed, it was reading the update
word and not the slot. The selftest shows one case where this happens: "My
commute is 45 minutes" followed by "My commute is now 45 minutes" is an UPDATE
when cues are kept and a NOOP when they are removed.

## Method

Slot extraction is a regex, and it is the whole method:

- Subject: `my`/`our X is Y` gives `user`; `the X is Y` gives `the`.
- Attribute: the last two content words before the copula, with determiners and
  time adverbs dropped.
- Value: text after the copula, cut at the first comma, semicolon or clause
  connector, at most eight tokens.

`recognise` outcome per pair: UPDATE (same slot, different value), NOOP (same
slot, same value), ADD (no shared slot, so both stay on file).

`answer` scoring is a deterministic token-containment match, not the LongMemEval
judge (an LLM, so not deterministic). Correct if every content token of the gold
answer appears in the predicted value. This is lenient ("not Chicago" matches
"Chicago"), which only raises the floor. Questions with no slot abstain and count
as wrong.

Every score is printed with **slot coverage**: how many pair sides or questions
got a slot at all. A low score with low coverage means the extractor is the
bottleneck, and the floor makes no claim beyond that.

## Data

LongMemEval `longmemeval_s`, from
https://huggingface.co/datasets/xiaowu0162/longmemeval. Set `LME_DATA` to the
JSON file (default `/tmp/lme_dl/longmemeval_s.json`, the path `slot_probe.py`
uses). Not committed. If the file is absent, the subcommands say so and exit 2.

The held-out bucket is `sha256(qid) % 2 == 1`. `PROTOCOL-slots.md` does not name
the held-out bucket, so this is an assumption. The output prints both bucket
sizes, and they should read 35 and 37 for the report's split. If they come out
the other way round, flip the mapping in `held_out_bucket`.

## How to run

```
python3 benchmarks/lastwrite/lastwrite.py --selftest           # synthetic, no data needed
LME_DATA=/path/longmemeval_s.json python3 benchmarks/lastwrite/lastwrite.py recognise
LME_DATA=/path/longmemeval_s.json python3 benchmarks/lastwrite/lastwrite.py recognise --ablate-cues
LME_DATA=/path/longmemeval_s.json python3 benchmarks/lastwrite/lastwrite.py answer
LME_DATA=/path/longmemeval_s.json python3 benchmarks/lastwrite/lastwrite.py answer --ablate-cues
```

JSON goes to `results/` unless `--out` is given. No network, no model, no
dependencies beyond the standard library.

## How to read the result

- The Grimoire figure is only interesting relative to the floor. If the floor
  recognises a similar share of the 72 marked pairs, the engine's write-time
  machinery is not adding much beyond "same slot, newer date". If the engine is
  clearly above the floor on held-out, the extra is real. Check it against the
  false-positive count: a floor with near-zero false positives and modest recall
  is a fair comparison point.
- A large drop with `--ablate-cues` means the score depends on lexical update
  markers. The engine has no such dependence by design, so a gap there is a
  measurement of what the floor is cheating on.
- A low score here is not evidence the engine is good. It is evidence the slot
  regex is narrow. Read coverage first.

## Limits

- Slot extraction is regex over one statement at a time. Statements that state a
  fact without `my`/`the X is` (for example "I moved to Denver") get no slot and
  fall into ADD. Categorical updates like that are the case the engine's
  categorical rule was added for, and this floor cannot see them.
- The answer scorer is lenient and not the LongMemEval judge. Use the numbers as
  a floor for comparison, not as a LongMemEval accuracy.
- The 80.25% figure is from a different benchmark with a different scorer.
