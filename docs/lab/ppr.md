# ppr: personalised PageRank arm over the fact-entity graph

Experiment flag: `ppr` (`GRIMOIRE_EXP=ppr`). Off by default. Code: `go/internal/index/memory_ppr.go`,
hooks in `memory.go` (MemoryQuery field, MemoryEntries hook, one line in rankMemory) and
`memory_candidates.go` (one arm in the over-bound candidate path). Tests: `memory_ppr_test.go`.

## What it does

When the flag is on and the query has text, `MemoryEntries` builds a small bipartite subgraph
(facts and entity names; edges from `memory_entities` and from `memory_links` weighted by link
confidence). It starts from the entities the query names, expands at most 2 layers, stops at
2,000 facts, and runs 8 power iterations with restart probability 0.5. Two rules keep it from
being a route around ranking: fan normalisation (an entity sends mass in proportion to its
weighted degree), and an authority prior on the entity-to-fact step (human 1.0, agent 0.7,
pulled 0.3). The weights are ordered like `memory.Authority` and were chosen from that ordering,
not tuned. Every lookup applies the ranking window's visibility predicate.

The scores enter in two places: as a ranking term `wPPR * score` with wPPR = wEntity/2 (0.09),
and, above the scan bound only, as a fifth candidate arm (top `pool` facts by PPR mass).

Flag off: `q.ppr` stays nil, the hook lines are not reached, and ranking is unchanged. The
existing `index` tests pass unchanged with the flag off (`TestBelowScanLimitMatchesGolden` is
flag-off and still passes; with `GRIMOIRE_EXP=ppr` it fails, as expected, because ranking changes).

## Measurement

Protocol: `benchmarks/latency/bench.py`, sizes 1,000 and 10,000, both corpus modes, same binary
(`grimoire-ppr`) for off and on. On runs start the server through a wrapper script that exports
`GRIMOIRE_EXP=ppr` (bench.py strips `GRIMOIRE_*` from the server environment, so the wrapper is
the way the variable reaches the server). n = 200 measured queries per mode. Differences of 1 to 3
queries (0.005 to 0.015 gold) are noise. Port 19250. Results: `/mnt/bulk/lab-work/ppr/out-{off,on}-{default,session}/results.json`.

Machine load was high and varied during the runs (1-minute load average 13 to 40, from other
workloads on the box), so latency comparisons are indicative only. Off and on alternated in
one script, but not at the same load.

Plain recall (`GET /api/memory?q=`), gold@10 and p50 client latency:

| size | corpus | off gold@10 | on gold@10 | off p50 ms | on p50 ms |
|---:|---|---:|---:|---:|---:|
| 1,000 | default | 0.890 | 0.895 | 28.8 | 41.8 |
| 10,000 | default | 0.695 | 0.690 | 209.3 | 298.2 |
| 1,000 | session | 0.995 | 1.000 | 26.4 | 64.2 |
| 10,000 | session | 0.935 | 0.955 | 199.9 | 472.6 |

Writes (p50 ms, reconciled updates of 200): default 1k 55.2 off / 54.5 on (25 / 26); default
10k 457.3 / 477.0 (32 / 35); session 1k 51.6 / 96.8 (24 / 26); session 10k 452.4 / 799.8 (33 / 34).
Cold start (s, 1k / 10k): default 2.7 / 39.2 off, 3.4 / 46.8 on; session 2.7 / 28.9 off, 4.7 / 42.3 on.

Other recall modes at the same sizes (gold@10): expand off/on 1k default 0.88 / 0.895,
10k default 0.705 / 0.705, 1k session 0.975 / 0.975, 10k session 0.875 / 0.885; hops1 off/on
1k default 0.89 / 0.895, 10k default 0.695 / 0.69, 1k session 0.995 / 1.0, 10k session 0.935 / 0.955.

**Flag confirmation.** The wrapper's environment was checked directly (`envcheck.sh` prints
`GRIMOIRE_EXP=ppr` in the exec'd process). Behaviour differs between the on and off runs at every
cell (plain and hops1 gold, token counts, and latency roughly 1.4x to 2.4x higher), which the flag-off
path cannot produce. The unit tests exercise the same code under `exp.Set`.

**Cold start and index.** Nothing in the indexing path changed; the cold-start differences above
are load noise, not a measured cost of this arm.

**Not measured.** 50,000 facts (the orchestrator runs finalists). At 10,000 facts the scan window
(20,000) covers the whole store, so the candidate arm never runs in these cells; only the ranking
term is exercised. The candidate arm is the part that could matter above the bound and is unmeasured.

## Adversarial fixture (pulled fact on a popular entity)

`TestPPRPulledFactOnAPopularEntityStaysOutOfTheTopFive`: 12 human facts about one person mention a
popular entity (Grafana); one pulled fact (origin `gmail`) mentions the same entity. With the flag on,
the pulled fact is not in the top five for a query about the human facts, and its PPR score is
strictly below a human fact's. Result: PASS. The fixture is a unit test, not a benchmark query set.

## Limits

- The synthetic corpus gives each fact about one entity, with about two facts per entity. The
  graph has little multi-hop structure, so a PPR walk has little to add beyond the existing entity
  arm. This is the main reason for the null result and is a property of the corpus, not a proof
  about real notes.
- The ranking term changes reconcile target selection too (MemoryEntries is shared), so the write
  path is affected when the flag is on. The write-side numbers above show no measurable change in
  reconcile update counts (24 to 26, 32 to 35, within noise).
- Latency: the walk adds two to three SQL round trips per hop plus in-memory iteration. At 10k
  facts the on-cost was about 90 ms p50 (plain) under the measured load.
- Authority weights (1.0 / 0.7 / 0.3) are reasoned from the authority ordering, not measured.

## Verdict

**REJECT** at 1,000 and 10,000 facts: no gold@10 gain beyond noise (the largest difference is 4 of
200 queries, mixed in sign), and plain recall p50 rises by roughly 1.4x to 2.4x. The code stays
behind the flag rather than being deleted, so the 50,000-fact candidate arm can be run as a
finalist if the orchestrator wants that measurement; this doc does not claim it helps there.

> Not merged. The code for this experiment is on branch `lab/ppr`; only this write-up is kept on the integration branch, because the measurement did not justify the change.
