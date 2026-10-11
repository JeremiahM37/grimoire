# Lab: `actr` (ACT-R base-level activation as the frequency-recency term)

Survey rows 32-34 (`docs/` survey 03, top-8 item 1). Flag: `GRIMOIRE_EXP=actr`.

## What it does

With the flag off nothing changes: the recency term is the fixed 90-day half-life
on the write stamp (`recencyScoreWith`, `memory.go`), and `RecordUse` only
updates the `uses` count and `last_used` (as before).

With the flag on:

* **Events.** Each `RecordUse` (a helpful vote, the only use signal) also writes
  the use time to `actr_events`. The last 32 event times per fact are kept
  exactly; older events are folded into `actr_folded` (count, first, last),
  treated as spread uniformly over their span. This is an explicit integral
  approximation of the sum, in the spirit of the optimised-learning
  approximation (Petrov 2006), not the same formula. Recalls are not events:
  recall counts are written on every query, so counting them would feed the
  retriever's own choices back into the score.
* **Activation.** `B = ln sum_j (t - t_j)^-0.5` over the creation stamp (always
  the first event, so a never-used fact is still ranked by its age) plus the
  stored and folded events. Future events are ignored, and ages are floored at
  one hour.
* **Ranking term.** `Recency := 1/(1+exp(-B))`, replacing the fixed term with the
  same weight `wRecency`. A fact with no usable stamp keeps the fixed term.
  Reuse, usefulness, importance and the other weights are unchanged.

Code: `go/internal/index/actr.go` (new), `actr_test.go`, `actr_sim_test.go`.
Shared-file edits: one line in `RecordUse`, four lines in `rankMemory`
(`memory.go`); `benchmarks/latency/bench.py` now passes `GRIMOIRE_EXP` through
(it stripped every `GRIMOIRE_*`, which would have silently dropped the flag).

## Measurement: latency benchmark (protocol: sizes 1000 and 10000, both corpus modes)

Same binary for off and on (`/mnt/bulk/lab-work/actr/grimoire`, built from this
branch). n = 200 measured queries per cell, seed 20261010. Differences of 1-3
queries are noise. The benchmark has no use history, so the flag here changes
only the shape of the recency curve (power law on the creation stamp).

| corpus | N | flag | gold@10 (plain) | gold@10 (expand) | gold@10 (hops1) | recall p50 (ms) |
|---|---|---|---|---|---|---|
| default | 1,000 | off | 0.890 | 0.880 | 0.890 | 56.6 |
| default | 1,000 | on | 0.890 | 0.890 | 0.890 | 33.8 |
| default | 10,000 | off | 0.695 | 0.705 | 0.695 | 210.7 |
| default | 10,000 | on | 0.705 | 0.725 | 0.705 | 194.8 |
| session | 1,000 | off | 0.995 | 0.975 | 0.995 | 52.2 |
| session | 1,000 | on | 1.000 | 0.970 | 1.000 | 30.7 |
| session | 10,000 | off | 0.935 | 0.875 | 0.935 | 211.6 |
| session | 10,000 | on | 0.935 | 0.885 | 0.935 | 216.8 |

* Gold@10 differences are at most 2 queries and go both ways. Noise.
* **Latency is not a measurement of the flag.** Each pair ran off then on, and the
  first run carried cold-cache effects (cold start 10.9 s off vs 3.5 s on at
  N=1,000 in the default corpus). The 1,000-N p50 gap (56.6 vs 33.8) is ordering,
  not the flag. The 10,000-N gap is within run variance. Indexing is unchanged
  except the events written on `RecordUse`, so cold start is not a flag effect.
* The flag was confirmed live: the flag-on index has `actr_events` and
  `actr_folded`, the flag-off index has neither
  (`sqlite_master` probe on `vault-1000/.grimoire/index.db`).

Reproduce: `bench.py --binary <bin> --work <dir> --out <dir> --sizes 1000,10000
[--alias-scope session] --port <p>` with `GRIMOIRE_EXP=actr` for the on arm.

## Measurement: simulation (the main evidence)

`go test -run ActrSimulation -v ./internal/index` (output in
`/mnt/bulk/lab-work/actr/sim.txt`). 20 seeds, deterministic. Constants come from
the design, not from outcomes: 200 facts (half stationary, half bursty), 365 days,
top-5 shown per day, a randomised-exposure day with probability 0.2, a 14-day
label window, probes every 30 days from day 210.

**Next-use prediction.** Label: the fact has a *randomised-exposure* use in the
14 days after the probe, so the label does not depend on the retriever's choices.

| predictor | AUC (mean +/- s.e.) | P@10 |
|---|---|---|
| fixed 90-day half-life on write stamp (today) | 0.497 +/- 0.023 | 0.010 |
| ACT-R, naive learner (all uses) | 0.559 +/- 0.021 | 0.023 |
| ACT-R, randomised-only learner | 0.555 +/- 0.022 | 0.022 |

The fixed term ignores use history by design, so this comparison favours the
activation term; read the gap as "history carries some signal", not as a win
over a tuned baseline. Gains are about 0.06 AUC, with a 14-day label window and
few positives per probe, so P@10 is near floor and not informative.

**Rich-get-richer guard.** Pool of 20 facts with equal usefulness 0.3; fact X
starts with 10 earlier uses, Y none (both logs). Ranked retriever, 365 days.

| learner | final B(X)-B(Y) | X share of last-90-day ranked exposure (uniform = 0.050) |
|---|---|---|
| naive (all uses) | +2.271 +/- 0.323 | 0.200 |
| randomised-only | +0.959 +/- 0.140 | 0.193 |

The guard reduces the gap by about 58% but does **not** remove the bias: X still
takes about four times its uniform share of ranked exposure. The reason is that
the randomised arm is 20% of days, and each day only about 0.05 uses of Y land in
it, so the head start decays slowly. This is a partial negative.

## Verdict: KEEP BEHIND FLAG

* The production learner is the **naive** one (every helpful vote). The
  simulation shows that learner feeds back the retriever's own exposure. The
  guard that fixes this needs a randomised exposure arm, and production has none:
  the holdout withholds items but does not randomise which items are shown, and
  a held item cannot be used. So the flag as built is the biased variant.
* Even the randomised-only variant leaves a 4x exposure skew after a year.
* Next-use gain is small (about 0.06 AUC against a baseline that ignores use
  by design). It is measured on a simulation, not on real traces.
* Benchmark gold@10 is unchanged within noise, as expected: the benchmark has no
  use history, so the flag cannot show a benefit there.

Not GRADUATE: the guard is not wired. Not REJECT: the mechanism is cheap, the
predictive signal is positive in the simulation, and it is measured. A test
would need a real exploration slice (e.g. eps of recall requests show a random
eligible set) and a use trace from real sessions.

## Limits and costs

* **Query cost.** `actrTraces` reads all stored events on every ranked query
  (O(stored events), up to 32 per fact). Not measured on a large use history;
  the benchmark has none. Likely the first thing to fix before any graduation.
* **Events are helpful votes only.** Use is rare in practice, so most facts fall
  back to their creation age, which is the power-law version of the fixed term.
* **Recalls are excluded on purpose.** Including them would feed the retriever's
  choices back into B. The cost is that most exposure is not learned from.
* **Folding is an approximation.** Test: 500 uniform events over two years, error
  under 0.05 in B. Not tested on bursty histories.
* Importance still scales the whole score; the importance-dependent half-life is
  replaced under the flag (ACT-R has no importance term).
* d = 0.5 is the standard value, not fitted. Logistic mapping B -> recency is
  unfitted; it keeps the old 0-1 scale so `wRecency` keeps its meaning.
* The `actr` simulation uses `actrBase` from production, so it tests the same
  arithmetic; it does not test the SQL path under concurrency.
