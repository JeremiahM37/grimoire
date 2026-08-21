#!/usr/bin/env python3
"""LongMemEval through the MEMORY ENGINE — the arm nobody has run.

    python3 run_memory_arm.py ingest   # build both stores, per question
    python3 run_memory_arm.py read     # answer from recalled facts
    python3 run_memory_arm.py judge
    python3 run_memory_arm.py report

Every published Grimoire number ingests a haystack as session NOTES and
retrieves passages. That measures retrieval and never touches the engine that
decides what a later statement does to an earlier one — which is the half of
the product mem0, Zep and Letta are built around, and the half whose whole
purpose is the `knowledge-update` category.

So this runs the same frozen questions, the same reader and the same judge
against two memory stores that differ in exactly one thing:

    reconciled   a later fact may supersede an earlier one (vault scope)
    append       writes are session-scoped, so nothing supersedes anything
                 across sessions — identical extraction, identical retrieval

and reports them beside the published chunk-retrieval numbers for the same
question ids.

Scoped to `knowledge-update` by default. Not because the others do not matter,
but because this is the category the mechanism exists for: if write-time
reconciliation cannot win here it cannot win anywhere, and 31 questions
measured properly says more than 200 measured thinly.
"""
from __future__ import annotations

import argparse
import collections
import json
import os
import re
import shutil
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

HERE = Path(__file__).parent
REPO = HERE.parent.parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent))
sys.path.insert(0, str(HERE.parent / "locomo"))

import retrieve_memory as M  # noqa: E402
from goserver import launch  # noqa: E402

import run_locomo as R  # noqa: E402  (claude_call and friends)

DATA = Path(os.environ.get("LME_DATA", "/tmp/lme_dl/longmemeval_s.json"))
RESULTS = Path(os.environ.get("MEM_RESULTS", HERE / "results_memory"))
FROZEN = HERE / "results" / "questions.jsonl"

# Three arms, differing in exactly one thing each.
#
#   append       nothing may supersede anything across sessions (control)
#   reconciled   the shipped rules: subject-predicate-value only
#   slots        the same, plus value-slot updates (internal/memory/slots.go)
#
# `slots` runs a DIFFERENT BINARY, built from the same tree with the mechanism
# added, because that is the only way to ablate a change to the write path
# without a runtime switch that would then have to exist in the product.
ARMS = ["append", "reconciled", "slots"]
BINARIES = {
    "append": "/tmp/grimoire-rules-only",
    "reconciled": "/tmp/grimoire-rules-only",
    "slots": "/tmp/grimoire-slots",
}
RECALL_LIMIT = 30
BASE_PORT = 9150
WORKERS = int(os.environ.get("MEM_WORKERS", "5"))

READER_PROMPT = """You are answering a question about a user's recorded \
chat history with an assistant. Use ONLY the context below.

<context>
{context}
</context>

The question was asked on {qdate}.
Question: {question}

Reply with ONLY the short answer (a few words; write dates like \
"7 May 2023"). If the context does not contain the answer, give your best \
guess. Do not explain, do not use tools."""

_lock = threading.Lock()


def questions(category: str | None) -> list[dict]:
    """The FROZEN sample, so every number here lines up with a published one."""
    qs = [json.loads(x) for x in FROZEN.open()]
    if category:
        qs = [q for q in qs if q["category"] == category]
    return qs


def haystacks(qids: set[str]) -> dict[str, dict]:
    """Load only the entries we need — the file is 278 MB."""
    out = {}
    for e in json.loads(DATA.read_text()):
        qid = str(e["question_id"])
        short = qid[:8]
        if short in qids or qid in qids:
            out[short] = e
    return out


# ---------------------------------------------------------------- ingest

def _ingest_one(job) -> dict:
    q, entry, arm, port = job
    vault = Path("/tmp/lme-mem") / f"{arm}-{q['qid']}"
    shutil.rmtree(vault, ignore_errors=True)
    t0 = time.time()
    with launch(BINARIES[arm], vault, port, embed="auto",
                env={"GRIMOIRE_RATE_LIMIT": "off"}) as base:
        stats = M.ingest(base, entry, reconciled=(arm != "append"))
        facts = M.recall(base, q["question"], RECALL_LIMIT)
        counts = M.stored_facts(base)
    rec = {
        "qid": q["qid"], "arm": arm,
        "context": M.context_from(facts),
        "recalled": len(facts),
        "stored": counts["total"],
        "live": counts["live"],
        "superseded": counts["superseded"],
        "rejected": stats.get("rejected", 0),
        "statements": stats["items"],
        "ops": stats["ops"],
        "seconds": round(time.time() - t0, 1),
    }
    # The store is the artifact; keep it only long enough to read from.
    shutil.rmtree(vault, ignore_errors=True)
    return rec


def phase_ingest(category: str | None) -> None:
    RESULTS.mkdir(parents=True, exist_ok=True)
    out_file = RESULTS / "contexts.jsonl"
    done = set()
    if out_file.exists():
        for line in out_file.open():
            r = json.loads(line)
            done.add((r["qid"], r["arm"]))

    qs = questions(category)
    hs = haystacks({q["qid"] for q in qs})
    missing = [q["qid"] for q in qs if q["qid"] not in hs]
    if missing:
        print(f"  ! {len(missing)} question(s) not found in the dataset: {missing[:5]}")
    # A port per (arm, worker). The two arms sharing a port range meant a
    # reconciled store and an append store could be launched on the same port
    # by different threads — which the harness's own vault check reports as
    # "port already in use" rather than as silently scoring the wrong vault.
    ready = [q for q in qs if q["qid"] in hs]
    jobs = []
    for a, arm in enumerate(ARMS):
        for i, q in enumerate(ready):
            if (q["qid"], arm) in done:
                continue
            jobs.append((q, hs[q["qid"]], arm,
                         BASE_PORT + a * WORKERS + (i % WORKERS)))
    if not jobs:
        print("ingest already complete")
        return
    print(f"ingesting {len(jobs)} store(s) with {WORKERS} workers…")

    # Each worker owns a port, so jobs are grouped by port and run serially
    # within a group: two servers on one port is the failure the harness's
    # own vault check exists to catch.
    by_port = collections.defaultdict(list)
    for j in jobs:
        by_port[j[3]].append(j)

    fh = out_file.open("a")
    n = [0]

    def worker(port_jobs):
        for j in port_jobs:
            try:
                rec = _ingest_one(j)
            except Exception as e:  # noqa: BLE001 — one store must not kill the run
                rec = {"qid": j[0]["qid"], "arm": j[2], "error": str(e)[:200],
                       "context": "", "recalled": 0, "stored": 0}
            with _lock:
                fh.write(json.dumps(rec) + "\n")
                fh.flush()
                n[0] += 1
                print(f"  [{n[0]}/{len(jobs)}] {rec['arm']:11} {rec['qid']} "
                      f"stored={rec.get('stored')} live={rec.get('live')} "
                      f"recalled={rec.get('recalled')} "
                      f"ops={rec.get('ops')} {rec.get('seconds', 0)}s"
                      + (f" ERROR {rec['error']}" if rec.get("error") else ""), flush=True)

    with ThreadPoolExecutor(max_workers=WORKERS) as ex:
        list(ex.map(worker, by_port.values()))
    fh.close()


# ------------------------------------------------------------------ read

def phase_read(category: str | None, model: str) -> None:
    qs = {q["qid"]: q for q in questions(category)}
    ctxs = [json.loads(x) for x in (RESULTS / "contexts.jsonl").open()]
    out_file = RESULTS / "reads.jsonl"
    done = set()
    if out_file.exists():
        for line in out_file.open():
            r = json.loads(line)
            done.add((r["qid"], r["arm"]))
    jobs = [c for c in ctxs if (c["qid"], c["arm"]) not in done and c["qid"] in qs]
    if not jobs:
        print("read already complete")
        return
    print(f"reading {len(jobs)} with {model}…")

    def worker(c):
        q = qs[c["qid"]]
        prompt = READER_PROMPT.format(context=c["context"] or "(nothing recalled)",
                                      qdate=q.get("qdate", ""), question=q["question"])
        text, toks = R.claude_call(prompt, model)
        return {"qid": c["qid"], "arm": c["arm"], "answer": text, "tokens": toks}

    R._run_parallel(jobs, worker, out_file)


# ----------------------------------------------------------------- judge

def phase_judge(category: str | None, model: str) -> None:
    qs = {q["qid"]: q for q in questions(category)}
    reads = [json.loads(x) for x in (RESULTS / "reads.jsonl").open()]
    out_file = RESULTS / "judged.jsonl"
    done = set()
    if out_file.exists():
        for line in out_file.open():
            r = json.loads(line)
            done.add((r["qid"], r["arm"]))
    jobs = [r for r in reads if (r["qid"], r["arm"]) not in done]
    if not jobs:
        print("judge already complete")
        return
    print(f"judging {len(jobs)} with {model}…")

    def worker(r):
        q = qs[r["qid"]]
        # The SAME judge prompt the published runs used. A different one would
        # make every comparison in the report meaningless.
        prompt = R.JUDGE_PROMPT.format(question=q["question"], gold=q["gold"],
                                       answer=r["answer"] or "(no answer)")
        text, _ = R.claude_call(prompt, model)
        m = re.search(r'"correct"\s*:\s*(true|false)', text)
        return {"qid": r["qid"], "arm": r["arm"],
                "correct": bool(m) and m.group(1) == "true"}

    R._run_parallel(jobs, worker, out_file)


# ---------------------------------------------------------------- report

def phase_report(category: str | None) -> None:
    qs = {q["qid"]: q for q in questions(category)}
    judged = collections.defaultdict(dict)
    for line in (RESULTS / "judged.jsonl").open():
        r = json.loads(line)
        judged[r["arm"]][r["qid"]] = r["correct"]
    ctx = {(c["qid"], c["arm"]): c
           for c in map(json.loads, (RESULTS / "contexts.jsonl").open())}

    # The published chunk-retrieval numbers for the SAME question ids.
    published = collections.defaultdict(dict)
    for line in (HERE / "results" / "judged.jsonl").open():
        r = json.loads(line)
        published[r["condition"]][r["qid"]] = r["correct"]

    have = [a for a in ARMS if judged[a]]
    qids = sorted(set.intersection(*[set(judged[a]) for a in have])) if have else []
    print(f"\ncategory: {category or 'all'}   n = {len(qids)}\n")
    print("| arm | correct | stored facts (median) | context chars (median) |")
    print("|---|---|---|---|")
    for arm in have:
        got = sum(judged[arm][q] for q in qids)
        stored = sorted(ctx[(q, arm)].get("live", 0) for q in qids)
        chars = sorted(len(ctx[(q, arm)]["context"]) for q in qids)
        mid = len(qids) // 2
        print(f"| memory · {arm} | {got}/{len(qids)} ({100*got/len(qids):.1f}%) | "
              f"{stored[mid] if stored else 0} | {chars[mid] if chars else 0} |")
    for cond in ("hybrid-matched", "grimoire-go", "full"):
        if not published[cond]:
            continue
        sub = [q for q in qids if q in published[cond]]
        if not sub:
            continue
        got = sum(published[cond][q] for q in sub)
        print(f"| chunks · {cond} (published) | {got}/{len(sub)} "
              f"({100*got/len(sub):.1f}%) | — | — |")

    print("\nwrite operations, per arm:")
    for arm in have:
        ops = collections.Counter()
        for q in qids:
            ops.update(ctx[(q, arm)].get("ops") or {})
        upd = ops.get("UPDATE", 0) + ops.get("DELETE", 0)
        total = sum(ops.values()) or 1
        print(f"  {arm:11} {dict(ops)}  supersessions {upd}/{total} "
              f"({100*upd/total:.2f}%)")

    # The ablation contrast, paired and exact — the only comparison this run
    # was built to make.
    import math
    def mcnemar(a, b):
        b01 = sum(1 for q in qids if not judged[a][q] and judged[b][q])
        b10 = sum(1 for q in qids if judged[a][q] and not judged[b][q])
        n = b01 + b10
        if n == 0:
            return b01, b10, 1.0
        p = min(1.0, sum(math.comb(n, k)
                         for k in range(0, min(b01, b10) + 1)) / 2 ** n * 2)
        return b01, b10, p
    print()
    for a, b in (("append", "reconciled"), ("append", "slots"),
                 ("reconciled", "slots")):
        if a in have and b in have:
            w, l, p = mcnemar(a, b)
            print(f"  {b} vs {a}: +{w} / -{l} discordant, exact p = {p:.4f}")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("phase", choices=["ingest", "read", "judge", "report"])
    ap.add_argument("--category", default="knowledge-update")
    ap.add_argument("--all-categories", action="store_true")
    # The SAME reader and judge the published table used. A different one and
    # every comparison in the report is between two different experiments.
    ap.add_argument("--reader", default=R.READER_MODEL)
    ap.add_argument("--judge", default=R.JUDGE_MODEL)
    args = ap.parse_args()
    cat = None if args.all_categories else args.category

    if args.phase == "ingest":
        phase_ingest(cat)
    elif args.phase == "read":
        phase_read(cat, args.reader)
    elif args.phase == "judge":
        phase_judge(cat, args.judge)
    else:
        phase_report(cat)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
