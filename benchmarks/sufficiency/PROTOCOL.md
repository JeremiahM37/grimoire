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

## Amendments

Both were made before any reported number, and both because the run as first
written measured something other than what this document says it measures.

**A1 — stratified sampling (2026-08-20).** "Taken in dataset order" was wrong.
The file is not ordered randomly: taking the first N answerable questions per
conversation caught **4 single-hop questions out of 249**, of the category that
is 55% of LoCoMo. Single-hop is the one category where the retrieval scores are
not inverted, so excluding it made the finding look about twice as strong as it
is. Both probes now sample the answerable side stratified by category with
`random.seed(42)` — the seed the LoCoMo harness freezes — and both report a
per-category breakdown. The first, unstratified numbers were published and are
corrected in the report rather than removed.

**A2 — the reader probe was not measuring retrieval (2026-08-20).**
`/api/ask` reads the WHOLE corpus instead of retrieving when the corpus fits a
100k-character budget, and a LoCoMo conversation sits right at that boundary.
An unknown fraction of questions were therefore answered from the full
transcript with no retrieval involved — long-context, not RAG, and with no
retrieval to judge. `GRIMOIRE_CONTEXT_BUDGET=0` now pins every question to the
retrieval path, and the mode is recorded per question so the report can show it
rather than claim it.

**A3 — verdict parsing (2026-08-20).** The first parser required the verdict
line to end after `yes`/`no`. Models write `SUPPORTED: no — the notes mention
X but never state Y`, so 31% of real replies went unparsed and were scored as
"did not refuse". That was a parser bug being counted as a model failure; the
pattern now accepts trailing text and keeps it, since it is the most useful
sentence in the reply. Fixed before the reported run.
