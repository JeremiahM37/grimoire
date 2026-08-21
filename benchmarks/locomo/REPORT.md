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

## Round 7 — three further candidates, all null (2026-08-18)

Scored against the same-day control (`probe-nocap`, byte-identical contexts
re-read alongside). Full table and reasoning in
[../longmemeval/REPORT.md](../longmemeval/REPORT.md).

| condition | multi-hop | temporal | open-domain | single-hop | **overall** | context tokens* |
|---|---|---|---|---|---|---|
| control (identical contexts, re-read) | 55.4% | 76.0% | 61.3% | 88.3% | **78.0%** | ~7.1k |
| chunk 800→400 | 53.3% | 69.2% | 54.8% | 90.1% | **76.8%** | ~5.8k |
| within-note excerpt | 50.0% | 74.0% | 64.5% | 88.3% | **76.8%** | ~7.6k |
| within-note excerpt + relevance gate | 51.1% | 76.9% | 67.7% | 89.4% | **78.6%** | ~7.2k |

\* median reader input tokens minus the `none` baseline.

None is significant (p = 0.53, 0.50, 0.77). The pattern that matters is
multi-hop: it falls under every change that puts more of one note into the
response, and recovers when a relevance gate takes that material back out.

**Coverage is not a usable gate on this dataset.** The within-note excerpt
raised held-out evidence-turn coverage here (84.5% → 85.5%, multi-hop
45.8% → 48.4%) and still lost accuracy; the per-note cap in round 6 left
coverage flat and lost 20 points of multi-hop. Twice, by different mechanisms,
better coverage came with worse answers — because LoCoMo's multi-hop questions
have list answers, and the extra material is a distractor rather than
evidence. Future changes here need an answer-precision measure, not just a
recall one.

## Round 8 — within-note excerpt: no measurable change (2026-08-18)

Round 7's within-note excerpt measured −1.2pp here and looked like the price of
its LongMemEval gain. Re-scored on a further **500 never-before-scored LoCoMo
questions** (stratified sample of the 1,040 outside the frozen set), against a
control retrieved with the unmodified binary and read in the same session:

| sample | n | baseline | within-note excerpt | delta | W/L | exact McNemar |
|---|---|---|---|---|---|---|
| frozen sample | 500 | 78.0% | 76.8% | −1.2pp | 24/30 | 0.497 |
| extension (never scored before) | 499 | 76.6% | 77.2% | +0.6pp | 22/19 | 0.755 |
| **pooled** | **999** | **77.3%** | **77.0%** | **−0.3pp** | **46/49** | **0.838** |

Pooled per category:

| category | n | baseline | excerpt | delta |
|---|---|---|---|---|
| multi-hop | 176 | 54.5% | 54.5% | +0.0pp |
| temporal | 215 | 69.8% | 69.3% | −0.5pp |
| open-domain | 55 | 61.8% | 58.2% | −3.6pp |
| single-hop | 553 | 89.0% | 89.0% | +0.0pp |

The two samples disagree in sign and the pooled effect is a third of a point,
so the honest reading is no measurable change. Multi-hop — the category that
lost 20 points to the round-6 per-note cap and 5 to the ungated round-7
variant — is flat at n = 176. Keeping one entry per note is what makes the
difference: breadth is untouched, so the over-inclusion that broke list
answers under the cap does not arise.

This is also the round's methodological lesson repeating. A 1.2-point
"regression" at n = 500 sat inside the noise this study measured in round 6,
and doubling the sample dissolved it. Effects of that size need roughly a
thousand questions here, not five hundred.

## Round 9 — baselines, and a corrected full-context claim (2026-08-18)

Standard IR baselines run inside this harness at a matched budget, with every
row — including full context — read and judged in one session.

| method | multi-hop | temporal | open-domain | single-hop | **overall** | ctx tokens |
|---|---|---|---|---|---|---|
| no memory | 1.1% | 0.0% | 6.5% | 1.1% | **1.2%** | 0 |
| lexical only (BM25) | 42.4% | 65.4% | 67.7% | 83.9% | **71.4%** | 6,126 |
| dense only (embeddings) | 53.3% | 76.0% | 64.5% | 87.5% | **77.4%** | 6,923 |
| hybrid as shipped | 50.0% | 74.0% | 64.5% | 88.3% | **76.8%** | 7,559 |
| full context | 66.3% | 75.0% | 61.3% | 92.3% | **82.3%** | 25,011 |

Paired against the shipped hybrid (exact McNemar, n = 500):

| comparison | delta | W/L | p |
|---|---|---|---|
| vs lexical only | **+5.4pp** | 54/27 | **0.0036** |
| vs dense only | −0.6pp | 33/36 | 0.810 |
| vs full context | **−5.5pp** | 23/51 | **0.0015** |

### A published claim, corrected

Rounds 1–5 reported retrieval as *statistically indistinguishable* from full
context on this dataset (p = 0.82 / 0.51). Measured in one session, it is not:
**full context wins by 5.5 points at p = 0.0015.** The earlier comparison put a
fresh retrieval run against `full` reads stored from a previous epoch, and
round 6 established that the re-read alone moves this dataset by 3.6 points.
The parity claim was an artifact of that gap and is withdrawn.

This is not a regression in the product — the retrieval numbers are what they
were. It is a correction to what they were being compared against.

### What it means

LoCoMo's conversations are ~24k tokens: they fit in the reader's window, so
reading all of it beats retrieving from it. That is the expected result for a
corpus that fits, and it is the opposite of LongMemEval's 118k haystacks, where
the same retrieval beats full context by 8.5 points. The two datasets bracket
the regime change, and quoting only the flattering side of it would be the
easiest way to make these numbers useless.

Fusion still earns its place against the classical baseline (+5.4pp over BM25,
p = 0.0036), though on this dataset the dense leg alone is as good as the
fusion — the mirror image of LongMemEval, where neither leg alone comes close.

## Round 10 — check the size before retrieving (2026-08-18)

Round 9 established that full context beats retrieval on this dataset by 5.5
points. That is a fact about the corpus, not about the retrieval: LoCoMo's
conversations are ~24k tokens and fit a reader's window, so there is nothing to
leave out and leaving anything out can only lose information. `/api/context`
now checks that before it ranks.

| method | multi-hop | temporal | open-domain | single-hop | **overall** | ctx tokens |
|---|---|---|---|---|---|---|
| hybrid retrieval | 50.0% | 74.0% | 64.5% | 88.3% | **76.8%** | 7,559 |
| **corpus-fits** | **68.5%** | 75.0% | 64.5% | **90.8%** | **82.1%** | 29,513 |
| full context | 66.3% | 75.0% | 61.3% | 93.0% | **82.4%** | 25,011 |

| comparison | delta | W/L | p |
|---|---|---|---|
| corpus-fits vs hybrid retrieval | **+5.3pp** | 59/32 | **0.0061** |
| corpus-fits vs full context | −0.3pp | 35/36 | 1.000 |

It recovers the gap and lands statistically on top of full context, which is
the ceiling for this dataset. Multi-hop moves most — 50.0% → 68.5% — which is
what you would expect from questions that need evidence scattered across
sessions: ranking was dropping the sessions, and now nothing is dropped.

The cost is honest and visible: 29.5k context tokens against 7.6k. That is
what reading everything costs, and it is only spent when everything is small
enough to be worth reading. All 500 questions chose the `full` branch here.

### The control

On LongMemEval the same code must do nothing, because ~118k-token haystacks are
far over any sane budget. Verified without a reader or a judge: across all 200
questions the endpoint chose `retrieved` every time, and the context it
returned was **byte-identical to plain retrieval for 200/200** — median 13k
characters against a 150k budget. Identical strings produce identical scores,
so that dataset's numbers are unchanged by construction rather than by
re-measurement.

## Follow-up: the answer-precision measure round 7 asked for

Round 7 ended on "future changes here need an answer-precision measure, not
just a recall one." That measure exists now, and it is a different study rather
than another round of this one: [../sufficiency/](../sufficiency/).

It uses the **446 category-5 questions this protocol excludes** — adversarial,
unanswerable, no gold answer to grade — as the precision axis, and scores them
without a reader or a judge, which is what makes it able to resolve differences
this harness no longer can.

Two results bear on the rounds above:

- **No retrieval statistic separates answerable from unanswerable.** Fifteen
  signals span AUC 0.414–0.581 on a stratified sample of 498; no learned
  combination generalizes across conversations (0.651 fitted, 0.564 held out);
  the result is the same on both embedders. So the "relevance gate" that
  recovered multi-hop in round 7 cannot be turned into a general
  answerability check — it works by removing material, not by knowing whether
  the material answers anything.
- **A reader can, and it is free.** A grounding verdict emitted in the same
  completion as the answer refuses 80% of the unanswerable questions.
