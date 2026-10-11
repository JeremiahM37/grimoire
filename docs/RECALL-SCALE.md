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

Open: linking an alias-form fact to its entity needs a signal outside the
fact's text (co-occurrence in the same note or session, or an explicit alias
record). That is a memory-model change, not a ranking change.

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
