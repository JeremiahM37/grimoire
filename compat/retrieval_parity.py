#!/usr/bin/env python3
"""Prove the Go and Python builds retrieve IDENTICALLY.

Why this and not a benchmark re-run: LoCoMo and LongMemEval scores are a
function of (retrieval output) → (fixed reader) → (fixed judge). The reader and
judge are unchanged by the port, so if retrieval returns the same ranked
passages for the same corpus and queries, the published numbers describe the Go
build too. Re-running 700 LLM-judged questions would cost hours and dollars to
re-derive a value this establishes directly.

What it does NOT prove: that the benchmark numbers are correct — only that they
transfer. The original runs stand on their own protocol.

Usage (both servers running on the SAME vault content):

    .venv/bin/python compat/retrieval_parity.py
"""
from __future__ import annotations

import json
import os
import urllib.parse
import urllib.request

PY_BASE = os.environ.get("GRIMOIRE_PY_URL", "http://127.0.0.1:9111")
GO_BASE = os.environ.get("GRIMOIRE_GO_URL", "http://127.0.0.1:9121")

# Queries chosen to exercise the whole hybrid: exact-term hits (where BM25
# dominates), paraphrases (where the embedding leg dominates), rare terms,
# multi-term AND, and non-ASCII.
QUERIES = [
    "what port does the api gateway listen on",
    "who owns the deploy service",
    "kria fpga verilog testbench",
    "interview strategy tiered applications",
    "grimoire personal context server",
    "restic backup retention policy",
    "where do the notes live",
    "terraform proxmox provider import",
    "日本語 unicode note",
    "platform team on-call rota",
    "retrieval hybrid bm25 embedding",
    "what did the agent remember",
    "recipes sourdough flour",
    "roadmap ship v1",
    "vault encryption passphrase argon2",
]


def retrieve(base: str, q: str, k: int = 10):
    url = f"{base}/api/retrieve?q={urllib.parse.quote(q)}&k={k}"
    with urllib.request.urlopen(url, timeout=60) as r:
        return json.loads(r.read().decode())


def main() -> int:
    """Report AGREEMENT, not bit-equality.

    Bit-identical retrieval is not achievable across the two builds: numpy's
    np.linalg.norm is BLAS sdot, whose accumulation order depends on the BLAS
    kernel and is not portably reproducible. That leaves vectors differing by
    ~1e-6, and reciprocal-rank fusion converts a chunk's RANK into its score, so
    near-tied chunks can swap places. What matters for the benchmark claim is
    whether the passages a reader actually sees are the same, so that is what
    this measures.
    """
    top1 = top3 = 0
    overlap_sum = 0.0
    checked = 0
    detail = []

    for q in QUERIES:
        checked += 1
        try:
            p = retrieve(PY_BASE, q)
            g = retrieve(GO_BASE, q)
        except Exception as e:
            print(f"request failed for {q!r}: {e}")
            return 2
        pp = [h["path"] for h in p]
        gp = [h["path"] for h in g]
        if pp[:1] == gp[:1]:
            top1 += 1
        if pp[:3] == gp[:3]:
            top3 += 1
        inter = len(set(pp[:5]) & set(gp[:5]))
        denom = max(len(set(pp[:5])), 1)
        overlap_sum += inter / denom
        if pp[:3] != gp[:3]:
            detail.append(f"  {q!r}\n    py: {pp[:3]}\n    go: {gp[:3]}")

    print(f"queries compared: {checked}")
    print(f"  identical top-1 result : {top1}/{checked}")
    print(f"  identical top-3 order  : {top3}/{checked}")
    print(f"  mean top-5 set overlap : {overlap_sum / checked:.1%}")
    if detail:
        print("\ntop-3 differences:")
        for d in detail:
            print(d)
    # the gate: the passages a reader sees must agree at the top, where the
    # answer actually comes from
    mean_overlap = overlap_sum / checked
    ok = top1 == checked and mean_overlap >= 0.8
    if ok:
        print("\nPASS — retrieval agrees where it decides the answer")
    else:
        print("\nFAIL — retrieval disagrees at the top; the benchmark numbers "
              "must be RE-RUN against this build before being quoted for it")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
