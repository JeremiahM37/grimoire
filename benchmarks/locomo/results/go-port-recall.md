# Go port — retrieval parity measurement

Evidence-turn recall over LoCoMo, run through `dev_recall_http.py` against both
implementations on the SAME corpus, questions and scoring. No LLM calls, so the
number is deterministic and free of judge variance — what is being compared is
two retrieval stacks, not two models.

Both servers: model2vec (`potion-base-8M`), no Ollama, watcher off, same vault
content, freshly reindexed.

| | evidence-turn recall | multi-hop | temporal | single-hop | open-domain |
|---|---|---|---|---|---|
| Python | **360/539 = 66.8%** | 57.3% | 89.2% | 81.5% | 52.0% |
| Go     | **356/539 = 66.0%** | 56.1% | 89.2% | 81.5% | 52.0% |

**Gap: 4 evidence turns, 0.8pp**, entirely within multi-hop; temporal,
single-hop and open-domain are identical. On a smaller sample (2 conversations,
n=184) the gap was 1.6pp, so it narrows with more data — consistent with
rank-flip noise rather than a systematic deficiency.

## Why the two are not identical

Chunking, corpus contents and corpus order are identical (verified row by row).
The residual is arithmetic: `np.linalg.norm` is BLAS `sdot`, whose float32
accumulation order depends on the BLAS kernel and is not portably reproducible
in Go. That leaves stored vectors differing by at most 1.5e-6. Reciprocal-rank
fusion converts a chunk's RANK into its score, so two chunks with nearly equal
cosine can swap places, and a swap near the cutoff changes what the reader sees.

Everything else about the embedder IS byte-exact: numpy's mean is plain
sequential accumulation (verified empirically, not assumed), and Python's
`+ 1e-32` promotes the norm to float64 before dividing — both are reproduced, and
a direct embedder A/B on ASCII text matches byte for byte.

## What this licenses

The published LoCoMo (81.6%) and LongMemEval (75.0%) figures were measured
against the Python build with an LLM reader and judge. This measurement said the
Go build's retrieval is within ~1pp of it on a proxy metric — close enough to
expect those figures to hold, NOT close enough to quote them for the Go build
without re-running the full scored pipeline.

## The full scored pipeline, re-run (2026-08-14)

So it was re-run, both datasets, contexts regenerated from a live Go server:

| | LoCoMo (n=500) | LongMemEval (n=200) |
|---|---|---|
| Python + model2vec | 80.8% | 75.0% |
| Go + model2vec | **81.6%** | **74.5%** |
| exact McNemar | 30W/26L, p = 0.69 | 6W/7L, p = 1.00 |

Indistinguishable on both. The ~1pp proxy gap did not translate into a scored
difference in either direction, which is what "within noise" is supposed to
mean. The Go build may now quote these figures as its own.
