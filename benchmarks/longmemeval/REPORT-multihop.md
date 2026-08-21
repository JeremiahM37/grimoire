# The path Grimoire answers with is not better than the one it benchmarks

Method: [PROTOCOL-multihop.md](PROTOCOL-multihop.md), registered before the
numbers were read. Raw output: `results_multihop/`. Reproduce with
`python3 run_multihop.py all --category multi-session`.

**Null result, and an expensive one.**

---

## The result

All 51 `multi-session` questions from the frozen sample. Same ingestion, same
reader, same judge, all three arms read in the same session.

| retrieval | correct | context chars (median) | latency |
|---|---|---|---|
| `plain` — one query | **25/51 (49.0%)** | 13,294 | **0.1 s** |
| `smart` — decompose + rerank, `qwen3.5:4b` | 24/51 (47.1%) | 14,340 | 6.9 s |
| `smart35` — the same with `qwen3.6:35b-a3b` | 23/51 (45.1%) | 14,099 | slower still |

```
smart   vs plain:  fixed 11, broke 12   p = 1.00
smart35 vs plain:  fixed  7, broke  9   p = 0.80
```

**Plain single-query retrieval is nominally the best of the three**, and
neither multi-hop arm is distinguishable from it. Neither is distinguishable
from *worse*, either — with 51 questions this study can see a large effect and
nothing else — but nothing here is evidence that decomposition helps.

## The ambiguity, resolved

The first run left two readings open: decomposition does not help on this
category, or `qwen3.5:4b` is too weak to decompose and rerank. Reshuffling
without improving is what a poor reranker looks like, so the second was
plausible and would have implied the opposite action — document a model
requirement rather than gate the feature.

**The stronger decomposer did not fix it.** `qwen3.6:35b-a3b` — 36B parameters
against 4B, the largest model on this host — churns less (16 changed answers
against 23) and lands *slightly lower*. Whatever is wrong is not the
decomposer's size.

The remaining explanation the data supports: on a corpus where **every session
is already its own note**, the sub-questions retrieve substantially the same
passages the whole question does, and the rerank that follows then has an
opportunity to drop a good one. Multi-hop retrieval is a mechanism for corpora
where the hops are genuinely disjoint; LongMemEval haystacks ingested this way
may simply not be that.

## What followed from it

`/api/ask` charged every question two model calls for this. Across two
decomposers, four orders of magnitude of retrieval latency, and 51 questions in
the category the mechanism exists for, **the only measurement of it shows no
benefit.** That is not proof it is useless everywhere — one category, one
corpus shape, n = 51 — but it inverts the burden.

**So the default was flipped: `/api/ask` no longer decomposes, and `smart:
true` opts in.** Three things pointed the same way and none of them alone would
have been enough:

1. Plain is nominally the most accurate of the three arms.
2. It costs a model call per question and ~70× the retrieval latency.
3. Every published number in `benchmarks/` was measured on the plain path, so
   while decomposition was the default **the benchmarks did not describe what a
   user got**. That one is not about accuracy at all, and it is the reason the
   change is right even if the true effect is a small positive.

Pinned by `TestAskDoesNotDecomposeUnlessAsked`, which counts model calls
against a stub rather than asserting the flag is accepted — "accepted" is also
what a regression that ignored it would look like. Measured: default 1 call,
`smart: true` 2.

The `smart=1` parameter stays regardless of that decision. The console's
*"what would the agent see"* has to inspect the ranking the agent actually
used, whichever ranking that turns out to be.

## Limitations

- **Two decomposers**, 4B and 36B. A frontier model was not tried; the gap
  between 4B and 36B producing no improvement makes "a bigger model fixes it"
  the less likely explanation, not an excluded one.
- The server here runs a local Ollama, which the published conditions did not.
  That is why `plain` was re-measured under the same configuration rather than
  read from the stored numbers — the comparison is internal.
- `k=10` for every arm. The smart path pools three sub-question retrievals and
  reranks back down to `k`, so it sees more candidates and returns the same
  number; a larger `k` might change the picture and was not tried.
- Latency is from a single timed question (0.1 s vs 6.9 s for the 4B), not a
  distribution. The 36B arm was slower again and was not separately timed.
- **n = 51 cannot rule out a small benefit.** What it rules out is a large one,
  and a default that costs two model calls per question needs more than
  "possibly slightly positive" behind it.
