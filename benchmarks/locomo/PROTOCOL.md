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
  to `server/` — nothing may read benchmark data, special-case question
  shapes, or ship benchmark-only configuration.
- Every round's raw per-question results are kept in `results/` and every
  round is reported, including regressions and nulls.
- Published claims compare only runs executed under this protocol on this
  hardware; numbers from other papers are context, never a comparison row.
