# LongMemEval results

Method: [PROTOCOL.md](PROTOCOL.md); shared machinery and integrity rules
with the LoCoMo study (`../locomo/`). 200 stratified questions from
`longmemeval_s` (each with its own ~50-session, ~115k-token haystack),
reader `claude-haiku-4-5`, strict blind judge `claude-sonnet-5` (v2). Run
on 2026-07-20 with the round-5 product code — no product changes were made
for or after this run; it is a single-shot validation on a second dataset.

| condition | knowledge-update | multi-session | ss-assistant | ss-preference | ss-user | temporal | **overall** | context tokens* |
|---|---|---|---|---|---|---|---|---|
| none | 3.2% | 0.0% | 20.8% | 7.7% | 7.4% | 7.4% | **6.5%** | 0 |
| grimoire + model2vec | 83.9% | 60.8% | 87.5% | 30.8% | 88.9% | 81.5% | **75.0%** | ~5.9k |
| grimoire + nomic-embed | 77.4% | 60.8% | 95.8% | 15.4% | 85.2% | 79.6% | **73.0%** | ~5.8k |
| full context (~117k tokens) | 80.6% | 54.9% | 100.0% | 30.8% | 85.2% | 68.5% | **70.5%** | ~117k |
| grimoire + model2vec (Go) | 80.6% | 56.9% | 87.5% | 30.8% | 88.9% | 85.2% | **74.5%** | ~6.8k |

\* median reader input tokens minus the `none` baseline (CLI overhead).

The Go row was added on 2026-08-14, when the implementation was ported;
contexts came from a running Go server over HTTP (`retrieve_go.py`), with
the reader and judge phases unchanged and shared. Go vs Python on the same
embedder: 6 wins / 7 losses, exact McNemar p = 1.00 — indistinguishable.

## Reading the numbers

- **Retrieval matches — and directionally exceeds — full context at ~20×
  fewer tokens**: grimoire + model2vec 75.0% vs full 70.5% (30 wins /
  21 losses among discordant pairs, exact McNemar p = 0.26, n = 200). The
  defensible claim is parity at massive compression; the 4.5-point lead is
  a trend, not significance at this sample size.
- The gap pattern inverts vs LoCoMo: here the haystack is ~117k tokens, and
  the reader's needle-finding visibly degrades — full context loses to
  focused retrieval on temporal reasoning (68.5% vs 81.5%) and
  multi-session questions (54.9% vs 60.8%), the two types that require
  connecting facts across a huge transcript. LoCoMo's conversations
  (~24k tokens) fit comfortably, so full context stayed ahead there.
- The two embedders are statistically indistinguishable (p = 0.52); the
  30 MB pip-installable model2vec config needs no external service.
- `none` at 6.5% confirms the questions aren't guessable.
- Single-session-preference is poor in every condition (n = 13; gold
  answers are rubric-like statements the strict judge grades harshly) —
  also the hardest type in the LongMemEval paper.
- Caveat: our judge is not the official LongMemEval judge, so these
  numbers are not comparable to the paper's leaderboard; comparisons
  between the conditions in this table are like-for-like.

## Round 6 — per-note cap and chunk size: both null, and a measurement problem (2026-08-18)

Two candidate product changes were scored here, and the round's main result is
neither of them: it is that **this harness cannot resolve differences of the
size being tested**, and that every previously published cross-round
comparison shares the flaw.

### The noise floor

`control-sameepoch` is a byte-for-byte copy of the `grimoire-go` contexts,
re-read and re-judged in the same session as the new conditions. Identical
input, same frozen reader and judge:

| condition | overall | note |
|---|---|---|
| grimoire-go (round 5 reads) | 74.5% | original run |
| control-sameepoch | 73.4% | **identical contexts**, re-read |

8.0% of answers (16 of 200) flipped on identical input. On LoCoMo the same
control flipped 12.0% (60 of 500) and moved accuracy 3.6 points — enough that
McNemar called it significant (p = 0.027) against nothing but sampling.

Rounds 1–5 compared new reads against *stored* reads from earlier runs, so
every cross-round delta in this file carries that variance on top of whatever
the change did. It does not invalidate the large effects (retrieval vs `none`,
retrieval vs `full`), which are far bigger than the floor; it does mean a
2–4 point difference between two retrieval variants is not measurable at
n = 200 without a same-epoch control, and now there is one.

### The two candidates, measured against that control

| condition | knowledge-update | multi-session | ss-assistant | ss-preference | ss-user | temporal | **overall** | context tokens* |
|---|---|---|---|---|---|---|---|---|
| none | 3.2% | 0.0% | 20.8% | 7.7% | 7.4% | 7.4% | **6.5%** | 0 |
| full context | 80.6% | 54.9% | 100.0% | 30.8% | 85.2% | 68.5% | **70.5%** | ~117k |
| control-sameepoch | 80.6% | 56.9% | 87.5% | 23.1% | 88.9% | 81.5% | **73.4%** | ~6.8k |
| per-note cap | 80.6% | 60.8% | 95.8% | 30.8% | 92.6% | 85.2% | **77.0%** | ~6.7k |
| chunk 800→400 | 83.9% | 64.7% | 83.3% | 23.1% | 96.3% | 83.3% | **76.5%** | **~5.6k** |

\* median reader input tokens minus the `none` baseline.

- **per-note cap**: +3.6pp, 14 wins / 7 losses, **p = 0.189** — not significant.
- **chunk 800→400**: +3.1pp, 16 wins / 10 losses, **p = 0.327** — not
  significant, at **19% less context**.

Both point the right way here. Neither survives the other dataset: on LoCoMo
the cap scored −2.0pp (p = 0.203) and chunk-400 −1.2pp (p = 0.532) against
their same-epoch control, with the cap costing 7.6 points of multi-hop. **Both
were reverted; neither ships.**

### What does hold up

The diagnosis behind the cap is solid and independent of the judge, because it
was measured with no LLM in the loop on a held-out dev split:

- Evidence coverage was flat from rank 10 to 30 (54.2% → 55.7%) and then
  jumped to 92.4% by rank 75.
- **100%** of evidence chunks ranked at or beyond 30 were a later chunk of a
  note whose first chunk had already appeared, and the first such chunk arrived
  at mean rank 49.5 against 49.6 sessions per haystack.
- So per-note de-duplication, not relevance, set the recall floor: a fact
  anywhere but its note's single best-matching chunk was unreachable at
  ordinary k.

Removing that floor raised held-out turn-full coverage 75.6% → 83.3% at equal
context. It simply does not convert into measurable accuracy, and on LoCoMo the
same mechanism actively hurts — the extra same-note chunks that recover a
buried fact also drag in that session's other topics, and its list-answer
questions are graded on precision. The two datasets want opposite things from
a fixed constant.

### Consequence for future rounds

Retrieval changes should be gated on the deterministic dev metrics
(`dev_recall.py`, `../locomo/dev_coverage.py`) — evidence-turn coverage,
full-coverage rate, breadth, and context size — because those have no judge
variance. Where an end-to-end number is wanted, the baseline must be re-read
in the same session as the candidate, as `control-sameepoch` now is. Anything
smaller than roughly 5 points at these sample sizes should be reported as
unresolved rather than as a gain.
