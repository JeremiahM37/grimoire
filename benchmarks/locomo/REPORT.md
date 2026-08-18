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
(`dev_recall_http.py`, evidence-turn recall — zero LLM calls), never against
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

## Round 6 — per-note cap and chunk size: both null (2026-08-18)

Two candidates developed against LongMemEval were scored here, plus a control
that is the decisive row. Full diagnosis in
[../longmemeval/REPORT.md](../longmemeval/REPORT.md).

| condition | multi-hop | temporal | open-domain | single-hop | **overall** | context tokens* |
|---|---|---|---|---|---|---|
| none | 1.1% | 0.0% | 6.5% | 1.1% | **1.2%** | 0 |
| full context | 71.7% | 72.1% | 58.1% | 92.3% | **82.2%** | ~24.0k |
| grimoire-go (round 5 reads) | 68.5% | 78.8% | 64.5% | 89.0% | **81.6%** | ~7.0k |
| control (identical contexts, re-read) | 55.4% | 76.0% | 61.3% | 88.3% | **78.0%** | ~7.1k |
| per-note cap | 47.8% | 69.2% | 61.3% | 89.7% | **76.0%** | ~7.1k |
| chunk 800→400 | 53.3% | 69.2% | 54.8% | 90.1% | **76.8%** | **~5.8k** |

\* median reader input tokens minus the `none` baseline.

**The control row is the finding.** Its contexts are byte-identical to
`grimoire-go` — verified for all 500 questions — yet it scores 3.6 points
lower, because 60 of 500 answers (12.0%) flip from reader and judge sampling
alone. McNemar rates that difference significant (p = 0.027) even though the
only thing that changed was the sample.

Measured against that control rather than against the stored round-5 reads:

- **per-note cap**: −2.0pp, 20 wins / 30 losses, p = 0.203.
- **chunk 800→400**: −1.2pp, 29 wins / 35 losses, p = 0.532, at 19% less
  context.

Neither is significant, and the cap's headline −5.6pp against round 5 was
mostly the re-read effect rather than the change. Multi-hop still moves most
under the cap (−7.6pp vs control), consistent with the mechanism found by
reading the questions it lost: allowing a note several slots pulls that
session's other topics in with the evidence, and LoCoMo's multi-hop questions
have list answers that a strict judge marks wrong when an extra item is added.

> *What video games does Nate play?* (gold: Valorant, CS:GO, Xenoblade, Street Fighter)
> control: "Counter-Strike: Global Offensive, Street Fighter, Xenoblade Chronicles" — correct
> capped: "Counter-Strike: Global Offensive, Street Fighter, **Chess, Catan**, Xenoblade" — wrong

Both changes were reverted. As an incidental but useful check, rebuilding the
round-5 condition with the current code and the cap disabled reproduced the
stored contexts **byte-identically for all 500 questions**, which independently
confirms that this session's retrieval-cache rewrite, BM25 determinism fix and
`fts_chunks` removal are all output-neutral on real data.
