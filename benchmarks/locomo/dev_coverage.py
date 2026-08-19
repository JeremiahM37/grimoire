#!/usr/bin/env python3
"""LoCoMo evidence-turn coverage on the held-out dev questions.

Companion to ../longmemeval/dev_recall.py, and it exists for the same two
reasons: the scored 500-question sample must never be tuned against, and mean
recall hides the failure that actually matters.

Differences from the older dev_recall_http.py, which this does not replace:

  - the scored sample is excluded explicitly by qid, so a change cannot be
    tuned against a question that will later score it;
  - FULL coverage is reported alongside mean recall. A question whose answer
    depends on three evidence turns is answered wrong when two are retrieved,
    so the fraction of questions with EVERY evidence turn present is the number
    that tracks accuracy;
  - k and the search limit are parameters, so a change in chunk size can be
    compared at a matched CONTEXT budget rather than at a matched chunk count —
    halving chunk size halves the text a fixed k delivers, which would make any
    same-k comparison a comparison of budgets rather than of methods;
  - the number of DISTINCT SESSIONS in the context is reported alongside
    coverage. That column exists because its absence cost a scored run: a
    change that left evidence coverage flat here (84.5% -> 84.6%) turned out to
    cost 20.7 points of multi-hop accuracy, and the only thing that had moved
    was breadth — 11.2 distinct sessions per context down to 10.2. LoCoMo
    answers lean on sources beyond the annotated evidence turns, so coverage
    alone is not a sufficient proxy for this dataset and must never again be
    used as one.
"""
from __future__ import annotations

import argparse
import ast
import json
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent))

import goserver  # noqa: E402
from dev_recall_http import build_vault, norm, turn_texts  # noqa: E402
from run_locomo import CATS, load_data  # noqa: E402

# Notes are titled "Conversation session N (date)", so the retrieved context
# names its sources and breadth can be counted from what the READER sees.
SESSION_RE = re.compile(r"### Conversation session (\d+)")

RESULTS = HERE / "results"


def api(base: str, path: str, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + path, data=data, method="POST" if data else "GET",
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=600) as r:
        raw = r.read().decode()
    return json.loads(raw) if raw.strip() else {}


def context_for(base: str, question: str, k: int, search_limit: int) -> str:
    parts, seen = [], set()
    q = urllib.parse.quote(question)
    for c in api(base, f"/api/retrieve?q={q}&k={k}"):
        key = (c["path"], c["chunk"][:64])
        if key in seen:
            continue
        seen.add(key)
        parts.append(f"### {c['title']}\n{c['chunk']}")
    if search_limit:
        for h in api(base, f"/api/search?q={q}&limit={search_limit}&full=true"):
            text = h.get("body") or h.get("snippet") or ""
            key = (h["path"], text[:64])
            if key in seen:
                continue
            seen.add(key)
            parts.append(f"### {h['title']} (text search)\n{text}")
    return "\n\n".join(parts)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True)
    ap.add_argument("--vault", required=True)
    ap.add_argument("--port", type=int, default=9171)
    ap.add_argument("--embed", default="auto", choices=("off", "auto", "ollama"))
    ap.add_argument("--k", type=int, default=10)
    ap.add_argument("--search-limit", type=int, default=5)
    ap.add_argument("--convs", type=int, default=10)
    ap.add_argument("--tag", default="run")
    a = ap.parse_args()

    scored = {json.loads(line)["qid"] for line in (RESULTS / "questions.jsonl").open()}
    data = load_data()
    rows = []

    with goserver.launch(a.binary, a.vault, a.port, a.embed) as base:
        for ci, c in enumerate(data[: a.convs]):
            conv = c["conversation"]
            build_vault(Path(a.vault), conv)
            api(base, "/api/reindex", {})
            texts = turn_texts(conv)
            for qi, q in enumerate(c["qa"]):
                qid = f"c{ci}q{qi}"
                if qid in scored:          # never tune on a scored question
                    continue
                if int(q.get("category", 0)) == 5:
                    continue
                try:
                    ev = ast.literal_eval(str(q.get("evidence", "[]")))
                except (ValueError, SyntaxError):
                    ev = []
                ev = [norm(texts.get(d, "")) for d in ev]
                ev = [t for t in ev if t]
                if not ev:
                    continue
                raw = context_for(base, q["question"], a.k, a.search_limit)
                ctx = norm(raw)
                hit = sum(1 for t in ev if t[:120] in ctx)
                sessions = len(set(SESSION_RE.findall(raw)))
                rows.append({
                    "qid": qid,
                    "category": CATS.get(int(q["category"]), "other"),
                    "n_turns": len(ev), "n_hit": hit,
                    "recall": hit / len(ev), "full": hit == len(ev),
                    "ctx_chars": len(ctx), "sessions": sessions,
                })
            print(f"  conversation {ci}: {len(rows)} dev questions so far", flush=True)

    def agg(sel):
        if not sel:
            return None
        return (len(sel), 100 * sum(r["recall"] for r in sel) / len(sel),
                100 * sum(1 for r in sel if r["full"]) / len(sel),
                sum(r["ctx_chars"] for r in sel) / len(sel),
                sum(r["sessions"] for r in sel) / len(sel))

    print(f"\n=== {a.tag} — LoCoMo evidence-turn coverage, held-out dev ===")
    hdr = (f"{'slice':22} {'n':>5} {'turn rec':>9} {'TURN full':>10} "
           f"{'ctx chars':>10} {'sessions':>9}")
    print(hdr); print("-" * len(hdr))
    for cat in sorted({r["category"] for r in rows}):
        s = agg([r for r in rows if r["category"] == cat])
        print(f"{cat:22} {s[0]:>5} {s[1]:>8.1f}% {s[2]:>9.1f}% {s[3]:>10.0f} {s[4]:>9.2f}")
    s = agg([r for r in rows if r["n_turns"] > 1])
    print(f"{'multi-turn evidence':22} {s[0]:>5} {s[1]:>8.1f}% {s[2]:>9.1f}% {s[3]:>10.0f} {s[4]:>9.2f}")
    s = agg(rows)
    print("-" * len(hdr))
    print(f"{'ALL':22} {s[0]:>5} {s[1]:>8.1f}% {s[2]:>9.1f}% {s[3]:>10.0f} {s[4]:>9.2f}")
    out = HERE / "dev_results"
    out.mkdir(exist_ok=True)
    (out / f"{a.tag}.jsonl").write_text("\n".join(json.dumps(r) for r in rows))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
