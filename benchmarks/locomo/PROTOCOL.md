# LoCoMo benchmark protocol — pre-registered

Frozen before the first scored run. Any later change to this file must be
accompanied by a re-run of every condition it affects.

## What is being measured

Whether Grimoire's retrieval substrate — the same code paths its MCP agent
tools (`search_notes`, `ask_notes`) call — lets a fixed reader model answer
questions about long conversations better than having no memory at all, and
how close it gets to the full-context upper bound, at what token cost.

**Dataset**: [LoCoMo](https://github.com/snap-research/locomo) (Maharana et
al., ACL 2024) — 10 very-long multi-session conversations, 1,986 QA pairs.
The de-facto public benchmark for agent-memory systems (used by Mem0, Zep,
Letta). The dataset is fetched at run time and never committed to this repo.

**Questions**: categories 1–4 (multi-hop, temporal, open-domain, single-hop);
category 5 (adversarial/unanswerable) is excluded, following the practice of
published memory-system evals on this dataset. From the 1,540 remaining
questions we draw a stratified random sample of **n=500**, proportional to
category size, `random.seed(42)`. The sample is written to
`results/questions.jsonl` and committed, so the exact question set is
reproducible and auditable.

## Conditions

Identical information enters every condition: the same per-session transcript
text (speaker-labelled turns; image turns rendered as
`[shared a photo: <blip_caption>]`; the session's date/time header included).

| condition | context given to the reader |
|---|---|
| `none` | nothing (guessability floor) |
| `grimoire` | what Grimoire retrieval returns, as shipped zero-config (hashing embedder): top-10 chunks from `/api/retrieve` + top-5 `/api/search` FTS hits for the raw question |
| `grimoire-local` | same, with the optional local model2vec embedder (`pip install model2vec`; added in round 5) |
| `grimoire-ollama` | same, with the one-env-var supported config `GRIMOIRE_OLLAMA_URL` + `nomic-embed-text` embeddings |
| `full` | the entire conversation transcript (upper bound) |

For the grimoire conditions each conversation is ingested into a **fresh
vault** (one note per session) and queried with the **raw question text** —
no query rewriting, no hints, no benchmark-specific preprocessing. Retrieval
uses the same functions the product serves (`index.retrieve`,
`routers.search.search`).

*Disclosed ingestion change in round 3*: session notes are titled with their
session date (`Conversation session 3 (1:56 pm on 8 May, 2023)`), the way a
person titles a meeting log — in rounds 1–2 the date appeared only in the
note body. This is an experiment-design variable, not product code; it is
reported as its own round so its effect is attributable.

*Round 5* adds the `grimoire-local` condition (optional `model2vec`
static-embedding backend, auto-detected when installed; the index re-embeds
on backend change). No other condition re-ran — no code affecting them
changed since round 4.

*Round 4* changes product code only (ingestion identical to round 3):
BM25 term-frequency saturation in the lexical leg, and small-to-big
retrieval (top hits return their neighbouring chunks merged). Candidate
changes that failed the dev gate — pseudo-relevance feedback, a
bigram/sublinear-tf hashing embedder, cosine-leg query expansion — were
reverted and are reported as rejected, per the tuning rule.

## Reader and judge

- **Reader**: `claude-haiku-4-5`, one prompt template shared verbatim by all
  four conditions (the context block is simply empty for `none`). Single
  turn, no tools. The reader is forced to answer (no abstention), since every
  sampled question is answerable.
- **Judge**: `claude-sonnet-5`, blind to condition, sees only
  (question, gold answer, model answer), returns strict JSON
  `{"correct": bool}`. One judgment per answer.
  - *Amendment (judge v2), before any cross-round comparison was made*: a
    spot audit of round-1 judgments found the v1 prompt marking hedged
    non-answers ("not specified", "no explicit mention") as correct. v2
    requires the answer to affirmatively state the gold information. Per the
    re-run rule, **every** round is judged under v2; no v1 numbers are
    reported anywhere.
- Both run via the Claude Code CLI from an empty working directory with
  `--strict-mcp-config --max-turns 1` so no ambient project context leaks in.

## Metrics

1. **Accuracy** (judge-marked correct), overall and per category.
2. **Context cost**: reader input tokens per question (from CLI usage),
   per condition.

## Integrity rules

- Protocol, prompts, sample seed frozen before round 1.
- Improvement iterations may only change **generic product code** committed
  to the product code — nothing may read benchmark data, special-case question
  shapes, or ship benchmark-only configuration.
- Every round's raw per-question results are kept in `results/` and every
  round is reported, including regressions and nulls.
- Published claims compare only runs executed under this protocol on this
  hardware; numbers from other papers are context, never a comparison row.

---

## Amendment — round 6 (2026-08-18)

Two LongMemEval-derived candidates were scored here because the tuning rule
requires a re-run of every condition a product change affects. Both were
reverted; see [REPORT.md](REPORT.md).

The round also adds a **same-epoch control** — a byte-identical copy of an
existing condition's contexts, re-read and re-judged alongside the candidates.
It is now required for any comparison between retrieval variants: on identical
input it flipped 12.0% of answers here and moved accuracy 3.6 points, which is
larger than either candidate's effect. Comparing a new run against stored reads
from an earlier round, as rounds 1–5 did, silently includes that variance.

`dev_coverage.py` additionally reports the number of distinct sessions in the
context. That column exists because its absence cost a scored run: the per-note
cap left evidence coverage flat on this dataset (84.5% → 84.6%) while costing
multi-hop accuracy, and breadth was the thing that had moved. Coverage alone is
not a sufficient gate for LoCoMo.

## Amendment — round 8 (2026-08-18)

**Extension sample.** `results_ext/` holds a 500-question stratified sample
(`random.seed(7)`) of the 1,040 questions outside the frozen set and outside
category 5. It exists because round 6 measured this dataset's noise floor at
12% of answers, which makes a 1–2 point difference unresolvable at n = 500;
pooling the two samples gives n = 999. `LOCOMO_RESULTS` overrides the results
directory so the frozen sample is never touched.

## Amendment — round 9 (2026-08-18)

Adds the `dense-only` and `lexical-only` baselines (see
[../longmemeval/PROTOCOL.md](../longmemeval/PROTOCOL.md)) and requires that
every condition in a reported comparison — `full` included — be read and judged
in the same session as the others. Applying that rule withdrew this study's
parity-with-full-context claim; see REPORT.md.

## Amendment — round 10 (2026-08-18)

Adds the `corpus-fits` condition: contexts from `/api/context`, which returns
the whole corpus when it is under a character budget and retrieves when it is
not. Budget 150,000 characters for the scored run, chosen to sit above LoCoMo's
conversations (~96k) and far below LongMemEval's haystacks (~470k), so the same
binary exercises both branches.

The LongMemEval side is verified by CONTEXT IDENTITY rather than by scoring:
if the endpoint returns byte-identical strings to plain retrieval, the reader
and judge cannot produce different numbers, and re-running them would only add
sampling noise to a comparison that is already exact.
