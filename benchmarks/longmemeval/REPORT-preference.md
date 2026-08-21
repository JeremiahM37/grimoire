# `single-session-preference` was measuring answer length, not memory

Raw output: `results_preference/`. Reproduce with
`PREF_CATEGORY=<category> python3 run_preference_probe.py` for each of the six
categories (preference is the default).

**This is a correction to our own published table.** The
`single-session-preference` numbers in [REPORT.md](REPORT.md) — 15% to 31%
across every condition — were an artifact of the harness's reader prompt. They
understate every condition, and they are the reason the category looked
impossible.

---

## The tell

Every condition scored 15–31% on this category. Including `full`, which is
handed the entire ~118k-token haystack, and which therefore has perfect
information by construction. When a category is equally hard with perfect
information and with a 7k-token excerpt, the thing being measured is not
memory.

## What it actually was

The category's gold "answers" are **rubrics**, not answers:

> "The user would prefer responses that build upon their previous
> experimentation with turbinado sugar, suggesting ingredients or techniques
> that complement its richer flavor. They might not prefer generic
> cookie-making advice or suggestions that don't take into account…"

The harness's reader prompt — the same one used for every category — says:

> "Reply with ONLY the short answer (a few words; write dates like
> '7 May 2023'). … Do not explain."

So the reader was told to answer in five words, and then graded against a
paragraph describing an elaborated, personalised recommendation. It answered as
instructed:

| question | reader | judged |
|---|---|---|
| "my chocolate chip cookies need something extra. Any advice?" | "Try brown butter, sea salt, or espresso powder." | ✗ |
| "would it be a good idea to attend my high school reunion?" | "Yes, if you'd enjoy reconnecting with old friends." | ✗ |

Both answers are fine. Neither could ever satisfy the rubric, because the
rubric grades a form the prompt forbids.

## The test

Same 13 questions, same retrieved context (the shipped retrieval path), same
judge and same judge prompt. Both arms read in the **same session**, because
re-reading a byte-identical context flips 8–12% of answers and comparing
against the stored numbers would measure the epoch.

One thing changes: the `fitted` reader is asked for the kind of answer the
question is asking for — a recommendation grounded in what the history shows
about this person. It is told nothing about the rubric, and may still use only
the context.

| reader prompt | correct |
|---|---|
| terse (as published) | 3/13 (23.1%) |
| **fitted** | **10/13 (76.9%)** |

**7 fixed, 0 broken. Exact McNemar p = 0.0156.**

## Every category, which localises the bug

If the fitted prompt simply produced better answers everywhere, the finding
would be "our prompt is bad". Run across **all six categories — all 200
questions** — same condition, same judge, both arms in the same read epoch:

| category | n | terse | fitted | delta | fixed/broken | p |
|---|---|---|---|---|---|---|
| **single-session-preference** | 13 | 23.1% | **76.9%** | **+53.8** | 7/0 | **0.016** |
| single-session-assistant | 24 | 87.5% | 91.7% | +4.2 | 1/0 | 1.00 |
| knowledge-update | 31 | 77.4% | 80.6% | +3.2 | 2/1 | 1.00 |
| multi-session | 51 | 56.9% | 58.8% | +2.0 | 2/1 | 1.00 |
| single-session-user | 27 | 88.9% | 88.9% | 0.0 | 0/0 | 1.00 |
| temporal-reasoning | 54 | 81.5% | 81.5% | 0.0 | 2/2 | 1.00 |
| **overall** | **200** | **72.5%** | **77.5%** | **+5.0** | | |

**The bug is one category, not the prompt in general.** Preference moves 53.8
points and is the only category where anything is significant. The other five
move between 0.0 and 4.2, every one of them p = 1.00, and **seven of the
overall five points come from preference alone**. Nothing is made worse in a
way the data can see: across 187 non-preference questions the fitted prompt
fixed 7 and broke 5.

`knowledge-update` and `temporal-reasoning` are the controls that mattered
most — the most fact-shaped categories, where a four-sentence answer has the
most room to bury the value the judge is hunting for. Neither suffered.

## What this changes

- **The preference numbers in REPORT.md are wrong, for every condition.** They
  measure how well a five-word answer can satisfy a paragraph-long rubric,
  which is roughly not at all.
- **The overall number is understated by 5.0 points** on the condition
  measured: 72.5% → 77.5% across all 200 questions. Seven of those points'
  worth of questions are preference; the rest is scatter.
- **The ranking between conditions is unaffected.** Every condition shared the
  prompt, so the comparisons in REPORT.md — hybrid vs dense vs BM25 vs full —
  stand exactly as published. This corrects the level, not the order.
- **Anyone building a LongMemEval harness with one terse prompt for all six
  categories has the same bug.** It is the obvious way to build it: five of the
  six categories want a short factual answer, so the sixth is easy to miss.

## Honest about the method

The prompt was written **after** reading the failures — this is post-hoc, and a
prompt tuned until a number improves is worth very little on its own. Three
things are offered against that:

1. The mechanism was diagnosed from the data before the fix: the gold is a
   rubric, the prompt forbids the form the rubric grades. The fix follows from
   the diagnosis rather than from a search.
2. The control category was chosen and stated before it was run.
3. 7 fixed against 0 broken is not the shape of a tuned number.

It is still one condition and one reader. The preference effect rests on 13
questions — but the *localisation* rests on all 200, and the claim that the
terse prompt costs nothing elsewhere is the better-powered half of the result.
What this establishes is that the category's ceiling was the prompt; it does
not establish where the real ceiling is.

## Not fixed here

REPORT.md's tables are **left as published**, with a pointer to this file. They
are what that run measured, and silently restating them against a prompt that
run did not use would be worse than leaving a corrected record. Re-running all
fourteen conditions on a fitted prompt is the honest fix and has not been done.
