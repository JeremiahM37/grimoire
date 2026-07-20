# LoCoMo results

Method: [PROTOCOL.md](PROTOCOL.md) (pre-registered; amendments noted there).
Raw per-question outputs: `results/round{1,2,3}/` (`results/` top level =
round 3 = current code). All numbers below share one judge (v2, strict) and
one reader (`claude-haiku-4-5`). Run on 2026-07-19.

Rounds share the frozen 500-question sample and all prompts. Rounds 1→2
differ only in Grimoire product code; round 3 additionally changes one
disclosed ingestion detail (dated note titles — see PROTOCOL.md). `none` and
`full` do not touch Grimoire code, so their reader answers are shared across
rounds.

## Round 1 — Grimoire as shipped (v1.0.0)

| condition | multi-hop | temporal | open-domain | single-hop | **overall** | context tokens* |
|---|---|---|---|---|---|---|
| none | 1.1% | 0.0% | 6.5% | 1.1% | **1.2%** | 0 |
| grimoire (offline default) | 44.6% | 26.0% | 32.3% | 68.1% | **52.8%** | ~8.0k |
| grimoire + nomic-embed | 51.1% | 26.0% | 38.7% | 67.4% | **54.0%** | ~7.2k |
| full context | 71.7% | 72.1% | 58.1% | 92.3% | **82.2%** | ~24.0k |

\* median reader input tokens minus the `none` baseline (CLI system-prompt
overhead), i.e. tokens actually spent on retrieved/system-provided context.

## Round 2 — after four generic retrieval fixes

Changes (all product code, none benchmark-aware; each has a unit test):

1. `chunk_text` splits blank-line-free text (transcripts, logs) on line /
   sentence boundaries instead of emitting one giant chunk.
2. `index.retrieve` is hybrid: embedding cosine fused (reciprocal-rank)
   with IDF-weighted lexical overlap, so rare discriminative words count.
3. `/api/search` falls back to any-term matching when a natural-language
   query matches no note with all terms.
4. `/api/search?full=true` returns the query-relevant excerpt of long
   notes, not whole bodies.

Fixes were tuned only against the 1,037 held-out dev questions
(`dev_recall.py`, evidence-turn recall — zero LLM calls), never against
the scored sample.

| condition | multi-hop | temporal | open-domain | single-hop | **overall** | context tokens* |
|---|---|---|---|---|---|---|
| none | 1.1% | 0.0% | 6.5% | 1.1% | **1.2%** | 0 |
| grimoire (offline default) | 51.1% | 59.6% | 58.1% | 86.1% | **72.4%** | ~5.2k |
| grimoire + nomic-embed | 59.8% | 60.6% | 58.1% | 90.8% | **76.8%** | ~5.1k |
| full context | 71.7% | 72.1% | 58.1% | 92.3% | **82.2%** | ~24.0k |

## Dev-split retrieval recall (diagnostic, not scored)

Mean fraction of gold evidence turns present in the retrieved context,
on the 1,037 dev questions:

| config | hashing embedder | ollama (nomic-embed-text) | median context |
|---|---|---|---|
| as shipped | 63.7% | — | 31.7k chars |
| + chunker fix only | 45.4% | — | 7.3k chars |
| + hybrid retrieval | 61.9% | 75.6% | 7.5k chars |
| + OR fallback & excerpts (round 2) | 82.9% | 87.8% | ~19.7k chars |
| + BM25 & small-to-big (round 4) | **85.8%** | **90.0%** | ~23.0k chars |
| round-4 code, model2vec embedder | 89.5% | — | ~23.1k chars |

The as-shipped number is high only because broken chunking stuffed whole
sessions into context; round 2 beats it with ~40% less context.

## Round 3 — dated note titles (disclosed ingestion change)

One change vs round 2, and it is not product code: session notes are titled
with their session date, the way a person titles a meeting log. In rounds
1–2 the date appeared only in the note body, where chunking can separate it
from the turns it applies to.

| condition | multi-hop | temporal | open-domain | single-hop | **overall** | context tokens* |
|---|---|---|---|---|---|---|
| none | 1.1% | 0.0% | 6.5% | 1.1% | **1.2%** | 0 |
| grimoire (offline default) | 50.0% | 68.3% | 58.1% | 87.9% | **75.0%** | ~5.4k |
| grimoire + nomic-embed | 64.1% | 76.0% | 67.7% | 89.7% | **80.8%** | ~5.4k |
| full context | 71.7% | 72.1% | 58.1% | 92.3% | **82.2%** | ~24.0k |

## Round 4 — BM25 lexical leg + small-to-big retrieval

Product code only; ingestion identical to round 3. Two changes survived the
dev gate: Okapi BM25 (term-frequency saturation + length normalization)
replacing binary set-overlap in the lexical leg, and small-to-big retrieval
(chunks are ranked small, but the top hits return with their neighbouring
chunks merged, so answers straddling a chunk boundary stay whole). Chunk
token counts are also LRU-cached, so repeated queries no longer re-tokenize
the vault. Three candidates failed the dev gate and were reverted:
pseudo-relevance feedback, a bigram/sublinear-tf hashing embedder, and
cosine-leg query expansion.

| condition | multi-hop | temporal | open-domain | single-hop | **overall** | context tokens* |
|---|---|---|---|---|---|---|
| none | 1.1% | 0.0% | 6.5% | 1.1% | **1.2%** | 0 |
| grimoire (offline default) | 53.3% | 71.2% | 54.8% | 89.4% | **76.8%** | ~6.2k |
| grimoire + nomic-embed | 60.9% | 78.8% | 61.3% | 91.9% | **81.6%** | ~6.2k |
| full context | 71.7% | 72.1% | 58.1% | 92.3% | **82.2%** | ~24.0k |

## Round 5 — optional local embedding model (`pip install model2vec`)

Round 4 left one measurable gap: the zero-dependency hashing embedder is
paraphrase-blind (offline vs full-context p = 0.009). Round 5 adds an
**optional** local semantic embedder — install `model2vec` and Grimoire
auto-detects it (static embeddings, numpy-only, ~30 MB model, no external
service; the index re-embeds automatically when the backend changes). New
condition `grimoire-local`; every other condition carried forward unchanged.

| condition | multi-hop | temporal | open-domain | single-hop | **overall** | context tokens* |
|---|---|---|---|---|---|---|
| grimoire (offline default) | 53.3% | 71.2% | 54.8% | 89.4% | **76.8%** | ~6.2k |
| grimoire + model2vec (pip extra) | 56.5% | 79.8% | 61.3% | 91.6% | **80.8%** | ~6.1k |
| grimoire + nomic-embed | 60.9% | 78.8% | 61.3% | 91.9% | **81.6%** | ~6.2k |
| full context | 71.7% | 72.1% | 58.1% | 92.3% | **82.2%** | ~24.0k |

`grimoire-local` is significantly better than the hashing default
(48 wins / 28 losses, McNemar p = 0.029) and statistically
indistinguishable from full context (37/44, p = 0.51) — the fully-local,
no-external-service config now sits at the ceiling too.

## Reading the numbers

- **Round 4 grimoire + nomic-embed (81.6%) is statistically
  indistinguishable from the full-context ceiling (82.2%)**: among the 79
  questions where exactly one of the two was correct, retrieval won 38 and
  full context won 41 (exact McNemar p = 0.82, n = 500) — while using
  ~3.9x fewer context tokens per question (~6.2k vs ~24k). The offline
  default (76.8%) remains measurably below ceiling (p = 0.009): the gap
  that's left is the hashing embedder's paraphrase blindness.
- The scored round 3→4 delta (+1.8 offline / +0.8 nomic) is individually
  inside sampling noise (paired p = 0.69); the evidence that round 4's
  changes help is the dev split, where recall rose 82.9→85.8 (hashing) and
  87.8→90.0 (nomic) on n = 1,037 — the scored movement is directionally
  consistent with that.
- The `none` floor (1.2%) shows the questions are not guessable; the reader
  only knows what the condition supplies.
- Attribution by round: 1→2 (+19.6 / +22.8) the four generic retrieval
  fixes; 2→3 (+2.6 / +4.0, concentrated in temporal) date-carrying titles;
  3→4 (+1.8 / +0.8) BM25 + small-to-big.
- Temporal retrieval (78.8%) now clearly *beats* full context (72.1%) —
  focused context outperforms a 24k-token transcript there; multi-hop
  remains retrieval's hardest category (60.9% vs 71.7%), consistent with
  its lower dev-split evidence recall (~72%).
- **Tried and rejected across rounds** (dev-gated, reverted):
  unrestricted neighbor expansion (2.4x context for the recall it bought),
  pseudo-relevance feedback (±0.2), bigram/sublinear-tf hashing embedder
  (−0.1), cosine-leg query expansion (0.0). Small-to-big only shipped once
  capped to the top-3 hits, where it cost +16% context for +2.4 recall.
- Iteration stopped here: retrieval is a coin flip with the ceiling on the
  scored set, so further tuning would be fitting noise.

Caveats: one dataset, one reader model, one judge model; category-5
(adversarial) questions excluded, following published practice. Numbers
published by other memory systems on LoCoMo use different readers/judges and
are not directly comparable to this table.
