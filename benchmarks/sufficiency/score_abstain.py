#!/usr/bin/env python3
"""Score the reader verdict against the two baselines.

Reports the two rates a caller cares about — how often it correctly refuses an
unanswerable question, and how much it gives up to get that — plus exact
McNemar between the verdict and the prose baseline on the questions where they
disagree.
"""
from __future__ import annotations

import json
import math
import sys
from pathlib import Path


def _retrieval_threshold(path="signals.jsonl"):
    """The standard heuristic's AUC, read from the probe's committed output."""
    p = Path(__file__).resolve().parent / path
    if not p.exists():
        return None
    from analyse import auc
    recs = [json.loads(l) for l in p.open()]
    pos = [r["top_cosine"] for r in recs if r["label"] == "answerable"]
    neg = [r["top_cosine"] for r in recs if r["label"] == "unanswerable"]
    return auc(pos, neg) if pos and neg else None



def mcnemar(b: int, c: int) -> float:
    """Exact two-sided binomial test on the discordant pairs."""
    n = b + c
    if n == 0:
        return 1.0
    k = min(b, c)
    tail = sum(math.comb(n, i) for i in range(k + 1)) / (2 ** n)
    return min(1.0, 2 * tail)


def main(path="abstain.jsonl"):
    recs = [json.loads(l) for l in Path(path).open()]
    A = [r for r in recs if r["label"] == "answerable"]
    U = [r for r in recs if r["label"] == "unanswerable"]
    print(f"n = {len(A)} answerable / {len(U)} unanswerable\n")

    def rate(rows, pred):
        return (sum(1 for r in rows if pred(r)) / len(rows)) if rows else 0.0

    verdict_abstain = lambda r: r["supported"] == "ungrounded"      # noqa: E731
    prose_abstain = lambda r: r["prose_abstained"]                  # noqa: E731

    print(f"{'signal':20s} {'refuses unanswerable':>21s} {'answers answerable':>19s} "
          f"{'balanced acc':>13s}")
    for name, pred in (("verdict", verdict_abstain), ("prose match", prose_abstain)):
        tnr = rate(U, pred)
        tpr = 1 - rate(A, pred)
        print(f"{name:20s} {tnr:20.1%} {tpr:19.1%} {(tnr + tpr) / 2:12.1%}")
    # Computed from the retrieval probe's own output rather than pasted in. A
    # hardcoded baseline goes stale silently: this one still read 43.9% after
    # the probe was re-sampled and the real figure became 0.550.
    threshold = _retrieval_threshold()
    if threshold is None:
        print(f"{'retrieval threshold':20s} {'—':>20s} {'—':>19s} "
              f"{'(run probe.py)':>12s}")
    else:
        print(f"{'retrieval threshold':20s} {'—':>20s} {'—':>19s} {threshold:12.1%}"
              "   (top_cosine AUC from signals.jsonl)")

    # Where the verdict and the prose baseline disagree, on unanswerable
    # questions: the cell that matters, since that is where a caller either
    # gets a usable refusal or does not.
    b = sum(1 for r in U if verdict_abstain(r) and not prose_abstain(r))
    c = sum(1 for r in U if prose_abstain(r) and not verdict_abstain(r))
    print(f"\nunanswerable, verdict vs prose: {b} verdict-only / {c} prose-only, "
          f"exact McNemar p = {mcnemar(b, c):.3f}")

    # The same per-category split the retrieval probe gets. The scores were
    # useful only on single-hop lookups and inverted on everything harder, so
    # the question for the reader is whether it holds up where they failed.
    names = {1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop"}
    cats = sorted({r.get("category", 0) for r in A})
    if len(cats) > 1:
        print(f"\n{'answerable category':22s} {'n':>4s} {'answered (not refused)':>23s}")
        for cat in cats:
            sub = [r for r in A if r.get("category") == cat]
            if sub:
                print(f"{names.get(cat, cat):22s} {len(sub):4d} "
                      f"{1 - rate(sub, verdict_abstain):22.1%}")

    # Verdict/answer disagreement: the model says the notes do not support an
    # answer and then writes a cited one anyway. It is a defect of the signal,
    # not of the questions, and reporting the rate is the difference between a
    # measurement and an advertisement.
    import re as _re
    cited = _re.compile(r"\[\d+\]")
    inconsistent = [r for r in recs
                    if r["supported"] == "ungrounded" and cited.search(r["answer"])]
    if inconsistent:
        print(f"\nverdict/answer disagreement: {len(inconsistent)} of "
              f"{sum(1 for r in recs if r['supported'] == 'ungrounded')} "
              f"'ungrounded' replies still cite sources "
              f"({len(inconsistent) / max(1, sum(1 for r in recs if r['supported'] == 'ungrounded')):.1%})")

    unknown = sum(1 for r in recs if r["supported"] == "unknown")
    if unknown:
        print(f"\n{unknown} of {len(recs)} replies carried no verdict line "
              f"({unknown / len(recs):.1%}) — counted as NOT refusing.")


if __name__ == "__main__":
    main(*(sys.argv[1:] or []))
