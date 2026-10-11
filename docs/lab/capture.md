# Experiment `capture`: retroactive importance (synaptic tagging and capture)

Flag: `GRIMOIRE_EXP=capture` (off by default). Survey rows: `03-cognitive-neuroscience.md` 11-12.

## What it does

When a strong event lands on a fact F, low-importance facts written close to F
and sharing its scope get a bounded, decaying boost. Derived at index time, never
written into the markdown (no `imp=` change).

- **Event:** a person's fact (`by=human`, or a hand-edited bullet) that replaced
  at least one earlier fact. A person resolving a dispute is covered when the
  resolving fact supersedes one side. No prediction-error signal exists in the
  store, so agent updates are never events.
- **Targets:** live facts ranking at most 3 (declared 1-3, or unrated), not
  written by a person, not hidden, not from an untrusted origin.
- **Scope and window (constants chosen by reasoning, not tuned):** same session
  (weight 1) or same note (weight 1) or a shared entity (weight 0.5), within
  7 days strictly (|dt| < 7d).
- **Score:** max over events of `weight * 0.5^(|dt| / 2d)`. A maximum, not a sum,
  so a second event cannot stack without bound.
- **Effects under the flag:** (a) prune proposals exclude boosted facts
  (`PruneCandidates`); (b) recall adds `0.05 * score` to the ranking score
  (`wCapture`).
- **Storage:** table `memory_capture(note,id,score,reason,source)`, rebuilt on
  each memory write, sync and reindex. Undoing the event (deleting or rewriting
  the note) removes the boost on the next write.
- **Flag off:** no table is created by writes, no query text changes, rankMemory
  and PruneCandidates are unchanged. Verified by a test and by a probe (the
  flag-off server's index.db has no `memory_capture` table).

Files: `go/internal/index/capture.go`, `go/internal/index/capture_test.go`, and
guarded hook lines in `index.go` (removeRows, Reindex), `memory.go` (writeMemoryRows,
rankMemory, PruneCandidates), `sync.go` (end of a changed sync).

## Evidence

### 1. Planted timeline (`TestPlantedTimelinePromotesExactlyTheRelatedWrites` and others)

300 writes, two hours apart, one human correction at slot 200. The labels are
written by the test from the timeline and are not read back from the code.

| class | planted label | count | promoted |
|---|---|---|---|
| same note as the correction, in window | related | 31 (all related classes) | 31 |
| same session, in window | related | in above | yes |
| shared entity (Sonarr), in window | related | in above | yes |
| unrelated, same time, other session/note | negative | many | no |
| outside window, same note/session | negative | 2x50 | no |
| pulled (untrusted origin) in window | negative | 2 | no |
| hidden (private) in window | negative | 2 | no |
| already important (rating 4) in window | negative | 2 | no |

Result: precision 1.000, recall 1.000 (31 of 31 related, 0 false). **This is a
construction check.** The timeline is designed so the classes separate cleanly, so
it shows the rules are implemented as written, not how precise they are on real data.

Mutation check: removing the trust filter (`f.trusted = true || ...`) makes the
planted and pulled/hidden tests fail (2 unrelated pulled facts promoted). Restored.

### 2. Other checks (all pass)

- Decay: session-scope boosts equal `0.5^(dt/2d)` exactly and decrease with distance.
- Scope and source: an entity-only boost has reason `entity`, source is the correction.
- Second event: score with two corrections equals the max of the two alone, and is <= 1.
- Pulled event and pulled/hidden targets never boosted.
- Undo: removing the correction removes all boosts.
- Reindex reproduces the same rows exactly.
- Ranking: score = base + `0.05 * boost` exactly for every hit.

### 3. Prune simulation (planted timeline, `PruneCandidates(below=2)`)

| | off | on |
|---|---|---|
| total prune proposals | 204 | 173 |
| planted related (later needed) proposed | 31 | 0 |
| unrelated facts withdrawn from proposals | - | 0 |

Under the flag the 31 related facts are protected. Protection is only as good as the
relation, which is the construction check above.

### 4. Benchmark (`benchmarks/latency/bench.py`, sizes 1000 and 10000, n=200 queries)

Same binary for both arms. "on" = `grimoire-capture`, a wrapper that sets
`GRIMOIRE_EXP=capture` inside the exec'd process. `bench.py` strips `GRIMOIRE_*`
from the server environment, so setting it in the shell alone would have been
silently dropped. Proof the flag was live: the on-arm index.db has a
`memory_capture` table with rows (e.g. 36 at 1000 session, 51 at 10000 default); the
off-arm has no table.

Gold@10 (plain recall):

| corpus | size | off | on |
|---|---|---|---|
| default | 1000 | 0.89 (2 runs: 0.89, 0.89) | 0.995 (1 run), 0.89 (rerun) |
| default | 10000 | 0.695 | 0.695 |
| session | 1000 | 0.995 | 0.995 |
| session | 10000 | 0.935 | 0.935 |

Same-vault A/B (`/mnt/bulk/lab-work/capture/probe/probe.py`, 1000 default, 200
queries): top-10 lists identical in 200/200, gold 0.89 off and on. The one
0.995 result did not reproduce and is unexplained. Treat run-to-run noise at
1000 as at least 10 queries, not the 1-3 the protocol assumes.

Recall p50 (ms) and write p50 (ms): latencies vary by up to 2x between
identical runs on this shared host, so no latency claim is made.
Default 10000: recall plain 193 off / 342 on; write 599 off / 540 on.
Session 10000: recall plain 323 off / 251 on; write 448 off / 464 on.

**Measurement finding (affects the whole benchmark, not just this flag).** The
benchmark corpus writes ids that look minted (12 hex chars) but are not hashes
of their content. `memory` treats such ids as hand-edited, so `HumanAuthored()`
is true for about 99% of corpus bullets (10000 rows: 10064 human-or-hand of 10271).
Capture follows that same authority rule, so most targets are excluded and the
benchmark can produce almost no boosts (0 at 1000, 51 at 10000). **So this
benchmark cannot exercise the feature.** The corpus is shared and was not
changed here.

Side effect seen: reconcile update counts changed slightly (25->24, 32->33,
33->34) because reconcile reads ranking. Expected, not analysed further.

## Verdict

**KEEP BEHIND FLAG.** The mechanism is implemented, tested, deterministic and
reversible, and the prune and ranking effects are as designed. It is not graduated
because: the only evidence of relation quality is a construction check; the shared
benchmark corpus is inert for this feature because of the id artifact above; and
there is no real-usage data. Benchmark-level gold changes at 1000 were not
reproducible and do not show a benefit.

## Limits

- Events are human supersessions only. No surprise signal exists, so the
  high-prediction-error event in the brief is not implemented.
- The 0.05 ranking weight and the 7-day / 2-day constants are not tuned. Setting them
  against benchmark queries would be tuning on the test.
- Recomputing on every memory write is O(facts): cheap at 10000 in the write
  measurements, not measured at 50000.
- Boost protection from prune is binary: any score above 0 protects the fact, so
  an entity-only fact 6 days out is protected as much as a same-session one.
- An existing index built before the flag was turned on has no boosts until the
  next memory write or a `grimoire reindex`.
- `MemoryHit` carries no capture field; the reason is available through
  `ix.CaptureOf`, not in `/api/memory/explain`.
- The `hand` artifact means capture is effectively inert on the shared benchmark
  corpus; fix the corpus ids (generate real content hashes) before using it for
  any future memory-authority measurement.
