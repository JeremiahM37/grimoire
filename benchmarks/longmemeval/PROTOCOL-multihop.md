# Does the retrieval path Grimoire answers with beat the one it benchmarks?

Registered before the numbers were read. Run in progress; see
[REPORT-multihop.md](REPORT-multihop.md) for results as they land.

## The gap this exists to close

`/api/ask` does not retrieve the way `/api/retrieve` does. It calls
`smartRetrieve`: decompose the question into up to three sub-questions,
retrieve for each, dedupe the pool, rerank it. Plain `/api/retrieve` ranks one
query and returns it.

**Every number in [REPORT.md](REPORT.md) was built from plain
`/api/retrieve`.** The path the product actually answers with has never been
benchmarked, and until v2.5 could not be called on its own at all — which also
meant the console's *"what would the agent see"* showed a different ranking
from the one the agent answering that question saw.

## Hypothesis

> On `multi-session` questions, the multi-hop path retrieves better context
> than single-query retrieval.

`multi-session` because it is the category defined by needing facts from more
than one session — decomposition's entire purpose — and because at 56.9% it is
the worst of the five fact-shaped categories, with 51 questions in the frozen
sample.

Directional. The mechanism was built for this case; a two-sided test would be
pretending otherwise.

## Conditions

All 51 `multi-session` questions from the **frozen 200-question sample**, so
every number lines up with a published one. Haystacks ingested as session notes
exactly as every published condition ingests them, so the only thing that
differs is retrieval.

| arm | retrieval |
|---|---|
| `plain` | `/api/retrieve?k=10` — what every published number used |
| `smart` | `/api/retrieve?k=10&smart=1` with `qwen3.5:4b` |
| `smart35` | the same with `qwen3.6:35b-a3b` |

`smart35` was **added after `smart` returned a null**, to separate "the
mechanism does not help" from "the decomposer is too weak" — the two readings
imply opposite actions. That makes it a follow-up rather than a pre-registered
arm, and it is labelled as one. Only the smart arms depend on the model, so
`plain` is unchanged and all three were re-read in one session.

Same reader (`claude-haiku-4-5`), same judge (`claude-sonnet-5`), same prompts
as `run_lme.py`. **Both arms read in the same session**, because re-reading a
byte-identical context flips 8–12% of answers.

`plain` is re-measured here rather than taken from the stored numbers, for one
reason worth naming: the server in this run has a local Ollama configured
(`qwen3.5:4b`), because `Decompose` and `Rerank` need a model and without one
the smart path *is* the plain path. That is the one configuration difference
from the published run, and re-measuring `plain` under it keeps the comparison
internal.

## Analysis, fixed in advance

- **Primary:** exact one-sided McNemar on discordant pairs, α = 0.05.
- **Reported regardless:** both accuracies, the discordant split, and median
  context characters per arm.
- **Cost is part of the result, not a footnote.** Measured on one question:
  plain retrieval 0.1 s, smart 6.9 s — **70× slower**, because decomposition
  and reranking are two model calls per query. A two-point gain at 70× the
  latency is a different recommendation from a ten-point gain, and the report
  must state the trade rather than the accuracy alone.
- One category. No subgroup analysis.

## Declared outcomes

- **Wins:** the shipped answering path is better than the benchmarked one, the
  published numbers understate the product on this category, and the cost is
  quoted beside the gain.
- **No difference:** decomposition is not earning its two model calls on this
  category, which is worth knowing and worth saying — and the honest follow-up
  is whether `/api/ask` should spend them.
- **Loses:** report it. A rerank that discards the right passage is a real
  failure mode and this is the measurement that would find it.

## What this cannot show

- Nothing about the other five categories.
- Nothing about a *frontier* decomposer. Two were run — `qwen3.5:4b` and
  `qwen3.6:35b-a3b`, added after the first arm returned a null that a weak
  decomposer could equally have explained. Both are local models; a hosted one
  is untested.
- Nothing about `/api/ask` end to end, which also reranks into an answer and
  applies the fitted-vs-terse prompt question separately
  ([REPORT-preference.md](REPORT-preference.md)).
