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
    print(f"{'retrieval threshold':20s} {'—':>20s} {'—':>19s} {0.439:12.1%}"
          "   (AUC from probe.py; below chance)")

    # Where the verdict and the prose baseline disagree, on unanswerable
    # questions: the cell that matters, since that is where a caller either
    # gets a usable refusal or does not.
    b = sum(1 for r in U if verdict_abstain(r) and not prose_abstain(r))
    c = sum(1 for r in U if prose_abstain(r) and not verdict_abstain(r))
    print(f"\nunanswerable, verdict vs prose: {b} verdict-only / {c} prose-only, "
          f"exact McNemar p = {mcnemar(b, c):.3f}")

    unknown = sum(1 for r in recs if r["supported"] == "unknown")
    if unknown:
        print(f"\n{unknown} of {len(recs)} replies carried no verdict line "
              f"({unknown / len(recs):.1%}) — counted as NOT refusing.")


if __name__ == "__main__":
    main(*(sys.argv[1:] or []))
