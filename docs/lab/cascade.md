# `cascade`: a replacement gate for contested supersessions

Status: behind the `cascade` experiment flag (`GRIMOIRE_EXP=cascade`). Flag off,
the write path is unchanged. The `grimoire memory audit-supersessions` command is
not behind the flag. Verdict at the end.

## What it does

The write-time rules (`memory.DecideAs`) supersede an old fact when the new one
shares enough words and carries a value of the same kind (`SameSlot` +
`ValueUpdate`), or when they share a subject and predicate with a different
value (`Attribute`). Overlap cannot tell a modifier of a shared head noun
("billing postgres replica lags 2 seconds" / "analytics postgres replica lags
40 seconds") from a correction, so both true facts cannot coexist.

With the flag on:

1. `memory.Contested(a, b)` (`internal/memory/contest.go`): a pair is contested
   when each statement holds a discriminating word the other lacks. Values
   (digits, number words, clock times, units), measurement vocabulary and filler
   are not discriminating. Variant tokens such as `bf16`, `fp32`, `v2` are.
2. A supersession the rules would make of a contested candidate is deferred
   (`Decision.Contested`, `internal/memory/cascade_hook.go`). With no gate, the
   fact is kept alongside the old one, unmarked. A challenge was rejected: a
   challenged fact is hidden from default recall (`index/memory.go`,
   `AcceptedOnly`), so it would hide the new value.
3. `internal/cascade` (`Gate.Decide`): a local Laya head answers one choice
   question ("does B replace A?"). If its calibrated probability is at least `hi`
   it replaces, below `lo` it keeps, and inside the band it escalates to Jev.
   A stage that is off, errors or times out falls through; if nothing answers,
   the result is keep both. Nothing supersedes silently.
4. `reconcileFact` consults the gate for any supersession of a contested pair,
   whether the rules or the model proposed it (`cascadeReconcile`). Explicit
   corrections naming a target are not gated.

Settings: `cascade_laya_url`, `cascade_laya_model`, `cascade_laya_calibration`,
`cascade_jev_calibration`, `cascade_band` (env `GRIMOIRE_CASCADE_*`). Jev uses the
existing `decision_url`, `decision_model`, `decision_api_key`. Defaults: band
`0.05,0.65`, Jev calibration `1.876,-0.304`, Laya calibration identity. No Laya
head is shipped in the repo, so by default only Jev can decide.

Review: `GET /api/memory/supersessions` lists past supersessions with the gate's
verdict (`judged_distinct` only when a stage answered and said keep).
`POST /api/memory/supersessions/restore {id, path}` is refused to agents, takes a
snapshot (`mutateEntry`), clears `superseded_by`/`superseded_at`, and marks the
fact human (a person has endorsed it, so an agent cannot overwrite it silently).

## Measurement

All numbers are synthetic, from the deterministic generator
(`internal/supersede/gen.go`, seed 20261011, 150 pairs per category per split,
4050 in total). Templates and subject pools are disjoint across splits; no
statement appears in two splits (tested). Truth: `supersede` for true updates and
paraphrased updates; `keep` for everything else. n = 150 per category on test.
95% percentile bootstrap intervals in brackets.

Test split, rates per category. "false" = keep-truth pairs that were superseded;
"miss" = supersede-truth pairs that were not.

| category | (a) rules today | (b) + contested keep | (c) + Jev only | (d) + Laya only | (e) cascade |
|---|---|---|---|---|---|
| true_update, miss | 0.100 [0.053,0.153] | 0.200 [0.140,0.267] | 0.100 [0.053,0.153] | 0.167 [0.107,0.233] | 0.100 [0.053,0.153] |
| paraphrased_update, miss | 0.460 [0.387,0.540] | 1.000 [1.000,1.000] | 0.493 [0.413,0.573] | 1.000 [1.000,1.000] | 0.593 [0.520,0.667] |
| refinement, false | 0.667 [0.587,0.747] | 0.667 | 0.667 | 0.667 | 0.667 |
| same_text, false | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |
| modifier_collision, false | 1.000 [1.000,1.000] | 0.000 | 0.000 | 0.007 [0.000,0.020] | 0.007 [0.000,0.020] |
| variant_collision, false | 1.000 [1.000,1.000] | 0.000 | 0.000 | 0.000 | 0.000 |
| sibling_attribute, false | 0.500 [0.413,0.580] | 0.000 | 0.000 | 0.000 | 0.000 |
| unrelated_vocab, false | 1.000 [1.000,1.000] | 0.000 | 0.000 | 0.000 | 0.000 |
| list_edge, false | 0.000 | 0.000 | 0.000 | 0.000 | 0.000 |

Gate use on the test split (621 contested pairs): (e) decided 423 with the Laya
head (68%) and escalated 198 to Jev (32%). The band was chosen on the calibration
split (560 contested pairs): the uncertain band `[0.05, 0.65)` is the one with no
Laya error outside it, and it escalated 203 of 560 (36%) there.

Gate models, on the gate population (contested pairs; the classes are heavily
imbalanced: 34 positive of 560 calibration, 96 of 621 test):

| model | split | acc | log loss | Brier | ECE | AUC |
|---|---|---|---|---|---|---|
| Jev, calibrated | calib | 1.000 | 0.001 | 0.000 | 0.001 | 1.000 |
| Jev, calibrated | test | 0.992 | 0.017 | 0.005 | 0.012 | 1.000 |
| Laya head, calibrated | calib | 0.930 | 0.187 | 0.056 | 0.046 | 0.891 |
| Laya head, calibrated | test | 0.852 | 0.399 | 0.122 | 0.105 | 0.874 |

Jev's errors on test are all paraphrased updates (acc 0.938 on 81 positives).
The Laya head gets 0 of 81 test paraphrases right, which is why (d) misses them
all: it was trained on the train split's paraphrase frames (35 positives), and
the test split uses different frames. The reliability table is in
`results-score-jev.txt` and `results-score-laya.txt` (lab work directory).

Decision latency (sequential single calls, test gate pairs): Laya on this box
p50 255 ms, p90 355 ms (n=100, while the grimoire-laya service also ran). Jev
p50 99 ms, p90 150 ms (n=30). The batch labelling run was not timed per call.

Rules table for the train and calibration splits: `results-rules.txt` in the lab
work directory. The pattern is the same there, with (a) false 1.000 on
modifier/variant/unrelated and paraphrase misses 0.70 (train) and 0.49 (calib).

### Private real-pair check (counts only)

12 hand-labelled live pairs (8 replace, 4 distinct; 2 unclear excluded).
Current rules, flag off: they supersede 3 of the 8 replace pairs, none of the
4 distinct pairs. The 3 that the rules supersede are not contested, so no gate
variant touches them. Counterfactual: the 9 pairs that are contested in the
sense of `memory.Contested` (5 replace, 4 distinct) were put to the gate as if a
model had proposed their supersession:

| variant | distinct kept (false supersessions prevented) | replace kept (true updates wrongly blocked) |
|---|---|---|
| (c) Jev only | 3 of 4 | 2 of 5 |
| (d) Laya only | 4 of 4 | 5 of 5 |
| (e) cascade | 3 of 4 | 2 of 5 |

These are 12 pairs. The result is a count, not a rate with a useful interval.
The 4 distinct pairs in the live vault did not come from the rules as they now
stand; the earlier path that wrote them is not reproduced here.

## Budget and accounting

- Jev: 1785 of 2500 calls, all in `/mnt/bulk/lab-work/cascade/jev-ledger.jsonl`,
  each appended before the call. Breakdown: 1 smoke call, 1745 for the gate
  population (4050 pairs, 1746 contested), 9 for the contested private pairs,
  30 latency calls. Test-time escalations reused the cached answers and made no
  new calls.
- Laya: 2 training runs. The first (`laya-train`, frozen encoder, base
  `convaiinnovations/laya`, 6 epochs) hit the 590 s command timeout during epoch 6
  and saved nothing. The second, in the background, is the head used here.
- Jev-labelled head: not trained. On the train split the Jev labels equal the
  generator labels on all 565 pairs, so the training data would be identical, and
  a third run was outside the budget. The `laya-data` subcommand writes both
  label sets, so the comparison can be run later.

## Reproduce

```sh
cd go
go build -o /tmp/cascadelab ./internal/supersede/cmd/cascadelab   # or any scratch dir
cascadelab gen -seed 20261011 -n 150 -out DATA/pairs.jsonl
cascadelab rules -in DATA/pairs.jsonl
cascadelab population -in DATA/pairs.jsonl -out DATA/gate_pop.jsonl
JEV_API_KEY=... cascadelab jev -in DATA/gate_pop.jsonl -cache DATA/jev_gate.jsonl -ledger DATA/jev-ledger.jsonl
cascadelab laya-data -in DATA/gate_pop.jsonl -cache DATA/jev_gate.jsonl -out-truth ... -out-jev ...
cascadelab jev -base http://127.0.0.1:19800 -model cascade -in DATA/gate_pop.jsonl -cache DATA/laya_gate.jsonl
cascadelab score -in DATA/gate_pop.jsonl -cache DATA/jev_gate.jsonl
cascadelab cascade -in DATA/pairs.jsonl -laya DATA/laya_gate.jsonl -jev DATA/jev_gate.jsonl
```

`DATA` is a directory outside the repo. The real pairs were never written into
the repo or any commit. The `realverdicts` and `realgate` subcommands print
counts only. `realgate` has the calibrations hard-coded from the calibration
split; nothing was tuned on the private pairs.

Tests (offline, `env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP`):
`internal/memory` (contest, flag on/off), `internal/cascade` (gate with httptest
fakes: no server, band, failures), `internal/supersede` (generator determinism,
split disjointness, labels), `internal/api` (`TestFlagOff*`, `TestFlagOn*`,
`TestSupersessionReviewListsAndRestores`, route audit, leakprobe, full package).

## Limits and open items

- The synthetic gate population is easy. Jev is near perfect on it, so these
  numbers say little about how Jev does on real ambiguous pairs. The real-pair
  check has 12 pairs.
- Refinement (one statement adds detail to the other) is not contested and is
  not touched. Its false-supersession rate stays at 0.67 on the test split in
  every arm, because the rules still supersede it. This is the largest residual
  and the cascade does not address it.
- Paraphrased updates are where the cascade costs something. Keep-both alone
  raises paraphrase misses from 0.46 to 1.00 (arm b). Laya's confident
  "keep" on unseen paraphrase wording is the main reason the cascade (0.59)
  is worse than Jev alone (0.49).
- The Laya head is weak on wording it did not see in training, and it was trained
  on 35 positives. A larger, more varied train set is the obvious next step; it
  was not done because of the training budget.
- Gate population: 1746 contested pairs over 4050 generated pairs. Some pairs are
  duplicated (3851 unique statement pairs), and Jev answers are not bit-stable
  (repeat calls differ at the third decimal), so the cascade's calibration
  constant was taken from the run that is reported.
- The model path in `reconcileFact` is gated in the code but cannot be measured
  here, because the lab has no LLM backend.
- The restore marks the fact human. That is a policy choice; it can be argued
  either way.

## Verdict

KEEP BEHIND FLAG. On the synthetic test split the cascade removes the
false-supersession defect for modifier, variant, sibling and unrelated collisions
(under the rules, 0.50 to 1.00; under the cascade, 0.00 to 0.007), at a cost in
paraphrased-update misses (0.46 under the rules, 0.59 under the cascade). Refinement is untouched. The private check (12 pairs) is too
small to act on and shows the same trade. It should not graduate until a
larger and more varied labelled set exists and the refinement case has a
measured answer.
