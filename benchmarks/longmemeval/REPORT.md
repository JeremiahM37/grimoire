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

\* median reader input tokens minus the `none` baseline (CLI overhead).

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
