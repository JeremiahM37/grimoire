#!/usr/bin/env python3
"""LongMemEval harness — see PROTOCOL.md. Reuses the LoCoMo harness's CLI
plumbing and retrieval recipe so both studies share one methodology.

    python run_lme.py sample | retrieve | read | judge | report

Dataset: set LME_DATA (default /tmp/longmemeval_s), fetched from
https://huggingface.co/datasets/xiaowu0162/longmemeval — never committed.
"""
import argparse
import collections
import json
import os
import random
import shutil
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).parent
REPO = HERE.parent.parent
RESULTS = HERE / "results"
DATA = Path(os.environ.get("LME_DATA", "/tmp/longmemeval_s"))
sys.path.insert(0, str(HERE.parent / "locomo"))
sys.path.insert(0, str(REPO))

import run_locomo as R  # noqa: E402  (claude_call, _run_parallel, prompts, …)

SEED = 42
SAMPLE_N = 200
CONDITIONS = ["none", "grimoire-local", "grimoire-ollama", "full"]

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


def load_data():
    return json.loads(DATA.read_text())


# ---- phases -----------------------------------------------------------------

def phase_sample():
    qfile = RESULTS / "questions.jsonl"
    if qfile.exists():
        print("sample is frozen, not resampling")
        return
    pool = collections.defaultdict(list)
    for i, q in enumerate(load_data()):
        if str(q["question_id"]).endswith("_abs"):
            continue                                # abstention excluded (PROTOCOL.md)
        pool[q["question_type"]].append(
            {"qid": q["question_id"], "idx": i, "category": q["question_type"],
             "question": q["question"], "gold": str(q["answer"]),
             "qdate": q.get("question_date", "")})
    total = sum(len(v) for v in pool.values())
    rng = random.Random(SEED)
    sample = []
    for t in sorted(pool):
        sample += rng.sample(pool[t], round(SAMPLE_N * len(pool[t]) / total))
    RESULTS.mkdir(exist_ok=True)
    with qfile.open("w") as f:
        for q in sample:
            f.write(json.dumps(q) + "\n")
    print(f"sampled {len(sample)} of {total}:",
          dict(collections.Counter(q["category"] for q in sample)))


def _session_note(turns):
    return "\n".join(f"{t['role'].capitalize()}: {t.get('content') or ''}".strip()
                     for t in turns)


def _build_vault(inst, vdir):
    from server import config, db, index, vault
    config.VAULT = vdir
    config.grimoire_dir().mkdir(parents=True, exist_ok=True)
    db.init(config.db_path())
    for i, (sess, when) in enumerate(zip(inst["haystack_sessions"],
                                         inst["haystack_dates"], strict=False)):
        rel = f"session-{i:03d}.md"
        vault.write(rel, f"Date: {when}\n\n{_session_note(sess)}",
                    {"title": f"Chat session {i + 1} ({when})"})
        index.upsert(rel)


def phase_retrieve():
    os.environ["GRIMOIRE_NO_WATCHER"] = "1"
    data = load_data()
    qs = [json.loads(x) for x in (RESULTS / "questions.jsonl").open()]
    cfile = RESULTS / "contexts.jsonl"
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, cfile.open())} if cfile.exists() else set()
    out = cfile.open("a")
    from server import db
    for cond in CONDITIONS:
        os.environ.pop("GRIMOIRE_OLLAMA_URL", None)
        os.environ["GRIMOIRE_LOCAL_EMBED"] = "off"
        if cond == "grimoire-ollama":
            os.environ["GRIMOIRE_OLLAMA_URL"] = R.OLLAMA_URL
            os.environ["GRIMOIRE_EMBED_MODEL"] = "nomic-embed-text"
        elif cond == "grimoire-local":
            os.environ["GRIMOIRE_LOCAL_EMBED"] = "auto"
        n_done = 0
        for q in qs:
            if (q["qid"], cond) in done:
                continue
            inst = data[q["idx"]]
            if cond.startswith("grimoire"):
                tmp = Path(tempfile.mkdtemp(prefix="lme-"))
                _build_vault(inst, tmp / "vault")
                ctx = R._grimoire_context(q["question"])
                db.close()
                shutil.rmtree(tmp, ignore_errors=True)
            elif cond == "full":
                ctx = "\n\n".join(
                    f"## Session {i + 1} — {when}\n{_session_note(sess)}"
                    for i, (sess, when) in enumerate(
                        zip(inst["haystack_sessions"], inst["haystack_dates"],
                            strict=False)))
            else:
                ctx = ""
            out.write(json.dumps({"qid": q["qid"], "condition": cond,
                                  "context": ctx}) + "\n")
            out.flush()
            n_done += 1
            if n_done % 25 == 0:
                print(f"  {cond}: {n_done}")
        print(f"retrieve {cond}: {n_done} new")
    out.close()


def phase_read(limit=None):
    qs = {q["qid"]: q for q in map(json.loads, (RESULTS / "questions.jsonl").open())}
    ctxs = {(r["qid"], r["condition"]): r["context"]
            for r in map(json.loads, (RESULTS / "contexts.jsonl").open())}
    rfile = RESULTS / "reads.jsonl"
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, rfile.open())} if rfile.exists() else set()
    jobs = [{"qid": qid, "condition": cond, "context": ctx}
            for (qid, cond), ctx in sorted(ctxs.items()) if (qid, cond) not in done]
    if limit:
        jobs = jobs[:limit]
    print(f"read: {len(jobs)} calls")

    def worker(job):
        q = qs[job["qid"]]
        prompt = READER_PROMPT.format(context=job["context"],
                                      question=q["question"], qdate=q["qdate"])
        text, toks = R.claude_call(prompt, R.READER_MODEL, timeout=420)
        return {"qid": job["qid"], "condition": job["condition"],
                "answer": text, "input_tokens": toks}

    R._run_parallel(jobs, worker, rfile)


def phase_judge(limit=None):
    qs = {q["qid"]: q for q in map(json.loads, (RESULTS / "questions.jsonl").open())}
    reads = [json.loads(x) for x in (RESULTS / "reads.jsonl").open()]
    jfile = RESULTS / "judged.jsonl"
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, jfile.open())} if jfile.exists() else set()
    jobs = [r for r in reads if (r["qid"], r["condition"]) not in done]
    if limit:
        jobs = jobs[:limit]
    print(f"judge: {len(jobs)} calls")

    def worker(job):
        import re
        q = qs[job["qid"]]
        prompt = R.JUDGE_PROMPT.format(question=q["question"], gold=q["gold"],
                                       answer=job["answer"] or "(no answer)")
        text, _ = R.claude_call(prompt, R.JUDGE_MODEL)
        m = re.search(r'"correct"\s*:\s*(true|false)', text)
        if not m:
            raise RuntimeError(f"unparseable judge output: {text[:120]}")
        return {"qid": job["qid"], "condition": job["condition"],
                "correct": m.group(1) == "true"}

    R._run_parallel(jobs, worker, jfile)


def phase_report():
    qs = {q["qid"]: q for q in map(json.loads, (RESULTS / "questions.jsonl").open())}
    toks = {(r["qid"], r["condition"]): r["input_tokens"]
            for r in map(json.loads, (RESULTS / "reads.jsonl").open())}
    cats = sorted({q["category"] for q in qs.values()})
    acc = collections.defaultdict(lambda: [0, 0])
    tot = collections.defaultdict(lambda: [0, 0])
    tk = collections.defaultdict(list)
    for r in map(json.loads, (RESULTS / "judged.jsonl").open()):
        cat = qs[r["qid"]]["category"]
        acc[(r["condition"], cat)][1] += 1
        tot[r["condition"]][1] += 1
        if r["correct"]:
            acc[(r["condition"], cat)][0] += 1
            tot[r["condition"]][0] += 1
        t = toks.get((r["qid"], r["condition"]))
        if t:
            tk[r["condition"]].append(t)
    print(f"{'condition':17}" + "".join(f"{c[:14]:>16}" for c in cats)
          + f"{'overall':>10}{'medtok':>9}")
    summary = {}
    for cond in CONDITIONS:
        ok, n = tot[cond]
        if not n:
            continue
        row = f"{cond:17}"
        for c in cats:
            o, m = acc[(cond, c)]
            row += f"{o / m * 100 if m else 0:15.1f}%"
        med = sorted(tk[cond])[len(tk[cond]) // 2] if tk[cond] else 0
        print(row + f"{ok / n * 100:9.1f}%{med:>9}")
        summary[cond] = {"overall": {"correct": ok, "n": n},
                         "by_type": {c: {"correct": acc[(cond, c)][0],
                                         "n": acc[(cond, c)][1]} for c in cats},
                         "median_input_tokens": med}
    (RESULTS / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("phase", choices=["sample", "retrieve", "read", "judge", "report"])
    ap.add_argument("--limit", type=int)
    a = ap.parse_args()
    {"sample": phase_sample, "retrieve": phase_retrieve,
     "read": lambda: phase_read(a.limit), "judge": lambda: phase_judge(a.limit),
     "report": phase_report}[a.phase]()
