# No retrieval score knows when it failed

*498 questions, 14 signals, a threshold everyone ships, and a pre-registered bar
none of them clear.*

Almost every RAG system in production has a number it uses to decide whether it
found the answer. Usually it is a floor on the top result's cosine similarity.
Below the line, abstain or escalate; above it, answer. The number is tuned by
hand, on a handful of examples, and then trusted for the life of the product.

We tested fourteen of those numbers against 498 questions, half of which have no
answer in the corpus at all. The pre-registered bar was AUC 0.60 — a low bar,
barely better than a coin flip.

**Nothing reached it. The most widely deployed heuristic in the field lands at
0.550, and half the signals score below chance.**

The interesting part is not the failure. It is *why* it fails, which turns out
to be structural, and which predicts exactly which questions the technique will
mislead you on.

---

## The measurement

The unanswerable questions come from LoCoMo's category 5: adversarial questions
built to sound answerable about a conversation that never covers them. They are
excluded from essentially every published memory-system evaluation — including,
until this study, our own.

The answerable half is sampled from the same conversations, stratified by
category with a fixed seed so the category mix matches the dataset's. 249 of
each. Every question goes through the same retrieval call, `k=10`, and we record
every statistic the ranker can see.

AUC here is the probability that a random answerable question scores above a
random unanswerable one. 0.5 is a coin flip. 1.0 is a perfect detector.

| signal | answerable | unanswerable | AUC |
|---|---|---|---|
| `max_cosine` | 0.556 | 0.534 | 0.581 |
| `mean_cosine` | 0.454 | 0.432 | 0.581 |
| **`top_cosine`** — *the standard threshold* | 0.540 | 0.525 | **0.550** |
| `term_coverage` (IDF-weighted) | 0.895 | 0.894 | 0.527 |
| `n_hits` | 10.0 | 10.0 | 0.500 |
| `rare_term_coverage` | 0.896 | 0.904 | 0.492 |
| `cosine_gap` | 0.048 | 0.051 | 0.484 |
| `score_margin` | 0.0074 | 0.0075 | 0.472 |
| `legs_agree` | 0.458 | 0.546 | 0.456 |
| `n_query_terms` | 5.63 | 5.87 | 0.450 |
| `top_score` (fused RRF) | 0.0326 | 0.0328 | 0.448 |
| `top_lexical` | 11.93 | 13.12 | 0.431 |
| **`max_lexical`** — *best chunk's BM25* | 12.76 | 14.07 | **0.415** |
| `lexical_gap` (1st − 2nd) | 4.21 | 5.32 | 0.414 |

The entire field spans 0.414 to 0.581. The lexical signals are not merely
uninformative — they are *inverted*. A high BM25 score on the best-matching
chunk is weak evidence the question is **un**answerable.

## Why: proximity is not answerability

Split the answerable side by question type and the noise resolves into a
pattern:

| answerable category | n | `top_cosine` AUC | `max_lexical` AUC |
|---|---|---|---|
| single-hop | 138 | 0.617 | 0.522 |
| temporal | 53 | 0.507 | 0.354 |
| multi-hop | 45 | 0.437 | **0.192** |
| open-domain | 13 | 0.414 | 0.292 |

Retrieval scores carry real signal on exactly one kind of question — the direct
lookup, where "the answer sits in a chunk that looks like the question" is
literally true — and they **invert** on every kind that requires synthesis. On
multi-hop questions the best chunk's BM25 reaches 0.192: the distractor beats
the real question five times out of six.

You can see it in the questions themselves. The unanswerable ones that score
highest:

> *How does Deborah plan to involve local engineers in her idea of teaching STEM
> to underprivileged kids?* — BM25 **34.73**, cosine **0.661**

And the answerable ones that score lowest:

> *What is Caroline's relationship status?* — BM25 **2.18**
> *What are John's suspected health problems?* — BM25 **3.33**, cosine **0.265**

A well-formed unanswerable question is built **out of the corpus's own
vocabulary**. It names the people, places and projects that were discussed and
asks for the one detail that was not. A real question paraphrases, and often
asks for something the notes only imply.

Stated generally:

> **A retrieval score measures proximity between the query and the corpus.
> Answerability is whether the corpus states the asked-for fact. The two
> coincide only for lookups.**

That is the whole result. Everything below is an attempt to break it.

## It is not one embedder

Repeating the entire probe with a zero-dependency hashing embedder instead of a
learned one:

| | learned (model2vec) | hashing |
|---|---|---|
| `top_cosine` | 0.550 | **0.496** |
| `max_cosine` | 0.581 | 0.490 |
| `max_lexical` | 0.415 | 0.405 |
| range across all signals | 0.414–0.581 | 0.392–0.506 |

Flatter still. The hashing embedder's `top_cosine` is a coin flip to three
decimals. Whatever separates answerable from unanswerable, neither embedding
space encodes it.

## It is not that we needed to combine them

The obvious objection to a table of single signals is that some *combination*
would work. Plain logistic regression over thirteen of them, split **by
conversation** — not by question, because these statistics are partly properties
of the conversation, and a question-level split would measure memorisation:

| | AUC |
|---|---|
| learned combination, training conversations | 0.651 |
| learned combination, **held-out conversations** | **0.564** |
| best single signal, same held-out set | 0.395 (inverted) |

0.651 where it was fitted, 0.564 where it was not — still under the bar. The
hashing embedder gives 0.656 / 0.549, the same shape.

**The gap between fitted and held-out is the finding.** What little signal
exists is specific to the conversation it was tuned on, not a general property
of retrieval. Which is also a precise description of how these thresholds get
set in practice: tuned on the corpus in front of you, deployed against corpora
that are not.

## The correction we have to report

The first version of this table gave `max_lexical` 0.277 and `top_cosine` 0.439,
and it was committed and wrong. Those numbers came from taking the first N
answerable questions per conversation in file order, which is not random: it
caught **4 single-hop questions out of 249**, of the category that is 55% of the
dataset. Single-hop is the one category where the scores are *not* inverted, so
excluding it made the inversion look far stronger than it is.

The re-run above is stratified with a fixed seed. The conclusion is unchanged
and the mechanism is better supported — but the headline figures were wrong, and
they were wrong **in the direction that flattered the finding**, which is the
direction to be most suspicious of.

---

## Why you should believe this table when you shouldn't believe most RAG numbers

Including ours.

We built a control into our own accuracy benchmark: re-run a condition against
**byte-identical retrieved context**, re-read it, re-judge it, change nothing
else. On LoCoMo, 60 of 500 answers (12.0%) flipped. Accuracy moved 3.6 points.
McNemar rated that difference significant at p = 0.027 — against nothing at all.
On LongMemEval the same control flipped 8%.

The honest consequence, which we published about our own work: **every
cross-round comparison in our history smaller than that is unresolvable by that
harness.** Most product comparisons in this field are smaller than that, and
most are run once, self-judged, with no variance estimate at all.

That is exactly why this study is built the way it is. **Section 1 involves no
reader and no judge.** It is a retrieval probe. Re-running it against the
committed binary regenerates all 498 records byte-identically, on both
embedders. There is no sampling anywhere in it, so there is no noise floor to
clear — the numbers are a property of the ranker, reproducible in the strongest
sense available.

The tables above are not asking for the same trust the accuracy tables are. They
do not need it.

*(Three further results from the same discipline, for calibration on what we are
willing to publish about ourselves: our answering endpoint was retrieving via a
path **no published number had ever measured** — every benchmark described a
different code path than the one users got, and we flipped the default after
measuring it as no better. One of LongMemEval's six categories turns out to be
scoring **answer format rather than memory**, its golds being rubrics. And on the
one category we have measured it against — knowledge-update, n = 31 — our own
fact-extraction memory pipeline **loses to plain chunk retrieval**, 64–68%
against 83.9%, on the benchmark that pipeline exists to win.)*

---

## So what do you do instead?

The judgement has to come from something that **reads** the context. Nothing at
the retrieval layer can make it — that is what the 498 questions establish.

The cheap way to do that is to put the judgement inside the read that is already
happening: ask the reader for a one-line verdict *before* the answer, in the same
completion. No second model call, no classifier, no cross-encoder — the
alternatives in the literature cost a call per query or per document. The verdict
goes first deliberately: a model that has just written three confident sentences
will rate its own evidence to match them.

On 120 balanced questions, against a 4B local reader:

| signal | refuses unanswerable | answers answerable | balanced |
|---|---|---|---|
| **verdict** | **80.0%** | 45.0% | **62.5%** |
| string-matching the prose | 78.3% | 46.7% | 62.5% |
| best retrieval threshold | — | — | 55.0% |

Two readings of that, and the second one matters:

**A reader does what no retrieval statistic could** — 80% of unanswerable
questions correctly refused, against a retrieval-side family whose best member
barely clears a coin flip.

**But the verdict is not more accurate than a regex over the prose.** 62.5%
against 62.5%; they disagree on three of sixty unanswerable questions, exact
McNemar p = 1.00. Anyone claiming this improves abstention *accuracy* is reading
noise. What it improves is the **contract** — a field with three defined values
instead of a regex over English that is brittle across models, phrasings and
languages, and that fails silently when it breaks. That is an engineering
argument and it should be made as one.

The cost is real: the verdict refuses **55% of answerable questions**, while
evidence-turn recall on the same binary is 76.8%. The evidence usually reached
the context and the reader declined anyway.

That last number is a property of the model, not the design — swapping only the
reader for a 35B mixture-of-experts model moves it:

| reader | refuses unanswerable | answers answerable | balanced |
|---|---|---|---|
| 4B | 80.0% | 45.0% | 62.5% |
| 35B MoE | **95.0%** | **63.3%** | **79.2%** |

Better on both axes at once rather than trading one for the other. Paired: the
large reader refuses 9 unanswerable questions the small one let through and
misses none it caught (p = 0.0039); on answerable questions, 17 refused only by
the small reader against 6 only by the large (p = 0.0347).

## The latency footnote that reframes the whole thing

Ranking cost against corpus size, measured rather than assumed:

| corpus | per query |
|---|---|
| 10k chunks | 2.2 ms |
| 50k chunks | 9.3 ms |
| 200k chunks | 37.6 ms |

A reader call averages ~7 seconds. On a 200k-chunk corpus, ranking is **0.4% of
the time an agent spends answering a question.** Making retrieval twice as fast
is invisible.

The available win is the other shape entirely: **not answering faster, but not
making a doomed reader call at all.** A grounded/ungrounded verdict tells an
agent to stop, ask elsewhere, or say the corpus does not know — before it spends
eight seconds and a context window producing a confident wrong answer out of
on-topic passages.

Which is the practical form of the finding. You cannot know from the scores. You
can know from the read. Ask for it in the read you are already paying for.

---

## Limitations

- One dataset. LoCoMo's category-5 questions are adversarial *by construction*;
  naturally occurring unanswerable questions may separate more easily.
- Two embedders, both small. A large hosted embedding model is untested.
- The signals are the ones this ranker exposes. A cross-encoder or a trained
  calibration head is a different layer and is not what this rules out — what it
  rules out is the statistics you already have.
- Sections 4 and 5 involve a sampling reader and are reproducible only in
  distribution; raw replies are committed. Section 1 is byte-identical.

## Reproduce it

Everything is in
[`benchmarks/sufficiency/`](https://github.com/JeremiahM37/grimoire/tree/main/benchmarks/sufficiency)
— pre-registered protocol, probe, analysis, and the raw 498-record output.
`probe.py` against the committed binary regenerates `signals.jsonl` exactly.

It came out of [Grimoire](https://github.com/JeremiahM37/grimoire) — a
self-hosted server holding your markdown notes, your agents' memory, and
credentials your agents can *use* without ever reading, behind one MCP mount.
Retrieval is the substrate under all of that, which is why it gets measured this
hard and why the measurements are not the pitch. Nothing above depends on any of
it: the signals are the ones any hybrid retriever computes, and the question is
one every RAG system answers whether or not it knows it.
