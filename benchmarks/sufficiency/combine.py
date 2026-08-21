#!/usr/bin/env python3
"""Is there ANY combination of the retrieval signals that works?

Section 1 shows no single signal reaches the 0.60 bar. That leaves the obvious
objection: maybe a *learned* combination does. This fits one — plain logistic
regression, gradient descent, no dependencies — and tests it honestly.

The split is by CONVERSATION, not by question. Splitting by question would put
questions from the same conversation on both sides, and these signals are
partly properties of the conversation (how long the sessions are, how much
vocabulary the questions share with them), so a question-level split measures
memorization of the conversation rather than generalization to a new one.
"""
from __future__ import annotations

import json
import math
import sys
from pathlib import Path

from analyse import auc

FEATURES = ["top_cosine", "max_cosine", "mean_cosine", "cosine_gap",
            "top_lexical", "max_lexical", "lexical_gap", "legs_agree",
            "top_score", "score_margin", "term_coverage",
            "rare_term_coverage", "n_query_terms"]


def standardize(rows):
    cols = list(zip(*rows, strict=True))
    stats = []
    for c in cols:
        mu = sum(c) / len(c)
        sd = (sum((x - mu) ** 2 for x in c) / len(c)) ** 0.5 or 1.0
        stats.append((mu, sd))
    return stats


def apply(rows, stats):
    return [[(x - mu) / sd for x, (mu, sd) in zip(r, stats, strict=True)] for r in rows]


def fit(X, y, epochs=4000, lr=0.05, l2=0.01):
    w = [0.0] * len(X[0])
    b = 0.0
    n = len(X)
    for _ in range(epochs):
        gw = [0.0] * len(w)
        gb = 0.0
        for xi, yi in zip(X, y, strict=True):
            z = b + sum(wj * xj for wj, xj in zip(w, xi, strict=True))
            p = 1 / (1 + math.exp(-max(-30, min(30, z))))
            d = p - yi
            for j, xj in enumerate(xi):
                gw[j] += d * xj
            gb += d
        for j in range(len(w)):
            w[j] -= lr * (gw[j] / n + l2 * w[j])
        b -= lr * gb / n
    return w, b


def score(X, w, b):
    return [b + sum(wj * xj for wj, xj in zip(w, xi, strict=True)) for xi in X]


def main(path="signals.jsonl"):
    recs = [json.loads(line) for line in Path(path).open()]
    convs = sorted({r["conv"] for r in recs})
    train_convs = set(convs[: len(convs) // 2])
    tr = [r for r in recs if r["conv"] in train_convs]
    te = [r for r in recs if r["conv"] not in train_convs]

    Xtr = [[r[f] for f in FEATURES] for r in tr]
    ytr = [1.0 if r["label"] == "answerable" else 0.0 for r in tr]
    Xte = [[r[f] for f in FEATURES] for r in te]

    stats = standardize(Xtr)
    w, b = fit(apply(Xtr, stats), ytr)

    def split(rows, scores):
        pos = [s for r, s in zip(rows, scores, strict=True) if r["label"] == "answerable"]
        neg = [s for r, s in zip(rows, scores, strict=True) if r["label"] == "unanswerable"]
        return pos, neg

    ptr, ntr = split(tr, score(apply(Xtr, stats), w, b))
    pte, nte = split(te, score(apply(Xte, stats), w, b))
    print(f"train: {len(tr)} questions from {len(train_convs)} conversations")
    print(f"test:  {len(te)} questions from {len(convs) - len(train_convs)} "
          f"held-out conversations\n")
    print(f"learned combination, TRAIN AUC     {auc(ptr, ntr):.3f}")
    print(f"learned combination, HELD-OUT AUC  {auc(pte, nte):.3f}")
    best = max(FEATURES, key=lambda f: abs(auc([r[f] for r in te if r['label'] == 'answerable'],
                                               [r[f] for r in te if r['label'] == 'unanswerable']) - 0.5))
    ba = auc([r[best] for r in te if r["label"] == "answerable"],
             [r[best] for r in te if r["label"] == "unanswerable"])
    print(f"best SINGLE signal on the same held-out set: {best} at {ba:.3f}")


if __name__ == "__main__":
    main(*(sys.argv[1:] or []))
