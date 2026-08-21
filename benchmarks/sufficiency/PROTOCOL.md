# Sufficiency probe — protocol

Pre-registered before the reader run. Frozen; later changes must be noted
here with the run they affect.

## The question

Can a retrieval layer tell an agent whether the context it just returned is
enough to answer the question?

This is not the same question as "is the context relevant", and the
difference is the whole point. A relevance score says how close the query and
the corpus are. Answerability is whether the corpus *states the asked-for
fact*. Every production RAG stack conflates them, because the only signal
most of them expose is a similarity score.

## Why this data

LoCoMo ships **446 category-5 questions**: adversarial, unanswerable, and
excluded from every published memory-system evaluation — including our own
LoCoMo runs — because they have no gold answer to score against. They are not
nonsense. They name real people and real topics from the conversation and ask
for a fact that was never stated:

> *What type of individuals does the adoption agency Melanie is considering support?*

That makes them the sharpest available test of relevance-versus-sufficiency,
and an entirely unused one.

## Why not the accuracy harness

The LoCoMo and LongMemEval studies can no longer resolve differences of the
size being tested: 8–12% of answers flip on byte-identical input from reader
and judge sampling alone (see `../locomo/REPORT.md` round 6). Both measures
here avoid that floor:

- the retrieval probe involves **no reader and no judge** — it is a
  deterministic function of the index;
- the reader probe scores a **parsed verdict token**, not a judged answer,
  so there is no judge variance in it. Reader sampling remains, and is
  reported as such.

## Conditions

**Retrieval probe** (`probe.py`): each conversation is ingested into a fresh
vault, one note per session, exactly as the LoCoMo harness builds it. Each
question is sent to `/api/retrieve?k=10` as raw text. Recorded per query:
`top_cosine`, `max_cosine`, `mean_cosine`, `cosine_gap`, `top_lexical`,
`max_lexical`, `lexical_gap`, `legs_agree`, `top_score` (the fused RRF value),
`score_margin`, `score_spread`, IDF-weighted `term_coverage`,
`rare_term_coverage`, `n_query_terms`.

**Reader probe** (`abstain.py`): the same vaults, questions sent to
`/api/ask` — the shipped path, default prompt, no benchmark-specific wording.
Reader is the product default (`qwen3.5:4b` over Ollama). Recorded: the parsed
`supported` verdict, and whether the free-text answer matches a generous
prose-abstention regex.

**Sampling**: balanced per conversation — the same number of answerable
(categories 1–4) and unanswerable (category 5) questions from each, taken in
dataset order. Balance is deliberate: the class prior is a property of LoCoMo,
not of the signal, and an unbalanced probe lets a constant predictor look good.

## Metric

**AUC** (Mann-Whitney): the probability that a random answerable question
scores above a random unanswerable one. It needs no threshold, is invariant to
the class prior, and 0.5 is a coin flip. Reported for every signal.

For the reader verdict, also the two rates a caller actually cares about:
**abstention rate on unanswerable** (how often it correctly refuses) and
**answer rate on answerable** (how much it gives up to get that).

## Baselines

1. **Retrieval threshold** — the field's standard "is the top similarity high
   enough". Measured, not assumed.
2. **Prose abstention** — what a caller had before this change: string-matching
   the free-text answer for "the notes don't say". The regex is deliberately
   generous, so the baseline is measured at its best.

## Declared in advance

- A signal is only interesting at **AUC ≥ 0.60**, and an inverted signal
  (AUC ≤ 0.40) will **not** be shipped as a reversed rule. LoCoMo's adversarial
  questions were *constructed* from conversation vocabulary; a rule that reads
  "strong lexical match means we probably cannot answer" would fit that
  artifact and be actively harmful on real questions.
- The reader verdict is judged on both rates together. Refusing everything
  scores 100% abstention and is worthless.
