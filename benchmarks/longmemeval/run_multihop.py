#!/usr/bin/env python3
"""Does the multi-hop path Grimoire ACTUALLY answers with beat the one every
published number was measured on?

    python3 run_multihop.py --category multi-session

`/api/ask` does not retrieve the way `/api/retrieve` does. It decomposes the
question into sub-questions, retrieves for each, and reranks the pool
(`smartRetrieve`). Every number in REPORT.md was built from plain
`/api/retrieve`, so **the shipped answering path has never been benchmarked**.

That matters most on `multi-session` — 56.9%, the worst of the fact-shaped
categories, and the one whose questions are defined by needing facts from more
than one session. Decomposition is the mechanism built for exactly that.

Two arms, same 200-question frozen sample, same reader, same judge, both read
in the same session:

    plain   /api/retrieve            — what every published number used
    smart   /api/retrieve?smart=1    — decompose → retrieve each → rerank

The server is configured with a local Ollama, because `Decompose` and `Rerank`
need a model and returning the question unchanged is what happens without one.
That is a documented, ordinary configuration; it is also the one thing that
differs from the published run, and it is why `plain` is re-measured here
rather than read from the stored numbers.
"""
from __future__ import annotations

import argparse
import json
import math
import re
import shutil
import sys
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).parent
REPO = HERE.parent.parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent))
sys.path.insert(0, str(HERE.parent / "locomo"))

from goserver import OLLAMA_URL, launch  # noqa: E402
import run_locomo as R  # noqa: E402

DATA = Path("/tmp/lme_dl/longmemeval_s.json")
RESULTS = HERE / "results"
OUT = HERE / "results_multihop"
PORT = 9175
# A third arm answers the question the first run could not: is decomposition
# useless here, or was the decomposer too weak? Only the SMART arms depend on
# the model — `plain` is byte-identical whatever is configured — so this adds
# one context build rather than two.
ARMS = ["plain", "smart", "smart35"]
MODELS = {"plain": "qwen3.5:4b", "smart": "qwen3.5:4b", "smart35": "qwen3.6:35b-a3b"}

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


def get(base, path, timeout=300):
    with urllib.request.urlopen(base + path, timeout=timeout) as r:
        return json.loads(r.read().decode() or "null")


def post(base, path, body, timeout=300):
    req = urllib.request.Request(
        base + path, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode() or "{}")


def session_note(turns) -> str:
    return "\n".join(f"{t.get('role','')}: {(t.get('content') or '').strip()}"
                     for t in turns)


def build_vault(vault: Path, entry: dict) -> None:
    """One haystack as session notes — the same ingestion every published
    condition used, so the only thing under test is retrieval."""
    if vault.exists():
        shutil.rmtree(vault)
    vault.mkdir(parents=True)
    dates = entry.get("haystack_dates") or []
    for i, turns in enumerate(entry["haystack_sessions"]):
        when = dates[i] if i < len(dates) else ""
        (vault / f"session-{i:03d}.md").write_text(
            f"# Session {i + 1} — {when}\n\n{session_note(turns)}\n", encoding="utf-8")


def context_for(base: str, question: str, smart: bool, k: int = 10) -> str:
    q = urllib.parse.urlencode({"q": question, "k": k})
    path = f"/api/retrieve?{q}" + ("&smart=1" if smart else "")
    hits = get(base, path) or []
    return "\n\n".join(f"## {h.get('title') or h.get('path')}\n{h.get('chunk','')}"
                       for h in hits)


def phase_contexts(category: str) -> None:
    OUT.mkdir(exist_ok=True)
    qs = [q for q in map(json.loads, (RESULTS / "questions.jsonl").open())
          if q["category"] == category]
    cfile = OUT / f"contexts-{category}.jsonl"
    done = {(r["qid"], r["arm"]) for r in map(json.loads, cfile.open())} \
        if cfile.exists() else set()
    todo = [q for q in qs if any((q["qid"], a) not in done for a in ARMS)]
    if not todo:
        print("contexts complete")
        return

    data = json.loads(DATA.read_text())
    # Index by BOTH the full id and its 8-character prefix. Most LongMemEval
    # ids are 8 hex characters, but some carry a `gpt4_` prefix and are 13 —
    # truncating those to 8 gives "gpt4_ab", which matches nothing, and seven
    # multi-session questions were silently skipped on the first run.
    by_short = {}
    for e in data:
        qid = str(e["question_id"])
        by_short[qid] = e
        by_short.setdefault(qid[:8], e)
    vault = Path("/tmp/lme-multihop")
    def env_for(arm: str) -> dict:
        return {
            "GRIMOIRE_OLLAMA_URL": OLLAMA_URL,
            "GRIMOIRE_LLM": "ollama",
            "GRIMOIRE_LLM_MODEL": MODELS[arm],
            "GRIMOIRE_RATE_LIMIT": "off",
            "GRIMOIRE_CONTEXT_BUDGET": "0",
        }

    env = {
        "GRIMOIRE_OLLAMA_URL": OLLAMA_URL,
        "GRIMOIRE_LLM": "ollama",
        "GRIMOIRE_LLM_MODEL": "qwen3.5:4b",
        "GRIMOIRE_RATE_LIMIT": "off",
        # The corpus-fits shortcut would hand over the whole vault and never
        # rank, which is a real grimoire behaviour and the wrong thing here:
        # the question is about RETRIEVAL.
        "GRIMOIRE_CONTEXT_BUDGET": "0",
    }
    fh = cfile.open("a")
    for i, q in enumerate(todo, 1):
        entry = by_short.get(q["qid"])
        if entry is None:
            print(f"  ! {q['qid']} not in dataset")
            continue
        build_vault(vault, entry)
        # One server per ARM now, because the arms differ in the model the
        # server is configured with. The vault is rebuilt once and reused.
        for arm in ARMS:
            if (q["qid"], arm) in done:
                continue
            with launch(str(REPO / "go" / "grimoire"), vault, PORT, embed="auto",
                        env=env_for(arm)) as base:
                try:
                    ctx = context_for(base, q["question"],
                                      smart=arm.startswith("smart"))
                except Exception as e:  # noqa: BLE001
                    ctx = ""
                    print(f"  ! {q['qid']}/{arm}: {e}")
            fh.write(json.dumps({"qid": q["qid"], "arm": arm,
                                 "context": ctx, "chars": len(ctx)}) + "\n")
            fh.flush()
        print(f"  [{i}/{len(todo)}] {q['qid']}", flush=True)
    fh.close()


def phase_read(category: str) -> None:
    qs = {q["qid"]: q for q in map(json.loads, (RESULTS / "questions.jsonl").open())}
    ctxs = [json.loads(x) for x in (OUT / f"contexts-{category}.jsonl").open()]
    rfile = OUT / f"reads-{category}.jsonl"
    done = {(r["qid"], r["arm"]) for r in map(json.loads, rfile.open())} \
        if rfile.exists() else set()
    jobs = [c for c in ctxs if (c["qid"], c["arm"]) not in done]
    if not jobs:
        print("read complete")
        return
    print(f"reading {len(jobs)}…")

    def worker(c):
        q = qs[c["qid"]]
        text, _ = R.claude_call(READER_PROMPT.format(
            context=c["context"] or "(nothing retrieved)",
            qdate=q["qdate"], question=q["question"]), R.READER_MODEL)
        return {"qid": c["qid"], "arm": c["arm"], "answer": text}

    R._run_parallel(jobs, worker, rfile)


def phase_judge(category: str) -> None:
    qs = {q["qid"]: q for q in map(json.loads, (RESULTS / "questions.jsonl").open())}
    reads = [json.loads(x) for x in (OUT / f"reads-{category}.jsonl").open()]
    jfile = OUT / f"judged-{category}.jsonl"
    done = {(r["qid"], r["arm"]) for r in map(json.loads, jfile.open())} \
        if jfile.exists() else set()
    jobs = [r for r in reads if (r["qid"], r["arm"]) not in done]
    if not jobs:
        print("judge complete")
        return
    print(f"judging {len(jobs)}…")

    def worker(r):
        q = qs[r["qid"]]
        text, _ = R.claude_call(R.JUDGE_PROMPT.format(
            question=q["question"], gold=q["gold"],
            answer=r["answer"] or "(no answer)"), R.JUDGE_MODEL)
        m = re.search(r'"correct"\s*:\s*(true|false)', text)
        return {"qid": r["qid"], "arm": r["arm"],
                "correct": bool(m) and m.group(1) == "true"}

    R._run_parallel(jobs, worker, jfile)


def phase_report(category: str) -> None:
    j = {}
    for r in map(json.loads, (OUT / f"judged-{category}.jsonl").open()):
        j[(r["qid"], r["arm"])] = r["correct"]
    ctx = {}
    for c in map(json.loads, (OUT / f"contexts-{category}.jsonl").open()):
        ctx[(c["qid"], c["arm"])] = c["chars"]
    have = [a for a in ARMS if any(k[1] == a for k in j)]
    ids = sorted({q for q, _ in j if all((q, a) in j for a in have)})
    if not ids:
        print("nothing judged yet")
        return
    print(f"\n## {category} · n = {len(ids)}\n")
    print("| retrieval | correct | context chars (median) |")
    print("|---|---|---|")
    for arm in have:
        got = sum(j[(q, arm)] for q in ids)
        chars = sorted(ctx.get((q, arm), 0) for q in ids)
        print(f"| {arm} | {got}/{len(ids)} ({100*got/len(ids):.1f}%) | "
              f"{chars[len(chars)//2]} |")
    for arm in ARMS:
        if arm == "plain":
            continue
        w = sum(1 for q in ids if not j[(q, "plain")] and j[(q, arm)])
        l = sum(1 for q in ids if j[(q, "plain")] and not j[(q, arm)])
        n = w + l
        p = min(1.0, sum(math.comb(n, k) for k in range(0, min(w, l) + 1))
                / 2 ** n * 2) if n else 1.0
        print(f"{arm} vs plain: fixed {w}, broke {l}, exact McNemar p = {p:.4f}")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("phase", choices=["contexts", "read", "judge", "report", "all"])
    ap.add_argument("--category", default="multi-session")
    a = ap.parse_args()
    for ph in (["contexts", "read", "judge", "report"] if a.phase == "all"
               else [a.phase]):
        globals()[f"phase_{ph}"](a.category)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
