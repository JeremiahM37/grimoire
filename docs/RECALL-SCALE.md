# Recall accuracy at scale (gold-in-top-10)

Status: one ranking change measured on the synthetic benchmark. The 80% target at
50,000 facts was **not reached**. The diagnosis below explains why.

## Measured

Benchmark: `benchmarks/latency/run.sh --sizes 1000,10000,50000` (seed 20261010,
offline model2vec embedder, 200 measured queries per size, targets are
`corpus.queries(...)[20:220]`). Gold is the target fact in the top 10 of plain
`GET /api/memory?q=`.

| facts | mode | before | after | change |
|---:|---|---:|---:|---|
| 1,000 | plain | 0.885 | 0.885 | none |
| 1,000 | expand | 0.880 | 0.880 | none |
| 1,000 | hops1 | 0.885 | 0.885 | none |
| 10,000 | plain | 0.695 | 0.695 | none |
| 10,000 | expand | 0.695 | 0.705 | +1 query |
| 10,000 | hops1 | 0.695 | 0.695 | none |
| 50,000 | plain | 0.605 | **0.625** | +4 queries |
| 50,000 | expand | 0.605 | 0.620 | +3 queries |
| 50,000 | hops1 | 0.605 | 0.625 | +4 queries |

Before and after were both run on this machine on the same day, so the
comparison is like for like. Client p50 for plain recall moved 11.9 to 13.3 ms
at 1k, 124.8 to 143.1 ms at 10k and 229.9 to 238.9 ms at 50k. The machine is
shared and load varied during the session, so treat latency deltas as noise
unless reproduced. Write p50 at 1k moved 49.8 to 106.4 ms with no write-path
change; this is also presumed load, not measured as a regression.

Reproduce: `benchmarks/latency/run.sh --sizes 1000,10000,50000`. Raw runs are not
committed; the tracked `results.json` and `REPORT.md` are unchanged.

## Diagnosis (50k, baseline)

Of the 200 targets, 121 had gold in the top 10. Of the 79 misses, 66 were not in
the top 20 at all and 13 were at ranks 10 to 19. Splitting by how the gold fact
names its entity:

| gold text form | targets | gold in top 10 (baseline) |
|---|---:|---:|
| full display name (`Priya A. Schmidt owns ...`) | 137 | 119 (87%) |
| first name or alias only (`Ilya prefers ...`) | 63 | 2 (3%) |

The corpus writes about a third of person and service mentions with the alias
(`corpus.py`, `_make_fact`). The question always uses the full display name. For
an alias-form fact the query's last name (or numeric suffix, for services) is
not in the text, so the fact cannot be linked to the entity by any text signal:
many facts share the first name. Those 59 of 66 misses are not a ranking
failure a deterministic scorer can fix.

Among full-name misses, the dominant cause is **middle-initial collisions**.
"Priya N. Schmidt works in UTC" and "Priya A. Schmidt owns the backup rotation"
both score entity overlap 1. `properRE` requires two letters per capitalized
word, so the initial is dropped from entity extraction; `containsEntity` is a
substring test on the surviving words. Keyword scoring keeps single letters
(only stopwords are dropped), but its weight is too small to separate the two.

## Change

`go/internal/index/phrase_rank.go`: `phraseScore` is the longest run of query
tokens that appears contiguously, in order, in the fact's tokens, as a fraction
of the query's tokens. Runs of one word score zero. It is added to the base
score in `rankMemory` with `wPhrase = 0.05`. Tokens are the same
`memory.Tokens` keyword scoring uses, so the change is deterministic and
model-free.

Weight choice. `wPhrase = 0.15` was measured first and gave 129 of 200 (64.5%)
at 50k, but it broke the existing invariant that a supplied query vector
outweighs text when both are present (`TestSuppliedVectorLosesToTextWhenBothArePresent`).
The shipped value is the largest tested that keeps that invariant. It was not
chosen by searching over benchmark values, and no sweep was run.

| wPhrase | 50k gold@10 (plain) | full-name gold | alias gold | vector invariant |
|---:|---:|---:|---:|---|
| 0 (baseline) | 121/200 (60.5%) | 119/137 | 2/63 | holds |
| 0.05 (shipped) | 125/200 (62.5%) | 123/137 | 2/63 | holds |
| 0.15 (rejected) | 129/200 (64.5%) | 127/137 | 2/63 | fails |

The 50k number above (0.625) is the final benchmark on the shipped build. The
0.05 row here comes from a diagnostic run on the same vault; the final run's
plain recall gave 125/200, matching it.

## Negative result: the 80% target

Target: 160 of 200 at 50k. The ceiling for any scorer that cannot link alias-form
facts is the full-name share, 137 of 200 (68.5%). Reaching 80% needs 23 alias-form
targets recovered out of 63, which requires entity linking that the text does not
contain. The shipped change reaches 125 of 200 and recovers 4 of the 18 full-name
misses (7 absent, 7 at ranks 10 to 19 remain in the 0.05 build; the 0.15 build
left 3 absent). Full-name misses that remain are dominated by the same
middle-initial collisions and by numeric service suffixes (`gamarr-191` vs
`gamarr-413`), which the phrase term cannot resolve when the candidate is absent
from the pool.

The link itself needs a signal outside the fact's text. That signal is now
implemented as derived alias links; see the next section. It is not a change to
the markdown or the memory model.

## Alias linking (session scope)

Status: implemented and measured on the synthetic benchmark in two corpus
modes. The default corpus does not move, as designed. The session-scoped corpus
improves at every size. The 80% target at 50,000 facts is still not reached.

### What it does

An alias-form fact ("Priya prefers tabs", "auth-gateway runs on db01.prod") is
linked to a full entity only when the vault has evidence for exactly one
entity in the strongest available scope. The code is in
`go/internal/memory/alias.go` (pure functions) and
`go/internal/index/memory_alias.go` (derived tables and the flush).

- **Mentions.** A person is a first name, a middle initial and a last name
  ("Priya N. Schmidt"); the canonical name keeps the initial, so two people who
  share a first and last name stay distinct. Short forms are the first name and
  the first+last pair. A service is a hyphenated name with a numeric suffix
  ("auth-gateway-03"); its short forms are the stem and its first hyphen part.
  A host ("db01.prod") has the stem "db01" as its short form.
- **Evidence.** Each full mention writes one row per short form, with the note,
  fact id, session and stamp.
- **Scopes, strongest first.** Session: evidence in the same session, at any
  time. Note: evidence in the same note within 30 days. Co-mention: a fact that
  names another full entity X links to C when some earlier fact names both X and
  C. Unique: the alias has only one candidate entity, at or before the fact. The
  first scope with any candidate decides.
- **Ambiguity.** If the deciding scope has more than one candidate, nothing is
  linked. The scope is not allowed to fall through to a weaker one.
- **Time limits.** Co-mention and unique evidence count only if it is no later
  than the fact being linked. A full name first written later, in another
  session, does not change an earlier fact. Session and note evidence are not
  restricted this way.
- **Confidence.** session 1.0, note 0.8, co-mention 0.7, unique 0.5. A link adds
  `conf * 0.95` to the fact's entity score, so it stays strictly below a literal
  full-name match (1.0). The score is the larger of that and the fact's own
  entity overlap. Recall `explain` reports `link` and `link_scope`.
- **Query side.** A query only gets link credit when it names a canonical entity
  in full with an initial or a numeric suffix. "Priya Schmidt" with no initial
  gets no boost.

### Index

Three derived tables, created by the schema and rebuilt from the markdown:
`memory_mentions`, `memory_alias_src` (the evidence) and `memory_links` (the
resolved links, with scope and confidence). Nothing is written back to a note.

A note write marks the short forms it touched as dirty: the forms its evidence
added or removed, and the forms its facts use that the table already knows.
The flush re-resolves only the facts containing a dirty form, and writes only
the links that changed. A full reindex defers the flush to one pass at the end.
Deleting or rewriting the note that established an alias removes the links that
depended on it. An index built before alias links existed is upgraded by a
one-time backfill from the memory rows it already holds (no note is re-read and
nothing is re-embedded), tracked by an `alias_tables` marker. Writes for one note go in one transaction; per-statement
commits were the largest cost found (see Measurements).

The entity arm of candidate generation also includes facts linked to a name the
query gives in full, so a linked fact reaches the pool even above the scan bound.

### Measurements

Both corpus modes are in `benchmarks/latency/corpus.py`. The default mode is the
original corpus, unchanged. `--alias-scope session` adds session trailers so
that an alias mention shares a session with a full-name mention of the same
entity. The first fact of each session names the entity in full. About 20% of
sessions are alias-only and stay unresolved on purpose. Session stamps are
spread across the vault, because facts are chunked per entity. Real sessions
are tighter in time, so this measures the session rule without any time
locality.

Gold in top-10 for plain recall (`GET /api/memory?q=`), 200 queries per size,
seed 20261010. Before is the unmodified HEAD binary and after is this branch.

| facts | alias scope | before | after | expand before / after | hops1 before / after |
|---:|---|---:|---:|---|---|
| 1,000 | default | 0.885 | 0.890 | 0.880 / 0.880 | 0.885 / 0.890 |
| 10,000 | default | 0.695 | 0.695 | 0.705 / 0.705 | 0.695 / 0.695 |
| 50,000 | default | 0.625 | 0.625 | 0.620 / 0.620 | 0.625 / 0.625 |
| 1,000 | session | 0.940 | 0.995 | 0.940 / 0.975 | 0.940 / 0.995 |
| 10,000 | session | 0.845 | 0.935 | 0.845 / 0.875 | 0.845 / 0.935 |
| 50,000 | session | 0.710 | 0.745 | 0.705 / 0.710 | 0.710 / 0.745 |

The 1k and 10k "after" rows were re-run on the final build and match the
earlier runs exactly. The 50k "after" rows come from the build before the
per-note transaction change. Only performance changed in that change (the
gold results at 1k and 10k did not move), but 50k was not re-run, because the
50k benchmark is limited to two runs per mode and both were used.

Index time (cold start, measured on this machine with the model2vec embedder,
10k facts, server started alone, two runs each):

| build | 10k cold start |
|---|---:|
| before (HEAD) | 56.4 s, 57.2 s |
| after, one commit per alias statement (first version) | 82.0 s, 81.7 s |
| after, one transaction per note (final) | 58.1 s, 58.0 s |

The first version added about 25 s at 10k. A profile showed the time was blocked
in fsync, not in CPU: each alias row was its own autocommit. Batching the alias
writes into one transaction per note removed almost all of it. On the same disk
a bulk reindex of the 10k vault took 52.8 to 58.9 s before and 56.8 to 62.4 s
after, interleaved and with the cache disabled. The 50k cold start was measured
only on the unbatched build (478 s before, 747 s after, with other benchmark runs
sharing the machine), so it has not been measured since the batching change.

Write p50 at 10k was 334 to 357 ms before and 343 to 373 ms after, across the
sequential and the concurrent runs here. Those runs shared a machine with other
benchmark jobs, so this is a rough range, not a clean overhead figure.

Reproduce: `benchmarks/latency/run.sh --sizes 1000,10000 --alias-scope session`.
`run.sh` passes arguments to `bench.py`; a default run is unchanged.

### Limits

- **The 80% target is not reached in the session corpus** (0.745 at 50k). The
  alias-only sessions are unresolvable on purpose, and the link credit
  (0.95 times 0.18 weight) may be too small against the distractors that share
  the first name. I did not measure this, and I did not tune the weight against
  the benchmark. The 10k gain (+9 points) suggests the link is working, and the
  smaller 50k gain suggests the remaining misses are a ranking question. That
  would need a diagnostic run to confirm.
- **The default corpus barely moves**, as expected: its aliases are ambiguous by
  construction, so the linker refuses nearly all of them. The +1 query at 1k
  (0.885 to 0.890) appeared in both the pre-batch and the final run. It is one
  query, and I did not investigate which fact moved.
- **Ambiguous aliases leave no trace.** The spec's "record it as ambiguous" is
  implemented as "link nothing". There is no table or explain field for it.
- **Two-word short forms** ("Priya Schmidt") are resolved in facts, but they
  cannot create evidence, and a query with that form gets no boost.
- **Projects are not aliased.** Only person and service mentions create evidence.
- **Constants are reasoned, not tuned.** The confidences (1.0, 0.8, 0.7, 0.5),
  the 30-day note window, the 0.95 ceiling and the 64-key cap per fact were set
  from the design, not from benchmark results.
- **Common words can be short forms.** A service stem that is also an English
  word could link in the wrong place. Evidence and scope checks make that less
  likely, but do not rule it out. No real vault has been measured.
- **Per-fact short forms are capped at 64** in text order, so a very long fact may
  lose a late short form.

### Alias tests

- `go/internal/memory/alias_test.go`: full mention and short form extraction,
  the initial requirement, sentence-start words, services and hosts, the key cap,
  each scope rule, the ambiguity refusal, the note window, the co-mention and
  unique time rules, the retroactive-relink guard, and the link ceiling.
- `go/internal/index/memory_alias_test.go`: a session link ranks with its full
  name and reports `link_scope`; no boost without a full name in the query; an
  ambiguous session stays unlinked; a later full name in another session does not
  relink an earlier fact; deleting or rewriting the establishing note removes the
  links; a service links within its session; and a rebuild produces exactly the
  links the incremental writes produced.
- Existing `memory`, `index`, `db` and `api` tests pass (`env -u GRIMOIRE_SESSION
  go test`), along with `gofmt`, `go vet`, `go build` and `staticcheck ./...`.

## Tests

- `phrase_rank_test.go`: middle-initial separation, one-word and non-contiguous
  queries score zero, fraction semantics, and a lexical-only ranking test that
  fails when `wPhrase` is zero.
- `memory_importance_test.go` and `memory_test.go`: the score-sum and baseline
  formulas now include the phrase term.
- `testdata/memory_scan_golden.json` regenerated with
  `GRIMOIRE_WRITE_GOLDEN=1`. Order changed for the plain and valid_at queries
  and the set changed for two cases. The change is a ranking change on small
  fixtures, not only a score change.

Commands run: `gofmt -l .`, `go vet ./...`, `go build ./...`,
`go test ./internal/index/`, `go test ./...` (all pass with session environment
variables cleared; `internal/mcp` tests depend on the inherited Lectern session
and fail when it is set), `verify run .verify.yaml` (27 of 27 passed, with the
worktree venv linked to the main checkout's venv for the lint step).

## Limits

- One seed, one synthetic corpus, 200 queries per size. Differences of 1 to 4
  queries are within what a different seed or query draw could change.
- The benchmark is synthetic. The alias ratio and the name-collision rate come
  from the corpus generator, not from real vaults. The corpus was not edited.
- Latency numbers are from a shared machine with varying load.
- No held-out split was needed because no parameter was tuned on the benchmark.
  The one weight was set from the vector invariant and reported in full.

### On a real vault

The synthetic corpus writes people as "First M. Last" and services as
`name-NN`. Real notes mostly do not. On the author's own vault (1,968 notes,
365 memory facts) the upgrade ran as a 0.5 second backfill with no rebuild, and
found three full-name mentions (run ids such as `run-102`) and **no links**.
An earlier build linked eight facts through a false entity, `windows-wipe-2026`,
a slug ending in a year; year suffixes are now excluded and a test pins it.

So this is a mechanism that is correct and measured on a corpus shaped to
exercise it, with no demonstrated benefit on real data yet. Widening what counts
as a full name (two-word names with no initial, product names, host labels)
is what would make it matter in practice, and each widening needs a false-link
measurement on real notes before it ships.

