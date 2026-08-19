#!/usr/bin/env python3
"""Contexts from /api/context — the endpoint that reads the whole corpus when
it fits the budget and retrieves when it does not.

This is the condition that tests a claim the benchmarks themselves produced:
LoCoMo's conversations are ~24k tokens and full context beats retrieval on them
by 5.5 points, so a server that notices the corpus fits should recover that
gap. LongMemEval's ~118k-token haystacks are far over any sane budget, so the
same code must leave those numbers untouched — that is the control, not an
afterthought.
"""
from __future__ import annotations

import argparse
import json
import sys
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent))

import goserver  # noqa: E402
from retrieve_go import build_vault  # noqa: E402
from run_locomo import RESULTS, load_data  # noqa: E402


def api(base: str, path: str, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + path, data=data, method="POST" if data else "GET",
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=600) as r:
        raw = r.read().decode()
    return json.loads(raw) if raw.strip() else {}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True)
    ap.add_argument("--vault", required=True)
    ap.add_argument("--port", type=int, default=9511)
    ap.add_argument("--embed", default="auto", choices=("off", "auto", "ollama"))
    ap.add_argument("--condition", default="corpus-fits")
    ap.add_argument("--budget", type=int, default=150000)
    ap.add_argument("--k", type=int, default=10)
    a = ap.parse_args()

    data = load_data()
    qs = [json.loads(x) for x in (RESULTS / "questions.jsonl").open()]
    cfile = RESULTS / "contexts.jsonl"
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, cfile.open())} if cfile.exists() else set()
    todo = [q for q in qs if (q["qid"], a.condition) not in done]
    print(f"{len(todo)} questions for {a.condition!r} (budget={a.budget})")

    modes: dict[str, int] = {}
    written = 0
    with goserver.launch(a.binary, a.vault, a.port, a.embed) as base:
        with cfile.open("a") as out:
            for ci, c in enumerate(data):
                batch = [q for q in todo if q["conv"] == ci]
                if not batch:
                    continue
                build_vault(Path(a.vault), c["conversation"])
                api(base, "/api/reindex", {})
                for q in batch:
                    qq = urllib.parse.quote(q["question"])
                    res = api(base, f"/api/context?q={qq}&k={a.k}&budget={a.budget}")
                    modes[res.get("mode", "?")] = modes.get(res.get("mode", "?"), 0) + 1
                    parts = [f"### {p['title']}\n{p['chunk']}" for p in res.get("passages", [])]
                    out.write(json.dumps({
                        "qid": q["qid"], "condition": a.condition,
                        "context": "\n\n".join(parts)}) + "\n")
                    written += 1
                out.flush()
                print(f"  conv{ci}: {len(batch)} ({written}/{len(todo)})")
    print(f"wrote {written} contexts; modes chosen: {modes}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
