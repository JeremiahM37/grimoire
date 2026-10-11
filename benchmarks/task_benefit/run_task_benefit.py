#!/usr/bin/env python3
"""Task-benefit study: do stored project facts change answers to convention questions?

See README.md for the design and the limits. Commands:

    python run_task_benefit.py dry-run --binary BIN [--out DIR]
        offline. Real grimoire server (embed=off, no model). Two stand-in
        readers: an oracle that answers only when the fact is in its prompt,
        and a stub that never does. Proves the plumbing: the on arm should
        score 10/10 and the off arm 0/10, and the output says so.

    python run_task_benefit.py live --binary BIN --n 3 [--max-calls 60] [--out DIR]
        LIVE: Haiku readers through the shared budget (benchmarks/budget.py).
        Refuses to start if it would exceed --max-calls or the shared cap.
        Refuses to run into a directory that already holds answers.

    python run_task_benefit.py report [--out DIR]
        per-task pass counts, retrieval hits, paired sign test.

Arms:
    off   the question alone
    on    the question with the Grimoire memory context for it, fetched from
          GET /api/memory/context (the endpoint the agent hook calls)
"""
from __future__ import annotations

import argparse
import json
import math
import re
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
BENCH = HERE.parent
sys.path.insert(0, str(BENCH))
sys.path.insert(0, str(HERE))

import goserver  # noqa: E402
from budget import Budget, BudgetExceeded, Live  # noqa: E402
from tasks import DISTRACTORS, TASKS  # noqa: E402

SYSTEM = "Answer the user's question. Be brief."
PORT = 9171
OUT_DEFAULT = HERE / "results"
ARMS = ("off", "on")


def off_prompt(question: str) -> str:
    return f"{question}\nReply with only the answer."


def on_prompt(question: str, context: str) -> str:
    return ("Relevant project memory (Grimoire, retrieved for this question):\n"
            f"<memory>\n{context}\n</memory>\n\n{question}\n"
            "Reply with only the answer.")


def passes(answer: str, require: list[str]) -> bool:
    return all(re.search(r, answer or "", re.IGNORECASE) for r in require)


def has_fact(context: str, fact: str) -> bool:
    # The server stores memory text without its final punctuation, so match
    # the fact minus a trailing period rather than the literal sentence.
    return fact.rstrip(".") in _unescape_json(context)


def _unescape_json(s: str) -> str:
    # Go's json encoder writes < and > as < / > inside the
    # context string, so a plain substring test would miss "<major>".
    return re.sub(r"\\u([0-9a-fA-F]{4})", lambda m: chr(int(m.group(1), 16)), s)


# ---- server ------------------------------------------------------------------

def seed(base: str) -> None:
    """Write the Quarry facts and the Harbor distractors as memory entries."""
    texts = [(t["fact"], f"quarry-{t['id']}") for t in TASKS]
    texts += [(d, "harbor-distractor") for d in DISTRACTORS]
    for text, topic in texts:
        _post(base, "/api/memory", {"text": text, "topic": topic})


def _post(base: str, path: str, body: dict) -> dict:
    import urllib.request
    req = urllib.request.Request(base + path, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"},
                                 method="POST")
    with urllib.request.urlopen(req, timeout=20) as r:
        return json.loads(r.read().decode() or "{}")


def retrieve(base: str, question: str) -> str:
    """The hook's own call: lexical, all scopes, the default budget and limit."""
    import urllib.parse
    import urllib.request
    q = urllib.parse.urlencode({"q": question, "format": "json",
                                "max_bytes": 2400, "limit": 5, "scope": "all"})
    with urllib.request.urlopen(f"{base}/api/memory/context?{q}", timeout=20) as r:
        return json.loads(r.read().decode()).get("context", "")


# ---- models ------------------------------------------------------------------

class OracleModel:
    """Dry-run stand-in: answers with the gold value only if the fact is in
    the prompt. It checks the plumbing, not any model."""

    def complete(self, prompt: str, label: str = "") -> str:
        for t in TASKS:
            if has_fact(prompt, t["fact"]) and t["question"] in prompt:
                return t["gold"]
        return "I do not know."


class StubModel:
    def complete(self, prompt: str, label: str = "") -> str:
        return "I do not know."


# ---- study -------------------------------------------------------------------

def run_study(base: str, model, n: int, out: Path, dry: bool, resume: bool = False) -> dict:
    """resume=True keeps the rows already in answers.jsonl and skips their
    (task, arm, rep) keys, and does not re-seed the vault that already holds
    the facts. A finished call is never made again."""
    out.mkdir(parents=True, exist_ok=True)
    afile = out / "answers.jsonl"
    done = set()
    rows = []
    if resume and afile.exists():
        rows = [json.loads(x) for x in afile.read_text().splitlines() if x.strip()]
        done = {(r["task"], r["arm"], r["rep"]) for r in rows}
    else:
        seed(base)
    with afile.open("a" if resume else "w") as f:
        for t in TASKS:
            ctx = retrieve(base, t["question"])
            hit = has_fact(ctx, t["fact"])
            for arm in ARMS:
                prompt = off_prompt(t["question"]) if arm == "off" \
                    else on_prompt(t["question"], ctx)
                for rep in range(1, n + 1):
                    if (t["id"], arm, rep) in done:
                        continue
                    label = f"{t['id']}:{arm}:{rep}"
                    err = None
                    try:
                        answer = model.complete(prompt, label)
                    except Exception as e:  # noqa: BLE001 — a failed call is spent, not retried
                        if isinstance(e, BudgetExceeded):
                            raise
                        answer, err = "", str(e)[:200]
                    row = {"task": t["id"], "arm": arm, "rep": rep,
                           "answer": answer, "passed": passes(answer, t["require"]),
                           "retrieved_fact": hit, "error": err}
                    rows.append(row)
                    f.write(json.dumps(row) + "\n")
                    f.flush()
    summary = report_rows(rows, dry=dry)
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    return summary


def sign_test(plus: int, minus: int) -> float:
    """Exact two-sided binomial sign test on non-tied pairs."""
    n = plus + minus
    if n == 0:
        return 1.0
    k = min(plus, minus)
    p = sum(math.comb(n, i) for i in range(k + 1)) / 2 ** n * 2
    return min(1.0, p)


def report_rows(rows: list[dict], dry: bool) -> dict:
    tasks = {}
    for r in rows:
        d = tasks.setdefault(r["task"], {"off": [0, 0], "on": [0, 0], "retrieved": None})
        d[r["arm"]][1] += 1
        if r["passed"]:
            d[r["arm"]][0] += 1
        if r["arm"] == "on":
            d["retrieved"] = r["retrieved_fact"]
    plus = sum(1 for d in tasks.values() if d["on"][0] / max(d["on"][1], 1)
               > d["off"][0] / max(d["off"][1], 1))
    minus = sum(1 for d in tasks.values() if d["on"][0] / max(d["on"][1], 1)
                < d["off"][0] / max(d["off"][1], 1))
    errors = sum(1 for r in rows if r["error"])
    return {
        "status": "dry-run-not-a-result" if dry else "run",
        "calls": len(rows), "errors": errors,
        "per_task": tasks,
        "totals": {arm: [sum(d[arm][0] for d in tasks.values()),
                         sum(d[arm][1] for d in tasks.values())] for arm in ARMS},
        "retrieval_hits_on_arm": sum(1 for d in tasks.values() if d["retrieved"]),
        "sign_test": {"tasks_on_better": plus, "tasks_off_better": minus,
                      "p_two_sided": round(sign_test(plus, minus), 4),
                      "unit": "task (pass rate over reps), ties dropped"},
        "limit": "10 fictional-project tasks cannot establish a general effect",
    }


def cmd_dry(args) -> int:
    if not args.binary:
        raise SystemExit("dry-run needs --binary (the real server runs offline, no model)")
    out = Path(args.out) if args.out else Path(tempfile.mkdtemp(prefix="task-benefit-dry-"))
    with goserver.launch(args.binary, out / "vault", args.port, embed="off",
                         log=out / "grimoire-server.log") as base:
        summary = run_study(base, OracleModel(), args.n, out, dry=True)
        print("oracle:", summary["totals"], "retrieval hits:", summary["retrieval_hits_on_arm"])
        stub = run_study(base, StubModel(), args.n, out / "stub", dry=True)
        print("stub:  ", stub["totals"])
    return 0


def cmd_live(args) -> int:
    out = Path(args.out) if args.out else OUT_DEFAULT
    existing = out / "answers.jsonl"
    if existing.exists() and existing.stat().st_size and not args.resume:
        raise SystemExit(f"{out}/answers.jsonl exists; pass --resume to continue without re-spending")
    done = set()
    if args.resume and existing.exists():
        done = {(r["task"], r["arm"], r["rep"])
                for r in (json.loads(x) for x in existing.read_text().splitlines() if x.strip())}
    planned = len(TASKS) * len(ARMS) * args.n - len(done)
    if planned > args.max_calls:
        raise SystemExit(f"plan needs {planned} calls, above --max-calls {args.max_calls}")
    budget = Budget("task_benefit", run_cap=args.max_calls)
    model = Live(budget, SYSTEM)
    with goserver.launch(args.binary, out / "vault", args.port, embed="off",
                         log=out / "grimoire-server.log") as base:
        summary = run_study(base, model, args.n, out, dry=False, resume=args.resume)
    print(json.dumps({k: summary[k] for k in ("calls", "errors", "totals", "sign_test")}, indent=2))
    return 0


def cmd_report(args) -> int:
    out = Path(args.out) if args.out else OUT_DEFAULT
    rows = [json.loads(x) for x in (out / "answers.jsonl").read_text().splitlines() if x.strip()]
    print(json.dumps(report_rows(rows, dry=False), indent=2))
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    for name in ("dry-run", "live"):
        p = sub.add_parser(name)
        p.add_argument("--binary", required=True)
        p.add_argument("--n", type=int, default=3)
        p.add_argument("--port", type=int, default=PORT)
        p.add_argument("--out")
        if name == "live":
            p.add_argument("--max-calls", type=int, default=60)
            p.add_argument("--resume", action="store_true",
                           help="continue an interrupted run; keeps finished rows")
    r = sub.add_parser("report")
    r.add_argument("--out")
    args = ap.parse_args(argv)
    if args.cmd == "dry-run":
        return cmd_dry(args)
    if args.cmd == "live":
        return cmd_live(args)
    return cmd_report(args)


if __name__ == "__main__":
    raise SystemExit(main())
