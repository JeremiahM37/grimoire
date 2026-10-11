# Lab: `header` (deterministic contextual header before indexing)

Experiment flag: `GRIMOIRE_EXP=header`. Off by default; with the flag off the
index, the rows and the ranking are the ones the code produced before this
change (`go test ./internal/index ./internal/api` passes unchanged).

## What it does

A memory fact is one sentence, so its vector and its keyword match see only
that sentence. With the flag on, each fact is embedded as `header. text` and
its keyword match runs over `header text`. The header is built from data the
index already holds, with no model:

- **title**: the note title (`vault.Note.Title`). In the benchmark corpus every
  note is titled `Memory`, so this field carries no entity information there.
- **entities**: the full names the fact mentions (`memory_mentions`) plus the
  full names its short forms are linked to by the alias linker
  (`memory_links`). This is the field that can carry a surname the fact does
  not spell out.
- **date**: the day of the fact's stamp.

Variants are chosen with `GRIMOIRE_HEADER_FIELDS` (comma list of `title`,
`entities`, `date`; default all three).

Recall still returns `memory_entries.text` unchanged. The header lives in a new
side table `memory_headers(note, id, title, date, header)`, created on first use
under the flag, so a flag-off index does not have it. The header is refreshed
whenever the alias flush changes a fact's links, and its embedding is recomputed
then, so the stored vector always matches the header the index holds.

Hook lines in shared files (all no-ops with the flag off):
`memory.go` (delete, write, rank keyword), `memory_alias.go` (flush).
New code: `index/header.go`, `index/header_test.go`.

**Not covered:** `memory_fts` (the FTS5 table used only by the over-bound
lexical candidate arm) is an external-content table over `memory_entries.text`.
Putting the header in it needs a schema change. The measured sizes are below the
20,000-row scan bound, so they use the window path, which does not read FTS.
The keyword arm does see the header.

## Proof the flag was live

- `bench.py` stripped every `GRIMOIRE_*` variable from the server environment.
  It now passes `GRIMOIRE_EXP` and `GRIMOIRE_HEADER_FIELDS` through
  (`SERVER_PASSTHROUGH`). This was in place before the first flag-on run.
- Every flag-on index has a `memory_headers` table with one row per fact (1,270
  at N=1,000 default; 1,273 session). Every flag-off index has no such table.
- The near-duplicate statistics below differ between arms on the same corpus.

## Protocol

`benchmarks/latency/bench.py`, sizes 1,000 and 10,000, corpus `default` and
`--alias-scope session`, same binary (built from this branch), flag off vs on.
n=200 measured queries per cell, seed 20261010. Gold is the target id in the
plain-mode top 10. Differences of 1 to 3 queries (0.005 to 0.015) are noise.
Machine load average was 6 to 22 during the runs, so latencies are indicative
only. Results: `/mnt/bulk/lab-work/header/out-*/results.json`.

## Recall: gold in top-10 (plain) and p50 latency

| corpus | N | off | on (title+entities+date) | on (title) | on (title+entities) |
|---|---:|---:|---:|---:|---:|
| default | 1,000 | 0.890 (68 ms) | 0.880 (57 ms) | 0.880 (31 ms) | 0.885 (40 ms) |
| default | 10,000 | 0.695 (190 ms) | 0.690 (219 ms) | 0.700 (199 ms) | 0.720 (263 ms) |
| session | 1,000 | 0.995 (27 ms) | 0.995 (34 ms) | 0.990 (48 ms) | 0.995 (31 ms) |
| session | 10,000 | 0.935 (211 ms) | 0.985 (261 ms) | 0.940 (332 ms) | 0.990 (227 ms) |

Expand and hops1 modes follow the same pattern (session 10k expand: 0.875 off,
0.985 on, 0.885 title, 0.985 title+entities). Full tables are in the result files.

## Index size, index time

- Index DB, N=10,000 session, cold build: 36,036,608 B off, 37,707,776 B on (+4.6%).
  N=1,000 default: 4.9 MB off, 5.1 MB on.
- Cold start, N=10,000 session, same vault, back to back: off 28.7 s and 31.2 s,
  on 35.9 s and 47.2 s. Two pairs only, under shared load. The extra time is the
  header embedding (longer input) plus the re-embedding after alias flushes.
  The number of re-embedded facts was not counted.
- The main-protocol cold starts (3.9 to 78 s) are dominated by load and are not
  used here.

## Near-duplicate structure (N=10,000, fact vectors, 256 dim)

| arm | corpus | mean cos within note | mean cos across notes | top-1 neighbour in same note | pairs cos>=0.9 (within / across) |
|---|---|---:|---:|---:|---|
| off | default | 0.115 | 0.114 | 0.45% | 0.02% / 0.01% |
| on, all | default | 0.346 | 0.313 | 5.65% | 0.06% / 0.01% |
| on, title | default | 0.228 | 0.228 | 0.45% | 0.03% / 0.02% |
| on, title+entities | default | 0.165 | 0.164 | 3.2% | 0.04% / 0.01% |
| off | session | 0.117 | 0.116 | 0.65% | 0.01% / 0.01% |
| on, all | session | 0.320 | 0.293 | 2.5% | 0.02% / 0.00% |
| on, title+entities | session | 0.156 | 0.157 | 1.4% | 0.01% / 0.01% |

Sampling: 200,000 random cross-note pairs, all within-note pairs, top-1 over
2,000 sampled facts (`/mnt/bulk/lab-work/header/neardup.py`). The within-note
clustering is the failure mode the brief predicted: a shared note-level header
raises the top-1 same-note share from under 1% to 2.5% to 5.7%. The
across-note lift from shared tokens (common to all facts) is about 0.2 for the
full header.

## Verdict

**KEEP BEHIND FLAG.**

- Gain exists only where alias links exist: session corpus at 10k, plain gold
  0.935 to 0.985 (+10 queries) with all fields, and 0.990 with title+entities.
  That is beyond the 1 to 3 query noise band, but it is one synthetic corpus
  with one seed, and the session scope is the one the corpus was built to
  exercise.
- No gain on the default corpus at any size (within noise, 0.69 to 0.72).
- Title alone does nothing. The corpus titles are all `Memory`, so this is not
  a test of title-carried surnames; the survey's expectation that the title would
  carry the surname does not hold for this corpus.
- Date adds nothing measurable (0.990 vs 0.985 is one query).
- Cost: index +4.6%, a visible cold-start increase, and a large change to the
  vector space (every fact's cosine to every other rises), with more
  near-duplicate clustering inside a note.

Graduation needs a real vault and a corpus where facts use first names and the
surname lives only in alias evidence, and a check of the within-note clustering
on that vault.

## Limits

- Synthetic corpus, one seed. n=200 queries per cell.
- The measured sizes do not reach the FTS lexical arm; the header is not in FTS.
- The alias-link case is the only one where the header helps, and it depends on
  the linker resolving the short form. Linked-entity rows are refreshed on flush.
- Latency numbers are under shared load (load average up to 22). No claim of a
  recall-latency change is made beyond "not clearly different".
- The `header` side table is created lazily; an index built with the flag off
  and queried with it on falls back to text-only keyword matching and does not
  re-embed (no re-index happens on flag change).
