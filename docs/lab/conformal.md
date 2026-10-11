# Experiment `conformal`: sized and abstaining context injection

Flag: `GRIMOIRE_EXP=conformal`. Off by default; with the flag off the context
path, its response and its routes are unchanged (the calibrate and status routes
are not registered at all).

## What it does

- **Sizing.** On the lexical path of `GET /api/memory/context`, the fixed
  relevance floor (0.3) and the count cap (`limit`, default 5) are replaced by a
  threshold `q` from split conformal calibration. Every candidate scoring at or
  above `q` is injected, still capped by `max_bytes`. The threshold is
  `q = j-th smallest gold score` with `j = floor((n+1)*alpha)+1`, which gives
  `P(gold >= q) >= 1-alpha` under exchangeability (`internal/conformal`).
- **Abstention.** When no candidate clears `q`, nothing is injected and the
  response says so: `"conformal": {"abstained": true, "reason": ...}`. The reason
  distinguishes "no candidate cleared the threshold" from "candidates cleared it
  but none fit the byte budget".
- **Storage.** Thresholds per alpha are stored in the index `meta` row
  `conformal_thresholds` (`internal/index/conformal_meta.go`), per deployment.
  Uncalibrated, the floor is the old 0.3 (`conformal.DefaultThreshold`).
- **Calibration.** `POST /api/memory/conformal/calibrate` (admin, flag-gated)
  takes `{pairs: [{query, gold}], alphas, default_alpha}`, scores each gold fact
  with the same candidate generation and score as the lexical path, and stores
  the result. `GET /api/memory/conformal` shows it. CLI:
  `grimoire memory conformal [calibrate FILE.jsonl]`. The pairs file is the
  source; the replay corpus or use trace can produce it, but that export is not
  wired in here.
- **Positive-score gate.** A threshold of 0 would admit zero-overlap candidates,
  so the applied floor is `SmallestNonzeroFloat64` (`conformal.Gate`).

Not covered: the hybrid (`rank=hybrid`) path is untouched. Its score scale is
different, so lexical thresholds do not transfer and it would need its own
calibration.

## Protocol (E8 harness: `benchmarks/latency/e8_conformal.py`)

- Calibration: corpus seed 20260701, N=1000, 1000 (question, gold) pairs.
- Held-out: corpus seed 20261010 (the bench seed), N=1000 and N=10000, 1000 gold
  queries each, plus 150 no-answer queries per size.
- No-answer set: 100 questions about entities the store never contains (made-up
  names, made-up services) and 50 questions about real entities asking for
  attributes never recorded.
- Shift: thresholds calibrated at N=1000 are used unchanged at N=10000.
- Baseline: flag-off server, default limit 5 and `max_bytes` 2400, plus a limit
  sweep k=1..10 for the fixed-k comparison. Flag-on: `max_bytes` 2400, no count cap.
- Exchangeability is assumed, not established: the calibration and held-out
  corpora are different seeds from the same generator, and the 10k store has
  twice the entities per fact of the 1k one.
- No model calls: lexical retrieval only, no embedder.
- Flag liveness: each flag-on server answers with a `conformal` block
  (`flag_on_probe` in `results.json`); the flag-off server is asserted not to.

## Results (n=1000 gold queries, 150 no-answer; the 1-3 query differences are noise)

Calibration (N=1000, seed 20260701): thresholds `alpha 0.05 -> 0`, `0.1 -> 0`,
`0.2 -> 0.667`; 30 of 1000 gold facts not returned by the candidate generator.

Coverage vs target (held-out, `max_bytes` 2400):

| N | alpha | target 1-alpha | coverage (flag on) | facts mean | bytes mean | inject-nothing on gold |
|---|---|---|---|---|---|---|
| 1000 | 0.05 | 0.95 | 0.820 | 5.88 | 1380 | 0.044 |
| 1000 | 0.10 | 0.90 | 0.820 | 5.88 | 1380 | 0.044 |
| 1000 | 0.20 | 0.80 | 0.820 | 4.61 | 1110 | 0.057 |
| 10000 | 0.05 | 0.95 | 0.694 | 7.05 | 1608 | 0.024 |
| 10000 | 0.10 | 0.90 | 0.694 | 7.05 | 1608 | 0.024 |
| 10000 | 0.20 | 0.80 | 0.694 | 6.48 | 1486 | 0.032 |

Fixed-k baseline (flag off, same budget), coverage / facts / bytes:

| N | k=5 (as deployed) | k=8 | k=10 |
|---|---|---|---|
| 1000 | 0.783 / 3.42 / 870 | 0.805 / 4.91 / 1178 | 0.820 / 5.79 / 1363 |
| 10000 | 0.638 / 4.14 / 1014 | 0.683 / 5.87 / 1369 | 0.693 / 6.87 / 1572 |

Equal coverage: at N=1000 the flag-on alpha=0.2 response (0.820 coverage, 4.61
facts, 1110 bytes) is matched by k=10 (0.820, 5.79, 1363), so it saves about 19%
of bytes at the same coverage, and at the 0.8 target k=8 (0.805, 4.91, 1178) is
about 6% more. At N=10000 the flag-on alpha=0.2 response (0.694, 6.48, 1486) is
matched by k=10 (0.693, 6.87, 1572), about 5% more bytes. The equal-coverage
saving is small and the alpha 0.05 and 0.1 settings inject more than k=5 for
+4 pp coverage.

No-answer abstention (fraction of requests injecting nothing):

| N | flag off, limit 5 | alpha 0.05 | alpha 0.10 | alpha 0.20 |
|---|---|---|---|---|
| 1000 | 0.600 | 0.600 | 0.600 | 0.893 |
| 10000 | 0.553 | 0.553 | 0.553 | 0.860 |

Mean facts injected on no-answer queries: off 1.13 (1k) / 1.66 (10k); alpha 0.2
0.24 (1k) / 0.42 (10k).

Shift (calibrated at 1k, served at 10k): coverage at alpha 0.2 fell from 0.820
to 0.694 against a target of 0.80; the flag-off baseline fell from 0.783 to
0.638. The threshold was calibrated on a store where 3% of gold facts were
missing from candidates, and the 10k store lowers that candidate ceiling
(gold@k=10 baseline 0.69 at 10k vs 0.82 at 1k). So the guarantee is not
preserved under this shift.

### Bench protocol (`benchmarks/latency/bench.py`, sizes 1000 and 10000, n=200 queries)

The bench measures `/api/memory` recall, which this change does not touch. The
flag is passed through `server_env` and each run records an `exp_probe`: flag-on
runs show `context_has_conformal_block: true`, flag-off runs `false`.

| corpus | N | off gold@10 | on gold@10 | off plain p50 ms | on plain p50 ms |
|---|---|---|---|---|---|
| default | 1000 | 0.890 | 0.890 | 29.4 | 29.8 |
| default | 10000 | 0.695 | 0.695 | 230.6 | 184.2 |
| session | 1000 | 0.995 | 0.995 | 46.3 | 23.5 |
| session | 10000 | 0.935 | 0.935 | 179.6 | 178.7 |

Gold@10 is identical as expected. The p50 differences (up to 2x) are not
caused by the flag: the code path under measurement is identical, and the host
load average was about 20 during these runs (shared with other services).
Treat latency here as noise, not an effect. The bench does not measure
`/api/memory/context`; the table above is the measurement of this change.

## Verdict

**KEEP BEHIND FLAG.** The abstention signal is real: on the no-answer queries
the baseline injects into 40% of requests, and alpha=0.2 cuts that to about 11%
at 1k and 14% at 10k. Sizing at alpha=0.2 is a few percent better than the best
fixed k at equal coverage. But the coverage guarantee does not hold: at N=1000
only alpha=0.2 reached its target (0.82 vs 0.80), alpha 0.05 and 0.1 do not
(threshold 0 means too many gold facts have no lexical overlap for any positive
threshold to reach 1-alpha), and under the 1k-to-10k shift coverage falls more
than 10 points below target. It is not a REJECT because the abstention behaviour
is measured and helpful at alpha=0.2 on this corpus. It is not a GRADUATE because
the guarantee fails for two of three targets and under shift.

## Limits and open items

- Exchangeability fails across store growth (1k to 10k). Recalibration on a
  schedule, or on the deployment's own replay data, is required before any
  guarantee can be claimed; the calibrate route supports it, the schedule does not exist.
- The coverage ceiling is candidate generation, not the threshold: the lexical
  path returns at most 100 candidates and scores with term overlap, so
  "no overlap" golds cannot be included at any threshold.
- Synthetic corpus only. Real question phrasing will differ from the generator's templates.
- Hybrid rank path not measured.
- The no-answer set is generated from the same templates as the corpus.
- Admin flag-on leakprobe runs were not performed (the test suite runs with the flag off, per protocol).
- Run: `E8_BINARY=<grimoire built with GRIMOIRE_EXP support> E8_WORK=<scratch> python3 benchmarks/latency/e8_conformal.py`
  (about 55 minutes on this host). Its results JSON is not committed.

> Not merged. The code for this experiment is on branch `lab/conformal`; only this write-up is kept on the integration branch. Abstention is the part worth returning to.
