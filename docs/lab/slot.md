# Experiment `slot`: collapse same-slot candidates before the top-k cut

## What it does

Behind `GRIMOIRE_EXP=slot` (`exp.On("slot")`, one guarded line in `rankMemory`).
After scoring and sorting, facts are grouped by slot, where a slot is the
subject and predicate that `memory.Attribute` reads (the same extraction
reconciliation uses). Negations and facts with no extractable slot are not
grouped. Each group keeps one representative, chosen by live, then authority,
then verified evidence, then later `valid_from`, ties keeping the higher score.
The others are reported as `alternatives` (count and ids) on the
representative, and the slot key appears on `explain`. A slot with an open
dispute (a `Challenges` link inside the candidate set) is never collapsed: all
its members return, marked `disputed`. The cut to `limit` happens after the
collapse, so freed positions go to other slots.

Files: `go/internal/index/memory_slot_collapse.go` (+ `_test.go`),
`go/internal/api/memory_slot.go` (+ `_test.go`), and hook lines in
`index/memory.go` and `api/memory.go`. With the flag off the JSON is unchanged:
`alternatives` and `slot` are `omitempty` and unset.

## Verification

- Flag off: `go test ./internal/index ./internal/api ./internal/memory ./internal/exp`
  pass unchanged, with the existing golden files untouched.
- Flag on: unit tests for representative order, disputes, negations, freed
  positions, and an end-to-end `MemoryEntries` check under `exp.Set`.
- `gofmt`, `go vet ./...`, `staticcheck` on the changed packages: clean.

## Measurement

Protocol: `benchmarks/latency/bench.py` (sizes 1000 and 10000, both corpus
modes, 200 measured queries, same binary, flag off vs on). Deviation: the
tracked harness strips every `GRIMOIRE_*` variable from the server's
environment, so `GRIMOIRE_EXP` would never reach the server. The run used a copy
of the harness with one patch that passes `GRIMOIRE_EXP` through. The tracked
file is unchanged. Proof the flag was live: each flag-on run returned 2000
server-emitted slot keys (0 in flag-off runs), and the flag-on responses carried
`alternatives` (for example 3281 collapsed ids at 1k default). The patched copy
and the analysis script live in `/mnt/bulk/lab-work/slot/bench/`.

Gold@10 and plain-recall p50 from `bench.py` (n=200; differences of 1 to 3
queries are noise):

| facts | corpus | flag | gold@10 | plain p50 ms | plain p95 ms |
|---:|---|---|---:|---:|---:|
| 1,000 | default | off | 0.890 | 55.2 | 85.7 |
| 1,000 | default | on | 0.860 | 46.0 | 53.0 |
| 10,000 | default | off | 0.695 | 207.3 | 271.4 |
| 10,000 | default | on | 0.670 | 422.6 | 765.0 |
| 1,000 | session | off | 0.995 | 29.0 | 44.9 |
| 1,000 | session | on | 0.985 | 49.9 | 61.4 |
| 10,000 | session | off | 0.935 | 196.5 | 340.1 |
| 10,000 | session | on | 0.880 | 373.7 | 952.6 |

The machine was shared (load average 11 to 35 during the runs), so the 1k
latency differences are inside the noise. The 10k increase is not: a Go
micro-benchmark of `collapseSlots` over 10,000 candidates measures 185 ms per
call. The key computation (`Attribute` and `IsNegation`, unmemoised regex work
per candidate) is the cost, at about 18 microseconds per scored candidate.

Analysis (`slot_analysis.py`, window 200 so a collapsed fact is seen even when
its representative ranks low; the top-10 prefix is identical to the default
window):

| facts | corpus | gold in top-10 off to on | gold collapsed behind another fact | queries with a collapse | distinct slots in top-10 off to on | corpus facts with a slot |
|---:|---|---|---:|---:|---|---:|
| 1,000 | default | 178 to 172 | **16** | 194/200 | 9.73 to 10.00 | 64.5% |
| 10,000 | default | 139 to 134 | **35** | 182/200 | 9.62 to 10.00 | 64.7% |
| 1,000 | session | 199 to 197 | **2** | 197/200 | 9.88 to 10.00 | 64.5% |
| 10,000 | session | 187 to 176 | **14** | 180/200 | 9.81 to 10.00 | 64.7% |

Mechanism. Every collision among live facts that share a key spans more than
one entity. At 1k default, 35 keys are shared by live facts, all cross-entity
(0 same-entity). At 10k default, 116 are shared, all cross-entity. The slot key
is built from subject text, so two people or services with the same display
name or first-name alias (`Dana owns ...`, `chroma is pinned to version ...`)
are treated as one slot. Session scope reduces the collisions but does not
remove them.

## Verdict

**REJECT** as specified. The target was zero gold facts collapsed behind a
different representative. The measured count was 2 to 35 per 200 queries, and
the risk the brief named (a wrong collapse hides the gold fact) happened. The
distinct-slot gain (+0.2 to +0.4 slots in the top-10) does not justify it.

Possible follow-up, not tested here: key slots on resolved entity identity
(the linker's entity IDs) rather than subject text, and cache the slot key at
write time so the per-candidate cost disappears. Both are design changes.
A rerun of this measurement would be needed before any graduation.

## Limits

- n=200 queries per cell. The 10k session drop (11 queries) is beyond noise;
  the 1k drops (2 to 6 queries) are not.
- The analysis ran with `explain=true`, which adds the slot key. Recall also
  records use counts, which feed the reuse bonus across the query sequence, in
  both arms. The same sequence is used for both flags.
- The Python port of `Attribute` and `IsNegation` was cross-checked against the
  Go keys on every returned hit of the flag-on runs: 8000 hits, 0 mismatches.
  Off-run keys come from the port.
- Only sizes 1k and 10k were run, per protocol. The 10k cost suggests the 50k
  cost will be about 1 s per recall in this form; this is extrapolated, not
  measured.
- Coverage: 64.5% of facts carry a slot. The rest have predicates outside
  `memory.Attribute`'s list (for example "works in", "listens on", "targets
  release").
- Latency numbers come from a shared, loaded machine (see above).

> Not merged. The code for this experiment is on branch `lab/slot`; only this write-up is kept on the integration branch, because the measurement did not justify the change.
