# What agent memory still gets wrong, and what Grimoire does about it

Researched 2026-10-10 from 2025-2026 papers, benchmark audits and engineering
write-ups. Evidence quality is marked: **measured here** means a test or
benchmark in this repository covers it; **design** means the mechanism exists
but nothing here measures how much it helps; **open** means unsolved.

The point of this page is the last column. A memory layer that lists only what
it fixes is advertising.

| # | Documented limitation | What Grimoire does | Status |
|---|---|---|---|
| 1 | Retrieval degrades with store size; near-duplicate distractors hurt most | Hybrid FTS + vector + entity arms with rank fusion; bounded top-k; opt-in query expansion and a bounded entity-graph walk (`expand=true`, `hops=1\|2`) | design; latency cost measured in `benchmarks/latency` |
| 2 | Superseded facts are served as current; implicit updates are the hardest case | Write-time reconciliation, `valid_from`/`valid_to` alongside belief time, `valid_at`/`as_of`/range recall, freshness marks (`use`/`verify`) | design; slot rules measured in `benchmarks/longmemeval/REPORT-slots.md` |
| 3 | Memory is a persistent injection channel | Untrusted origins fenced and unable to supersede human-written facts; dream security sweep | design |
| 4 | Cross-scope leakage in shared memory (one multi-tenant service leaked on ~44% of deny probes in a 2026 evaluation) | Access filtering at the index; a deny-probe suite over every route, MCP tool, stream and export, which fails when a new route has no probe | **measured here**: the suite found and fixed 3 real leaks (see `SECURITY-LEAKPROBE.md`) |
| 5 | Deleting a source leaves derived summaries, observations, caches and index rows | `forget --cascade` removes or redacts derived artefacts, purges index rows, then re-searches the index and writes a receipt holding only a salted hash | **measured here** (tests); paraphrases only caught with an embedder, and copies outside the vault (backups, git history, sync peers) are out of reach |
| 6 | Stored preferences degrade factual answers and raise sycophancy | `mode=factual` recall excludes personal categories; `exclude_personal` for profiles | design; category-based, so uncategorised preferences still pass |
| 7 | Observed evidence, extracted facts and model inferences are collapsed into one record | A derived `basis` on every entry (`stated`, `observed`, `inferred`, `imported`, `pulled`) and an `evidence` field on writes | design; derivation rules are heuristics where origin is unrecorded |
| 8 | Memory can lower task performance; recall accuracy is not task success | Per-memory utilization trace with a randomized holdout, importance and use-based retention | design; see the utilization docs for the estimator's limits |
| 9 | LLM extraction on every write is slow and expensive | Deterministic rule path on the hot write; a model is optional and bounded by the rules' vocabulary | **measured here** (`benchmarks/latency`) |
| 10 | Benchmarks are unreliable: flawed answer keys, lenient judges, vendor-run numbers | Pre-registered protocols, full-context and no-memory baselines, null results published; no numbers here claim a ranking over other systems | see `benchmarks/*/REPORT.md` |
| 11 | Skills learned from failures can compound the failure | Mined skill candidates are drafts in a review queue; nothing enters memory until a person accepts one. There is no automated test gate on promotion | design; review-gated, not test-gated |
| 12 | Portability: memory trapped in one product | Open JSONL/markdown export; importers for three other systems' formats | **measured here** (round-trip tests) |

## Still open

- **Benchmark validity is not fixed by being careful.** The best available
  independent audit found a lenient judge accepting most deliberately wrong but
  topically adjacent answers. Any LLM-judged score, ours included, inherits
  that. Treat single-run differences of a few points as noise.
- **Deletion has no guarantee outside the vault.** Backups, snapshots and
  other synced devices keep what they had.
- **Basis is derived, not attested.** An agent that writes an inference without
  marking it is recorded as having inferred; one that lies about evidence is
  recorded as having observed. Provenance is only as honest as its writers.
- **Personal-versus-factual is a category rule.** It does not recognise a
  preference that nobody categorised.
- **Task-level benefit is not demonstrated.** Utilization tracing estimates
  whether a recalled memory was used; it does not show the task went better.
- **Privacy model.** Agent memory, uploads and imports land in the shared
  commons, and the `private` flag is a retrieval filter rather than an owner
  boundary. Single-user deployments are unaffected; multi-user ones should
  read `SECURITY-LEAKPROBE.md` before relying on isolation.
