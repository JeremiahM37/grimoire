# Experiment `convex`: convex-combination fusion of the semantic and keyword arms

## What it does

Flag: `GRIMOIRE_EXP=convex`. Code: `go/internal/index/memory_convex.go`, one guarded hook in `rankMemory` (`memory.go`).

Premise check. The current scorer does not use rank fusion. `rankMemory` computes a weighted
sum of raw arm values: `0.42*semantic + 0.28*keyword + 0.18*entity + 0.04*recency + 0.08*useful
+ 0.05*phrase`, then multiplies by importance and adds reuse. Semantic (clamped cosine) and keyword
(IDF-weighted token overlap) are already in [0,1]. The lexical FTS5 bm25 arm, the entity, link and
newest arms, and the semantic top-k are candidate generation (a union that decides which rows are
scored), not score fusion. The survey row's premise ("fusion throws away score magnitude") therefore
does not describe this code.

What the flag changes. With the flag on and a query vector present, the two scored arms are combined as

    arms = (0.42 + 0.28) * (alpha * semN + (1 - alpha) * kwN)

where semN and kwN are min-max normalised over the scored candidates of the query (a constant arm
maps to 0). The total weight of the two arms is kept at 0.70, so the other terms keep their scale.
Shipped `alpha` is 0.8, chosen on a tuning split (below). Overrides for measurement only:
`GRIMOIRE_CONVEX_ALPHA`, and `GRIMOIRE_CONVEX_NORM=none` (convex weights over raw values; this
ablation was NOT run, see limits).

Flag off: the expression is grouped as before (`arms := wSemantic*h.Semantic + wKeyword*h.Keyword`,
then the same left-to-right sum). `go test ./internal/index ./internal/exp ./internal/api` passes
unchanged with the flag off. No golden file was edited. A lexical-only query (no vector) keeps the
original formula even with the flag on.

## Choosing alpha (tuning split, not the benchmark seed)

Queries and corpus generated with seed 7310442 (benchmark seed 20261010 not used). Criterion fixed
before reading results: mean gold-in-top-10 over 1k and 10k facts, n=200 queries each.

| alpha | 0 | .1 | .2 | .3 | .4 | .5 | .6 | .7 | .8 | .9 | 1 | flag off |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| gold@10, 1k | .850 | .865 | .880 | .895 | .910 | .920 | .915 | .920 | .925 | .925 | .925 | .910 |
| gold@10, 10k | .775 | .790 | .800 | .805 | .810 | .815 | .810 | .820 | .825 | .815 | .805 | .810 |
| mean gold@10 | .813 | .828 | .840 | .850 | .860 | .868 | .863 | .870 | **.875** | .870 | .865 | .860 |

MRR@20 on the same split prefers alpha 0.5 (mean .648) over 0.8 (mean .621). The gold criterion was
kept as the choice, but the reader should know the two metrics disagree. The gold curve is flat
between 0.4 and 0.9 (within 0.015 of the peak, i.e. about 3 queries), so the optimum is not sharp.

## Sensitivity curve (standard seed 20261010, default corpus, reported not used for the choice)

gold@10 / MRR@20, n=200.

| alpha | 0 | .1 | .2 | .3 | .4 | .5 | .6 | .7 | .8 | .9 | 1 | flag off |
|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1k gold@10 | .855 | .885 | .875 | .895 | .895 | .890 | .890 | .905 | .900 | .900 | .905 | .890 |
| 1k MRR@20 | .680 | .692 | .697 | .704 | .705 | .717 | .714 | .718 | .719 | .705 | .687 | .709 |
| 10k gold@10 | .685 | .685 | .695 | .705 | .700 | .700 | .700 | .710 | .710 | .695 | .675 | .695 |
| 10k MRR@20 | .530 | .544 | .549 | .552 | .550 | .546 | .535 | .523 | .491 | .459 | .419 | .527 |

## Measurement (benchmarks/latency/bench.py, protocol: sizes 1k and 10k, n=200, both corpus modes)

Standard seed 20261010. Flag off vs on, same binary (`GRIMOIRE_EXP=convex` on the on-run, alpha 0.8).

| size | corpus | off gold@10 | on gold@10 | off p50 ms | on p50 ms |
|---|---|---|---|---|---|
| 1,000 | default | .890 | .900 | 29.8 | 52.3 |
| 10,000 | default | .695 | .710 | 193.8 | 321.1 |
| 1,000 | session | .995 | .990 | 49.3 | 49.7 |
| 10,000 | session | .935 | .915 | 320.8 | 282.3 |

Index and cold start (not changed by this flag): 1k 3.1 s off / 7.3 s on; 10k 49.6 s off / 49.3 s on
(cold-start times vary 2x with box load, so they are not evidence of anything).

Latency is not comparable across the rows: load average at start was 18 for the default-off run,
33 for default-on, 19 for session-off and 14 for session-on, because other lab agents were
benchmarking on this host. The p50 differences are dominated by that load. The fusion adds an O(n)
pass over the candidates; that cost was not isolated.

Differences in gold@10 are 1 to 4 queries out of 200. The brief treats 1-3 queries as noise, so
the default-corpus gains and the session-corpus losses are both within or near that band.

## Proof that the flag was live for each flag-on measurement

- Sweep cells set `GRIMOIRE_EXP` and `GRIMOIRE_CONVEX_ALPHA` in the server environment directly.
  Alpha 0 vs alpha 1 at 1k gives different gold and MRR on both splits, so the fused path is taking effect.
- bench.py stripped every `GRIMOIRE_*` variable from the server environment, so the first on-run
  would have been flag-off silently. Fixed: `PASS_THROUGH` in `bench.py` now keeps `GRIMOIRE_EXP`,
  `GRIMOIRE_CONVEX_ALPHA` and `GRIMOIRE_CONVEX_NORM`. Checked directly: `bench.server_env()` returns
  `GRIMOIRE_EXP=convex` when set, and omits it when unset.
- The first on-run was contaminated: another lab agent's server held port 19401, so the health check
  was answered by a different vault (gold 0.0 in every mode). Its results were discarded; the server
  log shows `bind: address already in use`. `bench.py` now refuses to start on a port that is already
  in use. The on-runs were repeated on 19430-19432.

## Verdict

**KEEP BEHIND FLAG.** The optimum found on the tuning split (alpha 0.8) gives +1 query (1k) and +3
queries (10k) on the standard default corpus, which is noise. On the session corpus at 10k it is -4
queries. On the standard seed MRR@20 at 10k is lower at alpha 0.8 (.491) than flag off (.527). The
flag stays off by default and nothing graduates.

## Limits

- The `GRIMOIRE_CONVEX_NORM=none` ablation (does min-max normalisation matter, or only the reweighting?)
  was implemented but not measured. The question of whether normalising changes anything is therefore open.
- Selection used gold@10 on one tuning seed; MRR favours a different alpha. No held-out set beyond the
  standard seed was used.
- n=200 per cell; the table cannot resolve effects below about 3 queries.
- The shared-box load differed across runs; latency numbers are indicative only.
- `go test ./internal/api` failed once in this session (first run, output not captured) and passed on
  rerun. Not diagnosed.
- Not run: the 50k size (per protocol, orchestrator runs finalists).

> Not merged. The code for this experiment is on branch `lab/convex`; only this write-up is kept on the integration branch, because the measurement did not justify the change.
