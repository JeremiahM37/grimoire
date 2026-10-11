# Memory lab: sixteen ideas, measured

In October 2026 about 500 papers, systems and techniques were surveyed across
ten areas (agent memory systems, retrieval, cognitive science, parametric
memory, databases, security, decision theory, compression, evaluation, and
deliberately distant fields such as accounting, immunology and library
science). Sixteen were chosen: eight established techniques to test here, and
eight new ideas inferred from the adjacent fields. Each was built behind a
named experiment flag (`GRIMOIRE_EXP=a,b`, see `go/internal/exp`), measured
with small offline tests, and given a verdict.

The survey itself is not in this repository. Many of its rows were recalled
rather than confirmed against a source during the survey, so nothing here
relies on a number from it: every claim below is a measurement made in this
repository, with its command in the linked write-up.

## What the lab found

**The most important result was a defect, not a feature.** A reference model of
the documented memory semantics, run against the real server with random
operation sequences (`oracle.md`), found six classes of divergence in the
write path. Four were plain bugs and are fixed (an agent could forget a
person's fact; forgetting a superseded fact rewrote its supersession pointer;
settling a dispute with several challengers left the others believed; a
dispute with a private challenger could not be settled). The fifth is the
serious one:

> Reconciliation supersedes a true fact when an unrelated fact shares its
> vocabulary. On generated pairs about different things (a different modifier,
> a different variant, or merely shared words) the rules supersede every time.
> On the author's live vault, 4 of 14 recorded supersessions struck a fact
> that was still true.

The common cause, which also sank two ranking ideas, is that the system
identifies a subject by its text. "Billing Postgres" and "Analytics Postgres"
are one subject to the slot rules; two people with one name are one slot.

## Verdicts

| flag | idea | origin | result | verdict |
|---|---|---|---|---|
| (tests) `oracle` | Reference model + conservation invariants as property tests | accounting, property-based testing | Found six divergence classes; four fixed; clean sequences rose from 21 to 137 of 1,500 | **shipped** (tests) |
| (fixes) | Write-path repairs from the oracle | - | See `oracle.md` | **shipped** |
| `cascade` | Replacement gate: ask a decision model "does this replace that, or can both be true?" before superseding | decision theory, distillation | False supersession 100% -> ~0% on the collision families; decision model 0.99 accurate on contested pairs; missed paraphrased updates 46% -> 59% | keep behind flag; audit ships |
| `header` | Contextual header (title, linked entities) on each fact before indexing | retrieval | 50k session corpus 74.5% -> 83.5%; ambiguous default corpus 62.5% -> 60.5% | keep behind flag |
| `guard` | Paraphrase-proof injection classifier + datamarking at recall | security | Scanner alone 120/140 held-out attacks; scanner or classifier 131/140; 3 false positives in 365 | keep behind flag |
| `clone` | Write-time aliases, kept only if they earn their place | retrieval + immunology | Paraphrase recall at 10k: 84.7% -> 87.2% | keep behind flag |
| `lineage` | Corroboration counts independent origins, not writers | truth discovery, hearsay rule | A wrong claim copied by 3+ agents no longer wins: 0/200 -> 200/200 by construction | keep behind flag |
| `justify` | Derived facts record their premises; invalidation is exact | truth maintenance, build systems | Incremental invalidation equals a full recompute on 2,000 random graphs | keep behind flag |
| `prospect` | Prospective memory: a fact with a cue that fires a reminder | prospective-memory research, checklists | Virtual-week fixture: 44/44 firings, 0 false | keep behind flag |
| `capture` | Retroactive importance after a correction | synaptic tagging and capture | Planted timeline: 31/31 related facts protected, 0 unrelated | keep behind flag |
| `conformal` | Sized, abstaining context injection | conformal prediction | see `conformal.md` | see `conformal.md` |
| `actr` | Base-level activation from the use trace | ACT-R | Small predictive signal; the bias guard cannot be wired without randomized exposure | keep behind flag |
| `ips` | Learning to rank from the randomized holdout | click models, off-policy evaluation | Recovers planted weights only from ~16,000 logged slates; the live log has none held out | keep behind flag |
| `convex` | Convex score fusion | retrieval | The scorer was already a weighted sum; differences within noise | not merged |
| `ppr` | Personalized PageRank arm | HippoRAG, ACT-R fan effect | At most 4 queries of 200 either way, at 1.4x to 2.4x latency | **rejected**, not merged |
| `slot` | Slot collapse before ranking | disputes + distractor studies | Hid the gold fact in 2 to 35 of 200 queries | **rejected**, not merged |

Nothing graduated to on-by-default. That is the honest state: the ideas that
help do so on a synthetic corpus shaped to exercise them, or need a decision
model or real usage data that an offline test cannot supply.

## What would move a flag to default

- `cascade`: a decision model reachable from the write path (it sends pairs of
  facts to that model), and a larger set of real supersessions than fourteen.
- `header`, `clone`: the alias linker recognising the name forms real notes use.
- `ips`, `actr`: the randomized holdout actually enabled, then weeks of use.
- `guard`: a calibrated threshold from the deployed decision server, and a
  measurement of whether datamarking changes what a reader model does.

## Benchmark caveats found on the way

- `benchmarks/latency/bench.py` stripped every `GRIMOIRE_*` variable from the
  server it starts, so a flag set in the shell was silently dropped. It now
  passes the experiment variables through. Any earlier flag-on number taken
  without proving the flag was live should be distrusted.
- The synthetic corpus writes ids that are not content hashes, so the store
  reads about 99% of its bullets as hand-edited, which means human-authored.
  Authority-dependent measurements on it are unreliable until that is fixed.
- The default corpus makes first-name aliases ambiguous on purpose, which caps
  what any ranking change can do there at about 68%.
