#!/usr/bin/env python3
"""Score how well each retrieval signal separates answerable from unanswerable.

AUC is the whole story here: it is the probability that a random answerable
question scores above a random unanswerable one, it needs no threshold, and it
is invariant to the class prior. 0.5 is a coin flip.
"""
from __future__ import annotations

import json
import statistics as st
import sys
from pathlib import Path


def auc(pos, neg):
    allv = sorted([(v, 1) for v in pos] + [(v, 0) for v in neg])
    ranks, i = {}, 0
    while i < len(allv):
        j = i
        while j + 1 < len(allv) and allv[j + 1][0] == allv[i][0]:
            j += 1
        r = (i + j) / 2 + 1
        for k in range(i, j + 1):
            ranks[k] = r
        i = j + 1
    rp = sum(ranks[k] for k, (v, l) in enumerate(allv) if l == 1)
    n1, n0 = len(pos), len(neg)
    return (rp - n1 * (n1 + 1) / 2) / (n1 * n0)


def main(path="signals.jsonl", fields=None):
    recs = [json.loads(l) for l in Path(path).open()]
    A = [r for r in recs if r["label"] == "answerable"]
    U = [r for r in recs if r["label"] == "unanswerable"]
    print(f"n = {len(A)} answerable / {len(U)} unanswerable\n")
    fields = fields or [k for k, v in A[0].items()
                        if isinstance(v, (int, float)) and k not in ("conv", "ndocs")]
    rows = []
    for f in fields:
        a = [r[f] for r in A]
        u = [r[f] for r in U]
        rows.append((abs(auc(a, u) - 0.5), f, st.mean(a), st.mean(u), auc(a, u)))
    rows.sort(reverse=True)
    print(f"{'signal':22s} {'answerable':>12s} {'unanswerable':>13s} {'AUC':>7s}   verdict")
    for _, f, ma, mu, ac in rows:
        verdict = ("separates" if ac >= 0.60 else
                   "inverted" if ac <= 0.40 else "no signal")
        print(f"{f:22s} {ma:12.4f} {mu:13.4f} {ac:7.3f}   {verdict}")


if __name__ == "__main__":
    main(*(sys.argv[1:] or []))
