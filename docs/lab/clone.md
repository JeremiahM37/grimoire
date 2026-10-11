# clone: write-time alias texts with clonal selection (E5 + N7)

Flags: `clone` (E5 templates and coreference), `clone-llm` (adds local paraphrases,
requires `clone`), `clone-n7` (clonal selection, requires `clone`).
Files: `go/internal/index/memory_clone.go`, `memory_clone_test.go`; two guarded hook
blocks in `index.go` (Reindex) and `memory.go` (write path, ranking loop);
`benchmarks/clone/` (paraphrase set, local-LLM generator, driver).

## What it does

When `clone` is on, each memory fact gets alias texts stored in `memory_clone_alias`
(a table the index rebuilds from the vault). Aliases are embedded and feed the
semantic and keyword components of ranking, raising a fact's score to the best of its
aliases; they are never returned, never written to markdown, and each is attached only
to the (note, id) of the fact it came from.

- E5 templates: for facts in a small predicate lexicon (owns, prefers, works in,
  listens on port, runs on, is pinned to version, targets release, lives in), two
  restatements: `<subject> <noun>: <value>` and a wh-question. A fact opening with a
  pronoun (It/They/This/...) gets the antecedent substituted, from the last named subject
  in the same note; a short form that maps to two full names in the note abstains.
- clone-llm: paraphrases from `GRIMOIRE_CLONE_LLM_FILE`, produced offline by
  qwen3.5:4b (think:false). Each must pass a grounding check: no number absent from the
  fact, no capitalised mid-sentence word absent from the fact.
- clone-n7: only facts named in `GRIMOIRE_CLONE_SELECTION` (the recalled set) get
  candidates. At the end of a full reindex each candidate is kept only if (a) its own
  selection query's gold rank improves and (b) no selection query's gold rank worsens.
  At most 3 aliases are kept per fact. The check is exact: an alias changes only its own
  fact's score, so each new gold rank is the old rank plus the one crossing the alias
  causes (verified end to end in `TestCloneSelectionKeepsOnlyHelpfulAliases`).

Measured here vs open: the selection runs as a batch step at reindex time, driven by a
file. The live recall path does not record which facts were recalled, so the activated
set is simulated (the 300 paraphrase facts). An online activation loop is not built.

## Measurement protocol

- Binary: `go build ./cmd/grimoire` from `lab/clone` at the commit that adds the flag.
  Flag-off runs use the same build with `GRIMOIRE_EXP` unset.
- Standard: `benchmarks/latency/bench.py`, sizes 1000 and 10000, both corpus modes,
  200 measured queries per cell. `bench.py` now passes `GRIMOIRE_EXP` and the clone
  inputs through to the server (`PASSTHROUGH_ENV`). Flag liveness: the server logs a
  `clone:` line at every reindex with the flag on, and the run fails if it is absent.
  Each flag-on run's server log is in the work dir (`server-*.log`).
- Paraphrase set (`benchmarks/clone/paraphrase.py`): 300 current facts per corpus
  (seed `clone-ps:<n>:<scope>`), 3 rule-generated queries each.
  - R1 `Tell me what <S> <rule verb>.` is the SELECTION query. It is used only by N7.
  - R2 `Is <value> linked to <S>?` and R3 `What is the <rule noun> of <S>?` are HELD-OUT.
    All reported numbers are over R2+R3 (600 queries per cell). R2/R3 never influence
    any alias, threshold or selection.
- Local paraphrases: 300 facts (the paraphrase set at 1k default), one generation per
  fact, `think: false`, prompt contains only the fact text, never the queries. Ledger
  `/mnt/bulk/lab-work/clone/ledger.jsonl`: 301 generations of the 600 cap (one is a
  smoke test call, counted). The LLM arm was run only on the 1k default corpus, because
  10k would need 10k generations.

### Disjointness of paraphrase rules from alias templates

Both sides were written separately and checked by hand; they are not identical.
- E5 nouns (holding, preference, timezone, port number, host, pinned version, release
  target, repository location), E5 wh-frames ("Which/What ... does <S> have?") and the
  E5 nominal frame never occur in the paraphrase rules.
- Rule verbs (is responsible for, favours, is based in, accepts connections on, is
  hosted on, is locked to, is aimed at, is kept in), rule nouns (assignment, stated taste,
  working zone, listening socket, deployment machine, version lock, milestone, checkout
  location) and the frames "Tell me what", "Is ... linked to", "What is the ... of" never
  occur in E5.
- Shared: the fact's own value text and the subject name. Any query must carry these, so
  they cannot be kept disjoint without making the queries unanswerable. Gains therefore
  partly reflect matching on the value words; this is the main residual circularity.
- The standard benchmark is not independent of E5: its question templates use the same
  attribute words (timezone, port, version, release) that the E5 lexicon restates, and the
  lexicon mirrors the corpus generator's fact wording. Read the standard table as a
  no-regression check, not as evidence of gain.

## Results

### Paraphrase recall (held-out R2+R3, 600 queries per cell, gold in top-10)

| corpus | off | E5 templates | E5 + N7 | E5 + local LLM | E5 + LLM + N7 |
|---|---:|---:|---:|---:|---:|
| 1k default | 0.9552 (573) | 0.9638 (578) | 0.9690 (581) | 0.9707 (582) | 0.9741 (584) |
| 10k default | 0.8466 (508) | 0.8483 (509) | 0.8724 (523) | not run | not run |
| 1k session | 0.9931 (596) | 0.9966 (598) | 0.9983 (599) | not run | not run |
| 10k session | 0.9431 (566) | 0.9466 (568) | 0.9638 (578) | not run | not run |

Paired differences of 1 to 3 queries are noise. Single run per cell. Differences
under about 5 queries should not be read as effects.

### Standard benchmark, 200 queries per cell (gold@10, plain mode)

| corpus | N | off | E5 templates | E5 + N7 |
|---|---:|---:|---:|---:|
| default | 1,000 | 0.890 | 0.930 | 0.900 |
| default | 10,000 | 0.695 | 0.720 | 0.690 |
| session | 1,000 | 0.995 | 1.000 | 0.995 |
| session | 10,000 | 0.935 | 0.950 | 0.930 |

Expand mode (default, 1k/10k): off 0.880/0.705, E5 0.915/0.710, N7 0.885/0.700.

No regression beyond noise: the largest drop is 1 query (N7). The E5 gains (+8 queries at
1k default, +5 at 10k default) are at or above the noise line, but this benchmark's
templates overlap the E5 lexicon, so they are not evidence of gain.

Latency: the runs were sequential on a shared host and the numbers moved with load
(cold start alone varied from 4 s to 256 s across cells). They are not a clean
comparison, and the flag-on ranking path (alias scoring per candidate) was not isolated.
Do not read latency from this table.

### Aliases kept and index growth

| run | candidates | kept | per fact |
|---|---:|---:|---|
| E5, 1k default | 2,000 | 2,000 | 2.0 per fact |
| E5, 10k default | 20,000 | 20,000 | 2.0 per fact |
| N7 on 300 activated facts, 1k default | 600 | 79 | 0.26 per activated fact (of 2 candidates) |
| N7 on 300 activated facts, 10k default | 600 | 126 | 0.42 per activated fact |
| LLM + N7, 1k default | 1,199 | 112 | (candidates include 300 LLM paraphrase sets) |

Index growth is in rows, not bytes; bytes were not measured. Cold start with N7 is much
longer: the selection makes one full ranking per selection query and a per-trial
arithmetic check. Measured: about 22 s at 1k, and about 210 s at 10k (server log
`clone-n7` timing, in the reindex). E5 alone adds about 6 s at 1k and about 40 s at 10k
cold start (includes embedding the aliases).

### Coreference

Fixture (`TestCloneCorefFixtureAntecedentErrors`): 5 cases, 0 antecedent errors.
Unit tests cover abstention on an ambiguous short form. The benchmark corpus contains
no pronoun-led facts, so coreference contributes 0 aliases there. The fixture is tiny and
only shows the rule is implemented as written.

### task_benefit dry run (`run_task_benefit.py dry-run`, no model calls)

- Flag off: retrieval hits 7 of 10. Flag on: 7 of 10. Same tasks retrieved (T01, T03,
  T05, T07, T08, T09, T10); T02, T04, T06 missed in both.
- Flag-on index holds 8 alias rows for 21 entries; flag-off index has no clone table.
  The dry run uses `embed=off` (keyword only), so aliases can only raise the keyword
  component, and none crossed the top 10 for these tasks.

### Tests and checks

- `gofmt -l .`: clean. `go vet ./...`: clean. `staticcheck ./internal/index/...`: clean.
- No invisible Unicode in the new files.
- `env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP go test ./internal/index/ ./internal/api/`: pass.
- Clone tests (`-run Clone`): all pass, including the attachment test (an alias for one
  fact does not attach to a same-first-name fact) and the N7 end-to-end rank invariant.

## Verdict

**KEEP BEHIND FLAG.**

Reason: the paraphrase gains are small at 1k (+0.9 to +1.9 points, 5 to 11 queries of
600) and mostly within what the held-out set can resolve. The clearest gain is N7 at
10k default (+2.6 points, 16 queries), but it comes from aliases on the 300 facts that the
selection activated, which is exactly the set the paraphrase set measures. The standard
benchmark does not move beyond noise, and it is not an independent test. N7 cold-start cost
at 10k (about 3.5 minutes) is too high to ship without an online activation loop. The local
LLM arm was measured on 1k only. Graduating needs an independent paraphrase set (different
rule authors, or held-out LLM output) and an online activation source.

## Limits

- Activation is simulated by the paraphrase set, not by the live recall path.
- Predicate lexicon covers 8 predicates; facts outside it get no template aliases.
- Over-bound recall (above the 20,000-row scan window) does not see aliases: the candidate
  arms do not include an alias arm. Not measured at 50k (out of scope for this package).
- One run per cell; no repeated runs.
- The paraphrase set and E5 share the value words by construction.
- Latency was not measured in isolation.
