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
| `conformal` | Sized, abstaining context injection | conformal prediction | Coverage target met only at the loosest setting and only at 1k (0.69 vs 0.80 at 10k; 12 points short under shift). Abstention works: injecting into a no-answer query falls from 40% to 11-14% | not merged; abstention worth revisiting |
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

## What comes next: a randomised run on real work

Every result above comes from a corpus or a fixture we wrote. The next step is a
pre-registered holdout on live agent work, with no flags on:
[`benchmarks/holdout/PREREG.md`](../../benchmarks/holdout/PREREG.md). Its
analysis script is fixed at the same commit.

`benchmarks/holdout/per_memory.py` is an **exploratory** companion to that run.
It asks which individual memories seem to help or hurt, not whether injection
helps on average. Each memory is identified by its `target` (`fact:ID` or
`note:PATH`), the only identity column shared by the two arms. Its effects are
shrunk toward the overall effect (empirical Bayes), and a permutation test
asks whether memories differ at all. It reads outcomes with `analyze.py`'s own
loader and refuses to run before 2026-11-08. Nothing it prints is a
pre-registered finding. Its selftest is `per_memory.py --selftest`, which uses
synthetic data only.

## Anchors (2026-10-11)

A new idea tried after the sixteen above: a claim carries typed read-only probes
for the thing it is about, checked at write and, behind the `anchor` flag, at
recall. Write side is on for local callers; the recall side is behind the flag.
Measurements and limits: [`MEMORY-ANCHORS.md`](../MEMORY-ANCHORS.md).

## Seven gaps, measured and worked on (2026-10-11)

Each line is a gap found on one owner's live vault, the quick test that was run
on real data, and what the build now does. Counts only; the data is private.
Nothing here is a benchmark score.

| Gap | What was measured | What changed |
|---|---|---|
| No proof that injected memory helps | The holdout above is running. Its one analysis gives an average effect, not which memories help. | [`per_memory.py`](../../benchmarks/holdout/per_memory.py): per-memory effects with shrinkage and a heterogeneity test. Exploratory, and it refuses to read outcomes before the pre-registered end. |
| Facts cannot be checked | 0 of 329 live claims carried an anchor, because the writers send none. Marking and anchoring existing facts from an offline labelling pass: 165 marked as history, 12 claims given a passing anchor, text and ids unchanged. | Automatic weak anchors for writes that send none, `memory annotate` to retrofit, checks on other machines. See [MEMORY-ANCHORS.md](../MEMORY-ANCHORS.md). |
| Facts do not say which machine they are about | 66 of 108 dead path references in prose memory were paths on another machine. Guessing the machine from nearby text found 26 to 30 of the 66 and mislabelled 5 to 7 of the other 42. | `host` is recorded on write, and a check can run on an allowlisted machine over ssh. Guessing from text was not built. |
| What gets written is poorly chosen | 138 of 3,578 traced injections were stored facts, and 2 of those were linked to a following action, against about a third for memory files and notes. 19 of the 39 distinct facts injected were history. | Handoff facts that read as history are marked as events; the writer is told when a claim names nothing checkable. Leaving events out of injection is behind the `events` flag. |
| Updates replace true facts | 3 of 14 real supersessions struck a fact that was still true. | An update cannot replace a fact whose own anchors still pass; both are kept and the writer is told. |
| The staleness score misses real errors | A wrong version fact was scored 0.038 and marked "use". A wording rule (version number, "is deployed", worktree, temp path) flagged 42 of 346 facts and caught both facts that had really gone stale. Two cases is not a rate. | The wording rule and the anchor verdict apply at recall behind the `anchor` flag. |
| Skills are prose | `skills mine` returned 20 candidates, all habits. Two better miners found at best 5 real procedures in 24 candidates from 801 sessions, each with steps missing. Asking the writer for the procedure with checks gave 12 from 6 sessions; 6 were stored as posted and passed their checks, 5 named another machine the old way and 1 tripped the secret check on a placeholder (both since fixed). | `remember_procedure` and `skills write`: see [MEMORY-PROCEDURES.md](../MEMORY-PROCEDURES.md). |

A follow-up pass the same day asked agents, read-only, to find the specific
object behind each of the 149 claims that still had no check: 50 were grounded
(all 69 proposed checks passed when the server ran them), 23 were history, 15
were about another machine, 7 looked false and 54 could not be placed. The
claims that looked false were listed for the owner, not retracted. After it, 62
of 149 live claims carried a passing check. The first deploy after that showed
the loop working: the new version fact replaced the old one because the old
one's check had started to fail.

What is still open: the recall-side parts are off until the holdout ends, so
none of this has been shown to change what an agent does. Existing facts that
never named their object stay uncheckable. The numbers come from one vault.

## Built from the 2026-10-14 research sweep

A literature and practitioner sweep (about 2,300 papers and 244 practitioner
reports) and a counts-only look at the owner's injection trace from before the
holdout gave one clear finding: most injected memory repeats what the agent
already has. 69% of injections repeated an item already shown in the session,
64% were memory files whose summaries are already loaded, 4% were stored facts,
and only 4% of repeats followed a compaction
([`benchmarks/injection_stream`](../../benchmarks/injection_stream/README.md)).
What was built from it, all off by default. Nothing here changes the live service
until the holdout ends on 2026-11-08:

| Piece | Switch | What it does |
|---|---|---|
| Window dedup | hook `GRIMOIRE_CONTEXT_DEDUP=window` | An item stays excluded until the context window resets (compact, clear, resume), shared across prompt and tool-call stages. [MEMORY_USE.md](../MEMORY_USE.md) |
| Skip always-loaded content | `GRIMOIRE_EXP=loaded` + `context_loaded_paths` | Drops note items the agent already has loaded; `grimoire memory loaded-audit` asks a decision model which memory files add anything beyond their summary. [MEMORY_USE.md](../MEMORY_USE.md) |
| Rule cap and precision | `GRIMOIRE_EXP=rulecap` | At most `context_max_rules` rules per response; a rule shows how often its compiled check was right. [MEMORY_ADHERENCE.md](../MEMORY_ADHERENCE.md) |
| Deny once | `GRIMOIRE_EXP=denyonce` + hook `GRIMOIRE_HOOK_ALLOW_DENY=1` | A person's enforced rule denies the first violating call in a session, with the reason, then asks. [MEMORY_RULES.md](../MEMORY_RULES.md) |
| Receipts | `grimoire rules receipts` | Adherence from tool calls in transcripts, not from citations; the injected/not split refuses to run before the holdout ends. [MEMORY_RULES.md](../MEMORY_RULES.md) |
| Anchor history | always (passive) | Every probe result is kept with a hash of what it saw: live, dead or drifted, and pass-to-fail flips as staleness labels. [MEMORY-ANCHORS.md](../MEMORY-ANCHORS.md) |
| Write link | always (passive, hook) | A `remember` call's fact ids are linked to the writing session's trace hash, so writes can be joined to what that session was shown. [MEMORY_TRACE.md](../MEMORY_TRACE.md) |
| Offline harnesses | scripts | `benchmarks/lastwrite` (newest-statement-wins floor for update recognition), `benchmarks/correction_wording` (how a changed value should be shown), `benchmarks/rule_capacity` (adherence against the number of rules). Not yet run on real data. |
