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
| + OR fallback & excerpts (round 2) | **82.9%** | **87.8%** | ~19.7k chars |

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

## Reading the numbers

- **Round 3 grimoire + nomic-embed (80.8%) is statistically
  indistinguishable from the full-context ceiling (82.2%)**: among the 85
  questions where exactly one of the two was correct, retrieval won 39 and
  full context won 46 (exact McNemar p = 0.52, n = 500) — while using
  ~4.4x fewer context tokens per question (~5.4k vs ~24k).
- The `none` floor (1.2%) shows the questions are not guessable; the reader
  only knows what the condition supplies.
- The round 1→2 jump (+19.6 offline / +22.8 with nomic) is attributable
  entirely to the four generic retrieval fixes; the round 2→3 jump (+2.6 /
  +4.0, concentrated in temporal) to date-carrying titles.
- Temporal and open-domain retrieval now *beat* full context — focused
  context beats a 24k-token transcript on those categories; multi-hop
  remains the hardest for retrieval (64.1% vs 71.7%), consistent with its
  lower dev-split evidence recall (~70%).
- **Tried and rejected**: neighbor-chunk expansion ("small-to-big"
  retrieval) raised dev recall of the semantic leg from 61.9% to 73.6% but
  cost 2.4x the context — worse recall-per-token than the existing hybrid,
  so it did not ship.
- Iteration stopped here: the remaining gap to ceiling is inside sampling
  noise for n = 500, so further tuning would be fitting noise.

Caveats: one dataset, one reader model, one judge model; category-5
(adversarial) questions excluded, following published practice. Numbers
published by other memory systems on LoCoMo use different readers/judges and
are not directly comparable to this table.
