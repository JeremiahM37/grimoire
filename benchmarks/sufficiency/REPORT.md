# Sufficiency: can retrieval tell you when it has not found the answer?

Method: [PROTOCOL.md](PROTOCOL.md), pre-registered. Data: LoCoMo's 446
category-5 questions — adversarial, unanswerable, and excluded from every
published memory-system evaluation including our own — against a balanced
sample of answerable ones.

Neither measure here goes through the accuracy harness, which can no longer
resolve differences of this size: 8–12% of its answers flip on byte-identical
input (`../locomo/REPORT.md`, round 6). The retrieval probe involves no reader
and no judge at all.

## 1. No retrieval statistic can do it

498 questions (249 answerable / 249 unanswerable), `/api/retrieve?k=10`,
model2vec embedder. The answerable side is sampled stratified by category with
`random.seed(42)`, so its category mix matches LoCoMo's. AUC is the probability
that a random answerable question scores above a random unanswerable one; 0.5
is a coin flip. The protocol declared 0.60 as the bar for "interesting" before
any of this was run.

| signal | answerable | unanswerable | AUC |
|---|---|---|---|
| `max_cosine` | 0.556 | 0.534 | 0.581 |
| `mean_cosine` | 0.454 | 0.432 | 0.581 |
| **`top_cosine`** (the standard threshold) | 0.540 | 0.525 | **0.550** |
| `term_coverage` (IDF-weighted) | 0.895 | 0.894 | 0.527 |
| `n_hits` | 10.0 | 10.0 | 0.500 |
| `rare_term_coverage` | 0.896 | 0.904 | 0.492 |
| `cosine_gap` | 0.048 | 0.051 | 0.484 |
| `score_margin` / `score_spread` | 0.0074 | 0.0075 | 0.472 |
| `legs_agree` | 0.458 | 0.546 | 0.456 |
| `n_query_terms` | 5.63 | 5.87 | 0.450 |
| `top_score` (fused RRF) | 0.0326 | 0.0328 | 0.448 |
| `top_lexical` | 11.93 | 13.12 | 0.431 |
| **`max_lexical`** (best chunk's BM25) | 12.76 | 14.07 | **0.415** |
| `lexical_gap` (1st − 2nd) | 4.21 | 5.32 | 0.414 |

**Nothing reaches 0.60. Nothing gets close.** The whole field spans 0.414 to
0.581 — the most widely deployed sufficiency heuristic in production RAG, a
floor on the top result's cosine, lands at 0.550, and the lexical signals are
below a coin flip.

### The structure underneath, which is the real finding

Splitting the answerable side by question type shows the scores are not
uniformly useless. They are useful for exactly one kind of question:

| answerable category | n | `top_cosine` AUC | `max_lexical` AUC |
|---|---|---|---|
| single-hop | 138 | 0.617 | 0.522 |
| temporal | 53 | 0.507 | 0.354 |
| multi-hop | 45 | 0.437 | **0.192** |
| open-domain | 13 | 0.414 | 0.292 |

A retrieval score can tell you whether the words are there. So it carries a
little signal when the question is a **direct lookup** — single-hop, the one
category where "the answer is in a chunk that looks like the question" is
literally true — and it **inverts** on every type that needs synthesis. On
multi-hop questions the best chunk's BM25 reaches 0.192: the corpus-vocabulary
distractor beats the real question five times out of six.

That is visible in the questions. The unanswerable ones retrieval scores
highest:

> *How does Deborah plan to involve local engineers in her idea of teaching STEM to underprivileged kids?* — BM25 34.73, cosine 0.661

and the answerable ones it scores lowest:

> *What is Caroline's relationship status?* — BM25 2.18
> *What are John's suspected health problems?* — BM25 3.33, cosine 0.265

A well-formed unanswerable question is built **out of the corpus's own
vocabulary** — it names the people, places and activities that were discussed
and asks for the one detail that was not. A real question paraphrases, and
often asks for something the notes only imply. Stated generally: **a retrieval
score measures proximity between the query and the corpus; answerability is
whether the corpus states the asked-for fact.** They coincide only for
lookups.

### Nor does any combination of them

The obvious objection to a table of single signals is that a *combination*
might work. It does not. `combine.py` fits plain logistic regression over
thirteen of them and tests it split **by conversation** — not by question,
because these signals are partly properties of the conversation (how long its
sessions are, how much vocabulary its questions share with it), so a
question-level split would measure memorization of the conversation rather
than generalization to a new one.

| | AUC |
|---|---|
| learned combination, training conversations | 0.651 |
| learned combination, **held-out conversations** | **0.564** |
| best single signal on the same held-out set | 0.395 (inverted) |

It reaches 0.651 where it was fitted and 0.564 where it was not — still under
the bar, and the gap is the finding: what little signal exists is
conversation-specific rather than a general property of retrieval. There is no
threshold, no weighting and no learned rule over these statistics that tells
an answerable question from an unanswerable one.

### Correction: the first run of this table was sampled wrong

The first version of section 1 reported `max_lexical` at AUC 0.277 and
`top_cosine` at 0.439, and was committed. Those numbers came from taking the
first N answerable questions per conversation in file order, which is not
random: it caught **4 single-hop questions out of 249**, of the category that
is 55% of LoCoMo. Because single-hop is the one category where the scores are
NOT inverted, excluding it made the inversion look far stronger than it is.

The re-run above is stratified with a fixed seed. The conclusion is unchanged
and the mechanism is better supported — nothing reaches the declared bar,
and the per-category table is a stronger claim than the single number was —
but the headline figures were wrong and they were wrong in the direction that
flattered the finding, which is the direction to be most suspicious of.

## 2. What the fused score is, and why it now travels with its parts

`Hit.Score` is a reciprocal-rank value, `1/(60+rank)` summed over the two
legs. Rank 0 is rank 0 whether the chunk answers the question exactly or is
the least bad of ten poor matches — which is why `top_score` above is
essentially constant (0.0326 vs 0.0328) and why no caller could ever have
built a relevance floor on it.

`/api/retrieve` now returns each hit's `cosine` and `lexical` alongside it.
That does not solve sufficiency — section 1 is the evidence that nothing at
this layer does — but it makes the ranking inspectable, which is the same
argument the memory engine's score breakdown already makes: a ranking nobody
can inspect is one nobody can fix.

## 3. The verdict that does work, and costs nothing

Since the judgement has to come from something that reads the context, the
cheapest place to put it is inside the read that already happens. `/api/ask`
now asks its reader for a one-line verdict **before** the answer, in the same
completion, and returns it as `supported: grounded | ungrounded | unknown`.

- No second model call, no classifier, no cross-encoder — the alternatives in
  the literature cost a call per query or per document.
- The verdict comes first on purpose: a model that has just written three
  confident sentences rates its own evidence to match them.
- The prompt says *being on-topic is not support*, because section 1 is
  precisely the failure of on-topic-ness as a proxy.
- No verdict line parses as `unknown`, never as grounded. A model failing the
  format is not evidence that the notes contained the answer, and the
  extractive offline floor reports `unknown` because it quotes passages rather
  than judging them.

Measured on the same questions: section 4.

## 5. Aside: retrieval latency is not the bottleneck

Worth measuring before optimizing it. Ranking cost against corpus size
(`go test -bench BenchmarkRetrieve -benchtime=50x -count=3`, median of three,
AMD Ryzen AI Max+ 395):

| corpus | per query | allocated |
|---|---|---|
| 1k chunks | 0.40 ms | 78 KB |
| 10k chunks | 2.2 ms | 832 KB |
| 50k chunks | 9.3 ms | 4.8 MB |
| 200k chunks | 37.6 ms | 20 MB |

A reader call in the run above takes **8–10 s**. So on a 200k-chunk vault —
larger than most people's — ranking is **~0.4% of the time an agent spends
answering a question**, and on a normal vault it is a few hundredths of a
percent. Making it twice as fast would be invisible.

Profiling says the remaining cost is ~29% cosine and ~20% sorting, and neither
has a large *exact* win left: the sorts already exploit BM25 sparsity, and the
cosine loop is already a flat arena with precomputed norms across 8 workers.
The lever beyond that is an approximate index, which trades recall for speed
nobody would notice.

The useful latency win is the opposite shape: **not answering faster, but not
making a doomed reader call at all.** An `ungrounded` verdict tells an agent to
stop, ask elsewhere, or say the vault does not know — before it spends 8
seconds and a context window producing a confident wrong answer from
on-topic passages.
