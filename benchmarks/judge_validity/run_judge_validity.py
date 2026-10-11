#!/usr/bin/env python3
"""Judge validity: does the repo's LoCoMo judge accept wrong-but-adjacent answers?

See README.md. Commands:

    python run_judge_validity.py dry-run [--out DIR]
        offline. The judge is a fake that accepts everything, so the leniency
        metric reads 100% by construction. Proves the plumbing only.

    python run_judge_validity.py live [--max-calls 40] [--out DIR]
        LIVE: one Haiku judge call per wrong answer (30) and per correct
        paraphrase (10), using the judge prompt from benchmarks/locomo unchanged.
        Goes through the shared budget and refuses to re-spend a finished run.

    python run_judge_validity.py report [--out DIR]

Per triple, the judge and the deterministic key-fact checker both grade the
same answer. Reported: judge leniency on wrong answers (with a Wilson 95% CI),
judge false rejects on correct paraphrases, checker results on both, and the
list of every triple where the two disagree.
"""
from __future__ import annotations

import argparse
import json
import math
import re
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
BENCH = HERE.parent
sys.path.insert(0, str(BENCH))
sys.path.insert(0, str(BENCH / "locomo"))
sys.path.insert(0, str(HERE))

import run_locomo as L  # noqa: E402  (the repo's judge prompt and lean system line)
from budget import Budget, BudgetExceeded, Live  # noqa: E402
from triples import TRIPLES  # noqa: E402

OUT_DEFAULT = HERE / "results"
JUDGE_PROMPT = L.JUDGE_PROMPT
SYSTEM = L.LEAN_SYSTEM_PROMPT


def checker(answer: str, require: list[str]) -> bool:
    return all(re.search(r, answer or "", re.IGNORECASE) for r in require)


def parse_verdict(text: str):
    m = re.search(r'"correct"\s*:\s*(true|false)', text or "")
    return None if m is None else m.group(1) == "true"


def wilson(k: int, n: int, z: float = 1.96):
    if n == 0:
        return (0.0, 1.0)
    p = k / n
    d = 1 + z * z / n
    c = (p + z * z / (2 * n)) / d
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / d
    return (round(max(0.0, c - h), 4), round(min(1.0, c + h), 4))


def jobs():
    """Every (triple, kind, answer) the study grades, in a fixed order."""
    out = []
    for t in TRIPLES:
        out.append((t, "wrong", t["wrong"]))
    for t in TRIPLES:
        if "ok" in t:
            out.append((t, "paraphrase", t["ok"]))
    return out


class FakeJudge:
    """Dry-run only: accepts everything, so leniency is 100% by construction."""

    def complete(self, prompt, label=""):
        return '{"correct": true}'


def grade(model, out: Path, dry: bool) -> dict:
    out.mkdir(parents=True, exist_ok=True)
    rows = []
    with (out / "judgements.jsonl").open("w") as f:
        for t, kind, answer in jobs():
            prompt = JUDGE_PROMPT.format(question=t["question"], gold=t["gold"],
                                         answer=answer)
            label = f"{t['qid']}:{kind}"
            err = None
            try:
                raw = model.complete(prompt, label)
            except BudgetExceeded:
                raise
            except Exception as e:  # noqa: BLE001 — spent, not retried
                raw, err = "", str(e)[:200]
            row = {"qid": t["qid"], "kind": kind, "answer": answer,
                   "judge": parse_verdict(raw), "checker": checker(answer, t["require"]),
                   "raw": raw[:200], "error": err}
            rows.append(row)
            f.write(json.dumps(row) + "\n")
            f.flush()
    summary = report_rows(rows, dry=dry)
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    return summary


def report_rows(rows: list[dict], dry: bool) -> dict:
    wrong = [r for r in rows if r["kind"] == "wrong"]
    para = [r for r in rows if r["kind"] == "paraphrase"]
    lenient = sum(1 for r in wrong if r["judge"] is True)
    checker_wrong_ok = sum(1 for r in wrong if r["checker"])
    para_judge_ok = sum(1 for r in para if r["judge"] is True)
    para_checker_ok = sum(1 for r in para if r["checker"])
    unparsed = sum(1 for r in rows if r["judge"] is None)
    # the gold answers are graded by the checker alone (a free sanity check)
    gold_checker = sum(1 for t in TRIPLES if checker(t["gold"], t["require"]))
    disagree = [{"qid": r["qid"], "kind": r["kind"], "answer": r["answer"],
                 "judge": r["judge"], "checker": r["checker"]}
                for r in rows if r["judge"] is not None and r["judge"] != r["checker"]]
    return {
        "status": "dry-run-not-a-result" if dry else "run",
        "judge_model": "haiku (LoCoMo JUDGE_PROMPT unchanged; LoCoMo used claude-sonnet-5)",
        "calls": len(rows), "errors": sum(1 for r in rows if r["error"]),
        "unparseable_judge_output": unparsed,
        "wrong_answers": {"n": len(wrong), "judge_accepted": lenient,
                          "leniency": round(lenient / len(wrong), 4) if wrong else None,
                          "leniency_ci95": wilson(lenient, len(wrong)),
                          "checker_accepted": checker_wrong_ok},
        "correct_paraphrases": {"n": len(para), "judge_accepted": para_judge_ok,
                                "judge_false_reject_rate":
                                    round(1 - para_judge_ok / len(para), 4) if para else None,
                                "checker_accepted": para_checker_ok},
        "gold_answers_checker_accepted": f"{gold_checker}/{len(TRIPLES)}",
        "disagreements": disagree,
        "limit": "28-30 hand-written triples from one benchmark; the wrong answers are "
                 "authored, not sampled from a model, so the leniency rate is "
                 "specific to these triples",
    }


def cmd_dry(args) -> int:
    out = Path(args.out) if args.out else Path(__import__("tempfile").mkdtemp(prefix="judge-dry-"))
    s = grade(FakeJudge(), out, dry=True)
    print(json.dumps({k: s[k] for k in ("calls", "wrong_answers", "gold_answers_checker_accepted")},
                     indent=2))
    return 0


def cmd_live(args) -> int:
    out = Path(args.out) if args.out else OUT_DEFAULT
    planned = len(jobs())
    if planned > args.max_calls:
        raise SystemExit(f"plan needs {planned} calls, above --max-calls {args.max_calls}")
    if (out / "judgements.jsonl").exists() and (out / "judgements.jsonl").stat().st_size:
        raise SystemExit(f"{out}/judgements.jsonl exists; refusing to re-spend calls")
    budget = Budget("judge_validity", run_cap=args.max_calls)
    s = grade(Live(budget, SYSTEM), out, dry=False)
    print(json.dumps({k: s[k] for k in ("calls", "errors", "wrong_answers",
                                         "correct_paraphrases", "disagreements")}, indent=2))
    return 0


def cmd_report(args) -> int:
    out = Path(args.out) if args.out else OUT_DEFAULT
    rows = [json.loads(x) for x in (out / "judgements.jsonl").read_text().splitlines() if x.strip()]
    print(json.dumps(report_rows(rows, dry=False), indent=2))
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)
    d = sub.add_parser("dry-run")
    d.add_argument("--out")
    lv = sub.add_parser("live")
    lv.add_argument("--max-calls", type=int, default=40)
    lv.add_argument("--out")
    r = sub.add_parser("report")
    r.add_argument("--out")
    a = ap.parse_args(argv)
    return {"dry-run": cmd_dry, "live": cmd_live, "report": cmd_report}[a.cmd](a)


if __name__ == "__main__":
    raise SystemExit(main())
