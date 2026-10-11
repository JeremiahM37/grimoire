# Experiment `ips`: propensity-logged exposures and an offline ranking learner

Flag: `GRIMOIRE_EXP=ips` (default off). Branch `lab/ips`.

Status by kind of claim:

- **Measured here:** learner and off-policy estimators on a simulator with planted
  truth (20 seeds); latency-benchmark no-regression check at N=1000, flag off vs on.
- **Designed, not measured on real use:** everything that depends on live
  exposures. The live log cannot exercise it yet (see below).
- **Open:** whether the planted model is anything like real use. Nothing here is
  validated on real use.

## What it does

Logging (flag on only). Each eligible memory in a response is a *slate item*. It
now records: the slate id, its 1-based position in the ranked slate (before the
holdout removes items), whether the slate was shuffled for exploration, and the
seven ranking features (semantic, keyword, entity, recency, useful, link,
authority). The holdout probability (`p_withhold`) was already logged. New columns
`pos`, `slate`, `explored`, `feats` exist on `injections` and `trace_withheld`
with defaults that mean "not logged", so flag-off rows keep their meaning.

Position randomisation (flag on, setting `memory_position_explore`, default 0).
A share of slates has its order shuffled, independent of the features. Without
this there is no position propensity to estimate, and the learner says so.

Learner (`internal/rankfit`, pure arithmetic):
- examination curve exam(k) from explored slates, normalised exam(1)=1;
- weighted least squares of Used/exam(pos) on the features, weight
  exam^2 / P(shown), with P(shown) clipped at 0.05 (weights at most 20);
- naive fit (use on features, no position correction) for comparison;
- slate-level bootstrap intervals;
- off-policy value of a linear reranker of the logged slates, by inverse
  propensity (IPS) and doubly robust (DR, two-fold cross-fitted) estimators.

CLI: `grimoire memory learn-rank [--days N] [--json]`. Read-only: it opens
`adherence.db` with `mode=ro` (no migration, no write) and writes no weights.

Deployment (flag on only). `rankMemory` uses a weight vector from the index meta
key `memory_rank_weights` if one is present and well formed. Nothing writes that
key. Deploying learned weights is a separate decision.

## What the live log holds

Read-only counts, 2026-10-11 (counts only; no contents read):

| table | rows | with holdout (p_withhold > 0) | cited |
|---|---|---|---|
| injections | 2552 | 0 | 0 |
| trace_withheld | 0 | - | - |

The `pos`, `slate`, `explored` and `feats` columns do not exist in the live
database (0 of 4), because the running binary predates this work. The holdout rate
is 0 and no memory has been cited, so the live log has no propensities, no
positions, no features and no outcome. **The learner cannot be run on live data
today.** Enabling it needs `memory_holdout_rate` above 0, `memory_position_explore`
above 0, the `ips` flag, and time.

## Simulation

Model (`internal/rankfit/sim.go`):
- 20 candidates per slate with features x in [0,1]^7; the logger keeps the top K=8
  by its own weights w0 = (0.42, 0.28, 0.18, 0.04, 0.08, 0, 0), the live constants.
- Planted relevance r = w*.x with w* = (0.05, 0.10, 0.10, 0.45, 0.20, 0.05, 0.05).
  It differs from w0 mainly in recency.
- Examination exam(k) = 1/k. A shown item is used with probability exam(pos) x r.
- 20% of slates are shuffled for exploration; each item is withheld with
  probability 0.2 (uniform).

Success criteria, fixed before the run: normalised L1 distance between learned
and planted weights at most **0.10** (`rankfit.Tolerance`). Seeds: 20.

Results, 16,000 slates (128,000 exposures), 20 seeds, from `go test -run TestStudyTable`:

| claim | result |
|---|---|
| (a) naive fit learns the logger, not the planted model | naive L1 to planted: median 0.91 (0.87-0.96); cosine 0.69. Naive is nearer the logging weights than the planted ones in 20/20 seeds. Holds in the simulator. |
| (b) propensity-weighted learner recovers planted weights | L1 median 0.066 (0.033-0.099); 20/20 within 0.10; cosine 0.998. Median error in exam(k) 0.026. Holds at 128,000 exposures. |
| (c) off-policy ranks current vs learned as truth does | Truth: current 1.55, learned 1.72. IPS estimates 1.55 and 1.74; DR 1.56 and 1.74. Same order in 20/20 seeds for IPS and for DR. |

Claim (d), how much data (per-seed counts over 20 seeds):

| slates | exposures | L1 <= 0.10 | median L1 | IPS order right |
|---|---|---|---|---|
| 250 | 2,000 | 0/20 | 0.511 | 18/20 |
| 1,000 | 8,000 | 1/20 | 0.256 | 20/20 |
| 4,000 | 32,000 | 7/20 | 0.119 | 20/20 |
| 16,000 | 128,000 | 20/20 | 0.068 | 20/20 |

Claim (b) therefore holds only at about 10^5 exposures in this model. At 4,000
slates it fails on most seeds. `TestPropensityLearnerRecoversPlantedWeights` and
`TestOffPolicyRanksPoliciesLikeTruth` are run at 16,000 slates for that reason. The
4,000-slate failure is not hidden: it is the sample-size table above and an
explicit test.

Other checks: `TestExamNeedsExploration` (no explored slates gives
`ErrNoExploration`, not a guess); `TestExamIsRecoveredFromExploration`.

Limits of the simulation, stated plainly:
- The relevance model is linear and the examination model is exact, so the
  learner is well specified. Real use will not be.
- The holdout is uniform, so P(shown) is constant and the propensity weight does
  nothing here. The clipping and the zero-weight path are not exercised by
  claims (b) and (c).
- "Used" is modelled as a Bernoulli draw. The live signal is a citation, which is
  rarer and differently biased.
- Learned weights are in units of exam(1); only their direction and the policy
  ordering are meaningful. The value gap is compared on that scale.
- The naive learner is shown only to be nearer the logger than the truth. It is
  not decomposed into a position coefficient.

## Latency benchmark, no-regression (LAB rule 5)

`benchmarks/latency/bench.py --sizes 1000`, same binary (`go build` of this
branch), flag off and flag on (`GRIMOIRE_EXP=ips`, no weights stored). These are
the "no weights present" checks. Ports 19951-19956.

| corpus | flag | plain gold@10 | expand gold@10 | hops1 gold@10 | plain p50 ms | expand p50 ms |
|---|---|---|---|---|---|---|
| default | off | 0.89 | 0.88 | 0.89 | 36.1 | 59.9 |
| default | on | 0.89 | 0.88 | 0.89 | 26.0 | 54.7 |
| session | off | 0.995 | 0.975 | 0.995 | 32.8 | 73.8 |
| session | on | 0.995 | 0.975 | 0.995 | 26.9 | 57.2 |

Recall is unchanged in every cell. The latency differences are inside run-to-run
noise: the host's 1-minute load was 8-14 during the runs, and the same
configuration varied by 10-20 ms between runs. No latency claim is made.

Harness fix, which the orchestrator should know about: `bench.py`'s `server_env`
drops every `GRIMOIRE_*` variable, so the first "on" runs did not have the flag.
They were discarded. `benchmarks/latency/bench.py` now passes `GRIMOIRE_EXP`
through and drops all other `GRIMOIRE_*` variables (one line, in this branch).
The "on" rows above come from the corrected run. The tracked `results.json` and
`REPORT.md` were not touched.

## Verification

Run from `go/` with `env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP`:
- `gofmt -l .`: clean.
- `go vet ./...`: clean.
- `staticcheck` on the touched packages: clean.
- `go test` on `internal/rankfit`, `internal/adherence`, `internal/index`,
  `internal/settings`, `internal/exp`, `internal/utilization`, `internal/api`, and
  `cmd/grimoire`: see the final report.
- No invisible Unicode in the new files (checked).

## Verdict

**KEEP BEHIND FLAG.** The estimators behave as designed in a simulator that
matches their assumptions, and they need about 10^5 exposures to meet the
pre-set tolerance. The live log has no holdout, no positions and no citations, so
nothing can graduate. The learner is read-only and no weights are deployed.

## Open

- Real exposures: enable the holdout and the position exploration, then wait. The
  learner needs on the order of 10^5 exposures per arm of the planted scale.
- The logged position is the pre-holdout slate position. The agent sees the
  post-holdout list, which is shorter. The examination curve therefore describes
  the slate, not the displayed list. The holdout is small enough that this should
  matter little, but it has not been measured.
- `learn-rank` reports the off-policy values without intervals.
- Only the five scored components and link/authority are in the learned path. The
  phrase term stays as it is.
- No write path for the weights. Deploying them needs a decision and a measured
  effect on real use.
