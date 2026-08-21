#!/usr/bin/env python3
"""Compare the verdict across readers on the same frozen questions.

The 4B run left one question open: is the verdict over-cautious because the
DESIGN is, or because a 4-billion-parameter model is? Same 120 questions, same
prompt, same retrieval — only the reader changes, so the difference is
attributable.
"""
from __future__ import annotations

import json
import math
import sys
from pathlib import Path


def mcnemar(b: int, c: int) -> float:
    n = b + c
    if n == 0:
        return 1.0
    k = min(b, c)
    return min(1.0, 2 * sum(math.comb(n, i) for i in range(k + 1)) / (2 ** n))


def load(path):
    return {r["qid"]: r for r in map(json.loads, Path(path).open())}


def rates(rows):
    A = [r for r in rows if r["label"] == "answerable"]
    U = [r for r in rows if r["label"] == "unanswerable"]
    refused = lambda r: r["supported"] == "ungrounded"  # noqa: E731
    tnr = sum(1 for r in U if refused(r)) / len(U) if U else 0
    tpr = 1 - (sum(1 for r in A if refused(r)) / len(A) if A else 0)
    return tnr, tpr, (tnr + tpr) / 2, len(A), len(U)


def main(a_path="abstain.jsonl", b_path="abstain-35b.jsonl"):
    a, b = load(a_path), load(b_path)
    shared = sorted(set(a) & set(b))
    # The pairing is what makes McNemar meaningful, and it rests on qid meaning
    # the same question in both runs. Both are sampled with the same seed, so it
    # does — but "so it does" is the kind of assumption that silently stops
    # being true when a sampler changes, so it is checked rather than trusted.
    mismatched = [q for q in shared if a[q]["question"] != b[q]["question"]]
    if mismatched:
        raise SystemExit(
            f"{len(mismatched)} shared qids name DIFFERENT questions in the two "
            f"runs (e.g. {mismatched[0]}) — the samples are not paired and "
            "nothing below would be comparable")
    print(f"{len(a)} / {len(b)} records; {len(shared)} questions answered by both\n")

    rows = [("reader", "refuses unanswerable", "answers answerable", "balanced")]
    for name, data in ((a[shared[0]].get("reader", "qwen3.5:4b"), a),
                       (b[shared[0]].get("reader", "?"), b)):
        sub = [data[q] for q in shared]
        tnr, tpr, bal, na, nu = rates(sub)
        rows.append((name, f"{tnr:.1%}", f"{tpr:.1%}", f"{bal:.1%}"))
    w = [max(len(r[i]) for r in rows) for i in range(4)]
    for r in rows:
        print("  ".join(x.ljust(w[i]) for i, x in enumerate(r)))

    # Paired: where do they disagree, and in which direction?
    for label in ("answerable", "unanswerable"):
        qs = [q for q in shared if a[q]["label"] == label]
        a_ref = {q for q in qs if a[q]["supported"] == "ungrounded"}
        b_ref = {q for q in qs if b[q]["supported"] == "ungrounded"}
        only_a, only_b = len(a_ref - b_ref), len(b_ref - a_ref)
        print(f"\n{label} (n={len(qs)}): refused only by the small reader {only_a}, "
              f"only by the large one {only_b}, exact McNemar p = "
              f"{mcnemar(only_a, only_b):.4f}")

    cited = __import__("re").compile(r"\[\d+\]")
    for name, data in ((a_path, a), (b_path, b)):
        ung = [data[q] for q in shared if data[q]["supported"] == "ungrounded"]
        bad = [r for r in ung if cited.search(r["answer"])]
        print(f"\n{name}: {len(bad)}/{len(ung)} 'ungrounded' replies still cite sources"
              f" ({len(bad) / len(ung):.1%})" if ung else f"\n{name}: no refusals")


if __name__ == "__main__":
    main(*(sys.argv[1:] or []))
