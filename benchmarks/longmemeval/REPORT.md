# LongMemEval results

Method: [PROTOCOL.md](PROTOCOL.md); shared machinery and integrity rules
with the LoCoMo study (`../locomo/`). 200 stratified questions from
`longmemeval_s` (each with its own ~50-session, ~115k-token haystack),
reader `claude-haiku-4-5`, strict blind judge `claude-sonnet-5` (v2). Run
on 2026-07-20 with the round-5 product code — no product changes were made
for or after this run; it is a single-shot validation on a second dataset.

| condition | knowledge-update | multi-session | ss-assistant | ss-preference | ss-user | temporal | **overall** | context tokens* |
|---|---|---|---|---|---|---|---|---|
| none | 3.2% | 0.0% | 20.8% | 7.7% | 7.4% | 7.4% | **6.5%** | 0 |
| grimoire + model2vec | 83.9% | 60.8% | 87.5% | 30.8% | 88.9% | 81.5% | **75.0%** | ~5.9k |
| grimoire + nomic-embed | 77.4% | 60.8% | 95.8% | 15.4% | 85.2% | 79.6% | **73.0%** | ~5.8k |
| full context (~117k tokens) | 80.6% | 54.9% | 100.0% | 30.8% | 85.2% | 68.5% | **70.5%** | ~117k |
| grimoire + model2vec (Go) | 80.6% | 56.9% | 87.5% | 30.8% | 88.9% | 85.2% | **74.5%** | ~6.8k |

\* median reader input tokens minus the `none` baseline (CLI overhead).

The Go row was added on 2026-08-14, when the implementation was ported;
contexts came from a running Go server over HTTP (`retrieve_go.py`), with
the reader and judge phases unchanged and shared. Go vs Python on the same
embedder: 6 wins / 7 losses, exact McNemar p = 1.00 — indistinguishable.

## Reading the numbers

- **Retrieval matches — and directionally exceeds — full context at ~20×
  fewer tokens**: grimoire + model2vec 75.0% vs full 70.5% (30 wins /
  21 losses among discordant pairs, exact McNemar p = 0.26, n = 200). The
  defensible claim is parity at massive compression; the 4.5-point lead is
  a trend, not significance at this sample size.
- The gap pattern inverts vs LoCoMo: here the haystack is ~117k tokens, and
  the reader's needle-finding visibly degrades — full context loses to
  focused retrieval on temporal reasoning (68.5% vs 81.5%) and
  multi-session questions (54.9% vs 60.8%), the two types that require
  connecting facts across a huge transcript. LoCoMo's conversations
  (~24k tokens) fit comfortably, so full context stayed ahead there.
- The two embedders are statistically indistinguishable (p = 0.52); the
  30 MB pip-installable model2vec config needs no external service.
- `none` at 6.5% confirms the questions aren't guessable.
- Single-session-preference is poor in every condition (n = 13; gold
  answers are rubric-like statements the strict judge grades harshly) —
  also the hardest type in the LongMemEval paper.
- Caveat: our judge is not the official LongMemEval judge, so these
  numbers are not comparable to the paper's leaderboard; comparisons
  between the conditions in this table are like-for-like.

## Round 6 — per-note cap and chunk size: both null, and a measurement problem (2026-08-18)

Two candidate product changes were scored here, and the round's main result is
neither of them: it is that **this harness cannot resolve differences of the
size being tested**, and that every previously published cross-round
comparison shares the flaw.

### The noise floor

`control-sameepoch` is a byte-for-byte copy of the `grimoire-go` contexts,
re-read and re-judged in the same session as the new conditions. Identical
input, same frozen reader and judge:

| condition | overall | note |
|---|---|---|
| grimoire-go (round 5 reads) | 74.5% | original run |
| control-sameepoch | 73.4% | **identical contexts**, re-read |

8.0% of answers (16 of 200) flipped on identical input. On LoCoMo the same
control flipped 12.0% (60 of 500) and moved accuracy 3.6 points — enough that
McNemar called it significant (p = 0.027) against nothing but sampling.

Rounds 1–5 compared new reads against *stored* reads from earlier runs, so
every cross-round delta in this file carries that variance on top of whatever
the change did. It does not invalidate the large effects (retrieval vs `none`,
retrieval vs `full`), which are far bigger than the floor; it does mean a
2–4 point difference between two retrieval variants is not measurable at
n = 200 without a same-epoch control, and now there is one.

### The two candidates, measured against that control

| condition | knowledge-update | multi-session | ss-assistant | ss-preference | ss-user | temporal | **overall** | context tokens* |
|---|---|---|---|---|---|---|---|---|
| none | 3.2% | 0.0% | 20.8% | 7.7% | 7.4% | 7.4% | **6.5%** | 0 |
| full context | 80.6% | 54.9% | 100.0% | 30.8% | 85.2% | 68.5% | **70.5%** | ~117k |
| control-sameepoch | 80.6% | 56.9% | 87.5% | 23.1% | 88.9% | 81.5% | **73.4%** | ~6.8k |
| per-note cap | 80.6% | 60.8% | 95.8% | 30.8% | 92.6% | 85.2% | **77.0%** | ~6.7k |
| chunk 800→400 | 83.9% | 64.7% | 83.3% | 23.1% | 96.3% | 83.3% | **76.5%** | **~5.6k** |

\* median reader input tokens minus the `none` baseline.

- **per-note cap**: +3.6pp, 14 wins / 7 losses, **p = 0.189** — not significant.
- **chunk 800→400**: +3.1pp, 16 wins / 10 losses, **p = 0.327** — not
  significant, at **19% less context**.

Both point the right way here. Neither survives the other dataset: on LoCoMo
the cap scored −2.0pp (p = 0.203) and chunk-400 −1.2pp (p = 0.532) against
their same-epoch control, with the cap costing 7.6 points of multi-hop. **Both
were reverted; neither ships.**

### What does hold up

The diagnosis behind the cap is solid and independent of the judge, because it
was measured with no LLM in the loop on a held-out dev split:

- Evidence coverage was flat from rank 10 to 30 (54.2% → 55.7%) and then
  jumped to 92.4% by rank 75.
- **100%** of evidence chunks ranked at or beyond 30 were a later chunk of a
  note whose first chunk had already appeared, and the first such chunk arrived
  at mean rank 49.5 against 49.6 sessions per haystack.
- So per-note de-duplication, not relevance, set the recall floor: a fact
  anywhere but its note's single best-matching chunk was unreachable at
  ordinary k.

Removing that floor raised held-out turn-full coverage 75.6% → 83.3% at equal
context. It simply does not convert into measurable accuracy, and on LoCoMo the
same mechanism actively hurts — the extra same-note chunks that recover a
buried fact also drag in that session's other topics, and its list-answer
questions are graded on precision. The two datasets want opposite things from
a fixed constant.

### Consequence for future rounds

Retrieval changes should be gated on the deterministic dev metrics
(`dev_recall.py`, `../locomo/dev_coverage.py`) — evidence-turn coverage,
full-coverage rate, breadth, and context size — because those have no judge
variance. Where an end-to-end number is wanted, the baseline must be re-read
in the same session as the candidate, as `control-sameepoch` now is. Anything
smaller than roughly 5 points at these sample sizes should be reported as
unresolved rather than as a gain.

### Where the retrieval budget actually goes

Measured on the held-out dev split while looking for a larger lever, and worth
recording because it contradicts where tuning effort had been going. The
context is assembled from two legs — top-k semantic chunks plus top-n
`/api/search` note bodies — and the second one carries most of it:

| legs | turn-full coverage | context chars |
|---|---|---|
| chunks only (k=10, search=0) | **42.6%** | 10.4k |
| k=10 + search=5 (shipped) | **75.6%** | 22.4k |
| k=10 + search=10 | 77.4% | 33.8k |

The semantic chunk leg alone reaches 42.6%; the lexical whole-note leg adds
33 points. Both rounds of tuning above were aimed at the chunk leg — the minor
contributor — which is a large part of why their effects were small.

Reallocating the budget between the legs does not help either. At a matched
~22k characters:

| split | turn-full coverage | context chars |
|---|---|---|
| k=6 / search=6 | 75.6% | 21.8k |
| **k=10 / search=5 (shipped)** | **75.6%** | 22.4k |
| k=3 / search=7 | 74.8% | 22.2k |
| k=15 / search=3 | 73.3% | 21.4k |

The shipped split is already at the optimum, and weighting the excerpt
selector by IDF instead of by how many query terms a passage contains made it
worse (75.6% → 74.1%), so term coverage is the better signal there too.

Six independent changes were measured across this round — per-note cap,
chunk size, query expansion, a note-level prior, a wider small-to-big merge,
and IDF excerpt weighting — and every one was null or negative once both
datasets were considered. Taken with the error decomposition, which found 28
of 51 LongMemEval errors were questions full context also fails, the reading
is that this pipeline is at a local optimum and its remaining errors are not
mostly retrieval-limited.

## Round 7 — four candidates, one trade-off, no net gain (2026-08-18)

Round 6 ended with the diagnosis intact but no shippable change. Round 7 built
three more candidates from it and scored all four against same-epoch controls
on both datasets. The result is a clean structure rather than a win.

| candidate | LongMemEval | LoCoMo | pooled, 700 paired questions |
|---|---|---|---|
| per-note cap | **+3.5pp** (p=0.19) | −2.0pp (p=0.20) | 34W/37L, p=0.81 |
| chunk 800→400 | **+3.0pp** (p=0.33) | −1.2pp (p=0.53) | 45W/45L, p=1.00 |
| within-note excerpt | **+4.0pp** (p=0.10) | −1.2pp (p=0.50) | 37W/35L, p=0.91 |
| …+ relevance gate | 0.0pp (p=1.00) | **+0.6pp** (p=0.77) | 34W/31L, p=0.80 |

Every change that puts more of a NOTE into the response gains three to four
points on LongMemEval and loses one to two on LoCoMo. The one that takes it
back out — admitting a further chunk only if it scores within 85% of the hit
itself — reverses both signs exactly. Pooled across both datasets every
candidate is a coin flip. **None shipped.**

### The trade-off, stated plainly

LongMemEval hides its evidence as asides inside long sessions, so answers fail
by UNDER-inclusion and want depth. LoCoMo spreads its evidence across short
sessions and asks for lists, so answers fail by OVER-inclusion and want focus.
Depth and focus are the same dial turned opposite ways, and this pipeline is
already sitting where the two curves cross.

### The within-note excerpt (the best of the four)

Worth describing because it is the one that came closest and because its
failure is informative. Instead of merging a hit with its chunk_idx ±1
neighbours, a hit brings its note's other high-scoring chunks with it, in
document order with an elision marker — the shape the lexical leg's excerpt
already returns. It keeps ONE entry per note, so breadth is untouched:
distinct sessions per context stayed at 10.96 against the baseline's 10.96.

On the deterministic dev metric it is the only candidate that improves **both**
datasets — LongMemEval turn-full coverage 75.6% → 80.7%, LoCoMo 84.5% → 85.5%,
multi-hop 45.8% → 48.4%. It still lost 1.2 points of LoCoMo accuracy.

### Why coverage stopped being a usable gate

That is the second time on LoCoMo that better evidence coverage came with worse
accuracy — the per-note cap did the same with coverage flat. Two independent
mechanisms, same direction. On this dataset the deterministic metric is not
merely noisy about accuracy, it has been anti-correlated with it, because what
the extra material adds is distractors for a list answer rather than evidence.

So the honest position after seven rounds: retrieval coverage is not a
sufficient gate for LoCoMo, and end-to-end accuracy cannot resolve differences
of this size (the noise floor is 8–12% of answers). A change in this design
space cannot currently be validated either way, which is itself the reason to
ship none of them.

## Round 8 — within-note excerpt, replicated and significant (2026-08-18)

Round 7's best candidate failed significance at n = 200 (p = 0.096). Rather
than accept a trend, it was re-scored on the **270 LongMemEval questions that
had never been scored before** — the remainder of the dataset after the frozen
sample and the abstention items — against a control retrieved with the
unmodified binary and read in the same session.

| sample | n | baseline | within-note excerpt | delta | W/L | exact McNemar |
|---|---|---|---|---|---|---|
| original frozen sample | 200 | 73.5% | 77.5% | +4.0pp | 13/5 | 0.096 |
| replication (never scored before) | 270 | 71.5% | 75.9% | +4.4pp | 20/8 | **0.036** |
| **pooled — all 470 questions** | **470** | **72.3%** | **76.6%** | **+4.3pp** | **33/13** | **0.0045** |

The replication reproduces the original effect almost exactly (+4.4 against
+4.0) and is significant on its own. Pooled over the whole dataset the effect
is **+4.3 points at p = 0.0045**.

Replication per category:

| category | n | baseline | excerpt | delta |
|---|---|---|---|---|
| knowledge-update | 41 | 80.5% | 95.1% | **+14.6pp** |
| multi-session | 70 | 54.3% | 60.0% | **+5.7pp** |
| temporal-reasoning | 73 | 75.3% | 79.5% | +4.1pp |
| single-session-assistant | 32 | 93.8% | 93.8% | 0.0 |
| single-session-preference | 17 | 23.5% | 23.5% | 0.0 |
| single-session-user | 37 | 89.2% | 86.5% | −2.7pp |

Context cost is ~10% higher (7,563 against 6,843 median reader tokens), which
still leaves retrieval at roughly **15× fewer tokens than full context** while
now beating it by six points.

### The change

`finalize` expands each of the top hits into a query-focused excerpt of its
note — the note's other high-scoring chunks, in document order, joined with an
elision marker — instead of merging the hit with its `chunk_idx ±1`
neighbours.

It follows from the round-6/7 diagnosis. The chunk leg emits one chunk per
note, so a fact anywhere but its note's best-matching chunk is unreachable:
chunks-only coverage saturates at 43% whether k is 10, 25 or 30, and evidence
ranked past 30 was a later chunk of an already-seen note in **100%** of cases.
Adjacency merging only helps when the fact happens to sit next door. This is
also the shape the lexical leg already returns, which is why that leg was
carrying most of the pipeline's coverage.

Critically it keeps **one entry per note**, so response breadth is untouched —
10.96 distinct sessions per context against the baseline's 10.96. That is what
separates it from the per-note cap of round 6, which bought the same depth by
spending breadth and cost LoCoMo 20 points of multi-hop.

### LoCoMo: no measurable cost

Round 7 measured −1.2pp here (p = 0.50) and that looked like a real trade-off.
It was not. Re-scored on a further 500 never-scored LoCoMo questions:

| sample | n | baseline | excerpt | delta | W/L | p |
|---|---|---|---|---|---|---|
| frozen sample | 500 | 78.0% | 76.8% | −1.2pp | 24/30 | 0.497 |
| extension (never scored) | 499 | 76.6% | 77.2% | +0.6pp | 22/19 | 0.755 |
| **pooled** | **999** | **77.3%** | **77.0%** | **−0.3pp** | **46/49** | **0.838** |

Pooled multi-hop is **+0.0pp** (54.5% → 54.5%, n = 176). The round-7 multi-hop
worry was the small-sample noise this study now knows to expect.

**Result: +4.3 points on LongMemEval at p = 0.0045, no measurable change on
LoCoMo at n = 999.** Shipped.

## Round 9 — baselines, and every row in one session (2026-08-18)

Two problems with every table above this one: no reference point a reader
already has intuitions about, and rows compared across read epochs. Both are
fixed here. `dense-only` and `lexical-only` are the standard IR baselines run
inside this harness at a matched context budget, and `full-sameepoch` is the
full-context condition re-read alongside everything else.

| method | knowledge-update | multi-session | ss-assistant | ss-preference | ss-user | temporal | **overall** | ctx tokens |
|---|---|---|---|---|---|---|---|---|
| no memory | 3.2% | 0.0% | 20.8% | 7.7% | 7.4% | 7.4% | **6.5%** | 0 |
| dense only (embeddings) | 67.7% | 52.9% | 87.5% | 7.7% | 92.6% | 79.6% | **69.0%** | 7,189 |
| lexical only (BM25) | 71.0% | 54.9% | 83.3% | 15.4% | 92.6% | 79.6% | **70.0%** | 6,443 |
| hybrid, matched budget | 83.9% | 60.8% | 95.8% | 23.1% | 92.6% | 87.0% | **77.5%** | 6,612 |
| hybrid as shipped | 83.9% | 66.7% | 91.7% | 23.1% | 92.6% | 83.3% | **77.5%** | 7,563 |
| full context | 71.0% | 51.0% | 100.0% | 23.1% | 96.3% | 68.5% | **69.0%** | 117,981 |

Paired against the shipped hybrid (exact McNemar, n = 200):

| comparison | delta | W/L | p |
|---|---|---|---|
| vs dense only | **+8.5pp** | 20/3 | **0.0005** |
| vs lexical only | **+7.5pp** | 22/7 | **0.0081** |
| vs full context | **+8.5pp** | 30/13 | **0.0137** |

### What is new here

- **The fusion is worth 7.5–8.5 points over either leg alone**, and it is not
  a budget effect: the matched-budget hybrid spends 6,612 tokens — less than
  dense-only's 7,189 — and still scores 77.5%.
- **Beating full context is now significant** (p = 0.0137). Round 5 reported
  this as a directional trend at p = 0.26; that comparison crossed read epochs,
  and the same-session measurement is both larger and significant.
- **Re-reading full context changed its score** from 70.5% (July) to 69.0%
  (now) on identical contexts — the measurement floor from round 6, visible
  again.
- Dense and lexical are statistically indistinguishable from each other here
  (70.0% vs 69.0%). Neither leg is carrying the result; the fusion is.

### Harness

`retrieve_baselines.py` builds a context from one leg or both, filling to a
**character** budget rather than to a fixed k, because a comparison at equal k
compares budgets rather than methods. Each leg gets its own share of that
budget in the hybrid condition. The first version filled first-come-first-
served, which silently turned "hybrid" into dense-only — it scored identically,
down to the token count, and that run was discarded rather than reported.
