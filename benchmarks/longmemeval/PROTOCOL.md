# LongMemEval protocol — pre-registered

Frozen before the first scored run. Companion to the LoCoMo study
(`../locomo/`) — second dataset, same reader, same judge, same integrity
rules; anything not specified here follows the LoCoMo protocol.

**Dataset**: [LongMemEval](https://github.com/xiaowu0162/LongMemEval)
(ICLR 2025), `longmemeval_s`: 500 questions, each with its own haystack of
~50 chat sessions (~115k tokens) mixing evidence sessions into distractors.
Fetched from Hugging Face at run time; never committed.

**Questions**: the 30 abstention questions (`*_abs`) are excluded, matching
the LoCoMo category-5 exclusion. From the remaining 470 we draw a stratified
sample of **n = 200** proportional to `question_type`, `random.seed(42)`,
committed to `results/questions.jsonl`. The question's stated ask-date is
part of the prompt in **every** condition (several types need "as of when
was this asked").

**Conditions** (same information enters each):

| condition | context given to the reader |
|---|---|
| `none` | nothing |
| `grimoire-local` | Grimoire retrieval with the optional local model2vec embedder (`pip install model2vec`, zero config beyond that) |
| `grimoire-ollama` | Grimoire retrieval with `GRIMOIRE_OLLAMA_URL` + `nomic-embed-text` |
| `full` | the entire haystack transcript |

Each question's haystack is ingested into a fresh vault, one note per
session, titled with the session date (the LoCoMo round-3 convention),
turns speaker-labelled `User:` / `Assistant:`. Retrieval = the product's
`index.retrieve` top-10 + `/api/search` top-5 with `full=true`, raw
question text, no rewriting.

**Reader**: `claude-haiku-4-5`, one shared prompt template (empty context
block for `none`). **Judge**: `claude-sonnet-5`, strict v2 prompt, blind to
condition. NOTE: this is *not* the official LongMemEval judge — absolute
numbers are not comparable to the paper's leaderboard; comparisons between
the conditions in this table are.

**Metrics**: judge accuracy overall and per `question_type`; reader input
tokens per condition (context cost = median minus the `none` baseline).

**Integrity**: same rules as LoCoMo — no benchmark-aware product code, all
rounds reported, raw per-question outputs committed.

---

## Amendment — round 6 (2026-08-18)

Two candidate product changes were scored under the frozen sample, prompts,
reader and judge; both were reverted. The amendment is kept because the round
changed how this study must be run.

### New condition: `control-sameepoch` — required from now on

A byte-for-byte copy of an existing condition's contexts, re-read and
re-judged in the same session as any new candidate. It exists because rounds
1–5 compared fresh reads against *stored* reads from earlier runs, and the
reader and judge are sampled, not deterministic: on identical input this
control flipped 8.0% of answers here and 12.0% on LoCoMo, moving accuracy by
1.1 and 3.6 points respectively. A 2–4 point difference between two retrieval
variants therefore cannot be attributed to the variant without it.

**Any future round comparing retrieval variants must include a same-epoch
control, and must gate on the deterministic dev metrics first.**

### Candidates scored

1. `grimoire-go-cap` — `finalize` selects in score order with a per-note cap
   of half the requested k, instead of emitting the best chunk of every note
   before any note's second chunk.
2. `grimoire-chunk400` — `ChunkTarget` 800 → 400, leaving the selection rule
   alone.

Results and the reasoning are in [REPORT.md](REPORT.md). Neither reached
significance on either dataset, and the cap regressed LoCoMo multi-hop, so
neither shipped.

### How the candidates were found

Recorded because the diagnosis holds even though the remedies did not:

1. Attributing round-5's 51 errors against `full` (which has every piece of
   evidence by construction) split them into 23 that full context answered and
   we did not, and 28 that neither answered.
2. Reading the 14 multi-session questions in that second group showed almost
   all were enumerative ("how many X did I ..."), with Grimoire *undercounting*
   — 4 of 5 kitchen repairs, 3 of 4 festivals, 2 of 3 plants. Full context
   undercounted too.
3. A held-out dev split was built from the 270 questions that are neither
   abstention items nor in the scored sample, measured with an LLM-free
   evidence-TURN recall harness (`dev_recall.py`). Session-level coverage was
   measured first and was misleading at 94.8%: a chunk from the right session
   is not the chunk holding the fact.
4. The obvious control ran first — larger k does fix coverage, but only at
   k=100, costing 82k characters against 22k, which forfeits the reason to
   retrieve at all.
5. The rank distribution explained why: coverage was flat from rank 10 to 30
   (54.2% → 55.7%) then jumped to 92.4% by rank 75, and **100%** of evidence
   ranked at or beyond 30 was a later chunk of an already-seen note, the first
   of which arrived at mean rank 49.5 against 49.6 sessions per haystack.
   Per-note de-duplication, not relevance, set the floor.

### Candidates measured and rejected (nulls are reported)

- *Pseudo-relevance feedback / query expansion.* Re-tested specifically because
  its earlier rejection could have been an artifact — before the cap, expansion
  had nowhere to put what it found. It is not an artifact: at equal budget,
  expanded-only scored 62.2% and query+expanded 68.1% against 75.2% for the
  query alone.
- *Note-level prior.* Motivated by the finding that 65% of the evidence still
  missed after the cap sat in a note already in the top-10. Giving each note a
  prior equal to the sum of its best few chunk scores hurt monotonically:
  83.3% → 82.6% → 81.5% → 81.1% as the weight rose, while breadth fell from
  91.5% to 87.4% session coverage.
- *Wider small-to-big merge.* Merging the top 6 hits with their neighbours
  instead of the top 3 reached 84.1% coverage for 25.5k characters, while
  simply raising k to 15 reached 85.9% for 25.3k. Dominated at equal budget.
- *Per-note cap and smaller chunks* — scored, null, reverted; see REPORT.md.

## Amendment — round 8 (2026-08-18)

**Replication sample.** `results_ext/` holds the 270 LongMemEval questions
that are neither abstention items nor in the frozen scored sample — everything
the study had never scored. A candidate that fails significance on the frozen
200 is re-scored there before it is either shipped or abandoned, against a
control retrieved with the unmodified binary and read in the same session.
Both samples are reported, and the pooled figure covers all 470 questions.

Disclosed: those 270 questions were the dev split used to choose the
candidate's one parameter (three chunks per hit, over a grid of two and three).
The replication is therefore a fresh ACCURACY measurement — accuracy was never
measured on them — but not a fully clean holdout. The direction does not depend
on that choice: two chunks per hit also improved dev coverage (75.6% → 78.1%).

`LME_RESULTS` overrides the results directory so a second sample can be scored
without touching the frozen one.
