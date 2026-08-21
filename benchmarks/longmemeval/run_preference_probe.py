#!/usr/bin/env python3
"""Is `single-session-preference` measuring memory, or measuring answer length?

    python3 run_preference_probe.py

Every condition in the published LongMemEval table scores 15–31% on
`single-session-preference` — including `full`, which is handed the entire
haystack. When a category is equally hard with perfect information and with
none, the thing being measured is usually not the thing named on the tin.

Reading the data says what it is. The category's gold "answers" are **rubrics**:

> "The user would prefer responses that build upon their previous
>  experimentation with turbinado sugar, suggesting ingredients or techniques
>  that complement its richer flavor. They might not prefer generic
>  cookie-making advice…"

That describes what a good *long, personalised* response looks like. The
harness's reader prompt says:

> "Reply with ONLY the short answer (a few words; write dates like
>  '7 May 2023')."

So the reader is instructed to answer in five words and then graded against a
paragraph describing an elaborated recommendation. The ceiling is the answer
FORMAT, not the memory.

# The test

Same questions, same retrieved context, same judge. One thing changes: the
reader is asked for the kind of answer the rubric grades — a recommendation
that uses what it knows about the user.

- **`terse`** — the published prompt, re-read now so both arms are in the same
  read epoch (re-reading flips 8–12% of answers, so comparing against the
  stored numbers would measure the epoch).
- **`fitted`** — a prompt appropriate to a rubric-graded question.

If `fitted` is much higher, the published preference numbers understate every
condition equally, relative comparisons between conditions are unaffected, and
the harness has a prompt bug worth writing down.

If it is not, the category is genuinely hard and the finding is that.
"""
from __future__ import annotations

import json
import re
import sys
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent / "locomo"))
import run_locomo as R  # noqa: E402

RESULTS = HERE / "results"
OUT = HERE / "results_preference"
CONDITION = "grimoire-go"  # the shipped retrieval path; contexts already built

# The control. If the fitted prompt simply produces better answers everywhere,
# the finding is "the harness prompt is wrong". If it helps rubric-graded
# questions and HURTS fact-graded ones, the finding is the more useful one:
# one prompt cannot serve both question types, which is why the terse prompt
# exists and why it must not be applied to every category.
CATEGORY = __import__("os").environ.get("PREF_CATEGORY", "single-session-preference")

TERSE = """You are answering a question about a user's recorded \
chat history with an assistant. Use ONLY the context below.

<context>
{context}
</context>

The question was asked on {qdate}.
Question: {question}

Reply with ONLY the short answer (a few words; write dates like \
"7 May 2023"). If the context does not contain the answer, give your best \
guess. Do not explain, do not use tools."""

# The only change: the reader is told the question is asking for a
# recommendation grounded in what the history says about this person, which is
# what the gold rubric grades. It is NOT told anything about the rubric itself,
# and it still may use only the context.
FITTED = """You are answering a question about a user, using ONLY their \
recorded chat history below.

<context>
{context}
</context>

The question was asked on {qdate}.
Question: {question}

Answer the question the way an assistant who remembers this user would: make \
the recommendation or give the advice they are asking for, and ground it in \
what their history shows about their tastes, habits and past experience. \
Mention the specific things from their history that shape your answer. Two to \
four sentences. Use ONLY the context; do not invent details about them."""


def main() -> int:
    OUT.mkdir(exist_ok=True)
    qs = {q["qid"]: q for q in map(json.loads, (RESULTS / "questions.jsonl").open())
          if q["category"] == CATEGORY}
    ctxs = {r["qid"]: r["context"]
            for r in map(json.loads, (RESULTS / "contexts.jsonl").open())
            if r["condition"] == CONDITION and r["qid"] in qs}
    print(f"{len(qs)} preference questions, {len(ctxs)} contexts ({CONDITION})")

    jobs = [{"qid": qid, "arm": arm}
            for qid in sorted(ctxs) for arm in ("terse", "fitted")]
    rfile = OUT / f"reads-{CATEGORY}.jsonl"
    done = {(r["qid"], r["arm"]) for r in map(json.loads, rfile.open())} \
        if rfile.exists() else set()
    jobs = [j for j in jobs if (j["qid"], j["arm"]) not in done]

    if jobs:
        print(f"reading {len(jobs)} with {R.READER_MODEL}…")

        def read(job):
            q = qs[job["qid"]]
            tpl = TERSE if job["arm"] == "terse" else FITTED
            text, _ = R.claude_call(tpl.format(
                context=ctxs[job["qid"]], qdate=q["qdate"], question=q["question"]),
                R.READER_MODEL)
            return {"qid": job["qid"], "arm": job["arm"], "answer": text}

        R._run_parallel(jobs, read, rfile)

    reads = [json.loads(x) for x in rfile.open()]
    jfile = OUT / f"judged-{CATEGORY}.jsonl"
    jdone = {(r["qid"], r["arm"]) for r in map(json.loads, jfile.open())} \
        if jfile.exists() else set()
    todo = [r for r in reads if (r["qid"], r["arm"]) not in jdone]
    if todo:
        print(f"judging {len(todo)} with {R.JUDGE_MODEL}…")

        def judge(r):
            q = qs[r["qid"]]
            # The SAME judge and prompt the published table used. Changing the
            # judge as well would make this two experiments.
            text, _ = R.claude_call(R.JUDGE_PROMPT.format(
                question=q["question"], gold=q["gold"],
                answer=r["answer"] or "(no answer)"), R.JUDGE_MODEL)
            m = re.search(r'"correct"\s*:\s*(true|false)', text)
            return {"qid": r["qid"], "arm": r["arm"],
                    "correct": bool(m) and m.group(1) == "true"}

        R._run_parallel(todo, judge, jfile)

    judged = {}
    for r in map(json.loads, jfile.open()):
        judged[(r["qid"], r["arm"])] = r["correct"]
    ids = sorted({qid for qid, _ in judged})
    print(f"\n## {CONDITION} · {CATEGORY} · n = {len(ids)}\n")
    print("| reader prompt | correct |")
    print("|---|---|")
    for arm in ("terse", "fitted"):
        got = sum(judged.get((q, arm), False) for q in ids)
        print(f"| {arm} | {got}/{len(ids)} ({100*got/len(ids):.1f}%) |")

    flipped = [q for q in ids
               if not judged.get((q, "terse")) and judged.get((q, "fitted"))]
    broke = [q for q in ids
             if judged.get((q, "terse")) and not judged.get((q, "fitted"))]
    print(f"\nfixed by the prompt: {len(flipped)}   broken by it: {len(broke)}")
    n = len(flipped) + len(broke)
    if n:
        import math
        p = min(1.0, sum(math.comb(n, k) for k in range(0, min(len(flipped), len(broke)) + 1))
                / 2 ** n * 2)
        print(f"exact McNemar p = {p:.4f}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
