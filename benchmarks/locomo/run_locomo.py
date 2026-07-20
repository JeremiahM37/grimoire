#!/usr/bin/env python3
"""LoCoMo benchmark harness — see PROTOCOL.md (pre-registered) for the design.

Phases (resumable; each appends JSONL and skips work already done):

    python run_locomo.py sample                # draw the frozen question sample
    python run_locomo.py retrieve              # build vaults + collect contexts
    python run_locomo.py read   [--limit N]    # reader calls (claude CLI, parallel)
    python run_locomo.py judge  [--limit N]    # judge calls (claude CLI, parallel)
    python run_locomo.py report                # accuracy + token tables

The dataset is fetched to DATA (never committed). Grimoire is driven
in-process through the exact functions the product serves.
"""
import argparse
import collections
import json
import os
import random
import re
import shutil
import subprocess
import sys
import tempfile
import threading
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

HERE = Path(__file__).parent
REPO = HERE.parent.parent
RESULTS = HERE / "results"
DATA = Path(os.environ.get("LOCOMO_DATA", "/tmp/locomo10.json"))
DATA_URL = "https://raw.githubusercontent.com/snap-research/locomo/main/data/locomo10.json"

SEED = 42
SAMPLE_N = 500
CONDITIONS = ["none", "grimoire", "grimoire-local", "grimoire-ollama", "full"]
READER_MODEL = "claude-haiku-4-5"
JUDGE_MODEL = "claude-sonnet-5"
PARALLEL = int(os.environ.get("LOCOMO_PARALLEL", "12"))
OLLAMA_URL = os.environ.get("LOCOMO_OLLAMA_URL", "http://192.168.1.86:11434")

READER_PROMPT = """You are answering a question about a recorded conversation \
between two people. Use ONLY the context below.

<context>
{context}
</context>

Question: {question}

Reply with ONLY the short answer (a few words; write dates like "7 May 2023"). \
If the context does not contain the answer, give your best guess. Do not \
explain, do not use tools."""

# v2 (strict): v1 marked hedged non-answers ("not specified", "no explicit
# mention") as correct. Amendment recorded in PROTOCOL.md; every round is
# judged (or re-judged) under v2 so all reported numbers share one judge.
JUDGE_PROMPT = """You are strictly grading a model's answer against a gold \
answer.

Question: {question}
Gold answer: {gold}
Model answer: {answer}

Mark correct ONLY if the model answer affirmatively states the same essential \
information as the gold answer. If the model says the information is missing \
or not specified, hedges without committing, or gives a different value, it \
is incorrect — even if it also speculates near the right answer. Minor \
wording or format differences are fine. A date is correct only if it refers \
to the same day. A list answer is correct only if it names the gold items. \
Reply with JSON only: {{"correct": true}} or {{"correct": false}}"""


# ---- dataset ----------------------------------------------------------------

def load_data():
    if not DATA.exists():
        import urllib.request
        print(f"fetching LoCoMo -> {DATA}")
        urllib.request.urlretrieve(DATA_URL, DATA)
    return json.loads(DATA.read_text())


def session_docs(conv):
    """[(session_no, date_time, transcript_text)] with photo captions inlined."""
    out = []
    ns = sorted(int(k.split("_")[1]) for k in conv
                if re.fullmatch(r"session_\d+", k))
    for n in ns:
        lines = []
        for t in conv[f"session_{n}"]:
            line = f"{t['speaker']}: {t.get('text', '')}".strip()
            if t.get("blip_caption"):
                line += f" [shared a photo: {t['blip_caption']}]"
            lines.append(line)
        out.append((n, conv.get(f"session_{n}_date_time", ""), "\n".join(lines)))
    return out


# ---- phase: sample ----------------------------------------------------------

def phase_sample():
    qfile = RESULTS / "questions.jsonl"
    if qfile.exists():
        print(f"{qfile} exists — sample is frozen, not resampling")
        return
    data = load_data()
    pool = collections.defaultdict(list)      # category -> [q]
    for ci, c in enumerate(data):
        for qi, q in enumerate(c["qa"]):
            cat = int(q["category"])
            if cat == 5:
                continue                      # adversarial excluded (PROTOCOL.md)
            pool[cat].append({"qid": f"c{ci}q{qi}", "conv": ci, "category": cat,
                              "question": q["question"], "gold": str(q["answer"])})
    total = sum(len(v) for v in pool.values())
    rng = random.Random(SEED)
    sample = []
    for cat in sorted(pool):
        k = round(SAMPLE_N * len(pool[cat]) / total)
        sample += rng.sample(pool[cat], k)
    RESULTS.mkdir(exist_ok=True)
    with qfile.open("w") as f:
        for q in sample:
            f.write(json.dumps(q) + "\n")
    by = collections.Counter(q["category"] for q in sample)
    print(f"sampled {len(sample)} of {total} (seed {SEED}) by category: {dict(by)}")


# ---- phase: retrieve --------------------------------------------------------

def _grimoire_context(q, use_search=True):
    """Context exactly as the product retrieves it: top-10 semantic chunks +
    top-5 FTS hits for the raw question."""
    from server import index
    from server.routers.search import search as fts_search
    parts, seen = [], set()
    for c in index.retrieve(q, k=10):
        key = (c["path"], c["chunk"][:64])
        if key not in seen:
            seen.add(key)
            parts.append(f"### {c['title']}\n{c['chunk']}")
    if use_search:
        for h in fts_search(q=q, limit=5, full=True):
            key = (h["path"], (h.get("body") or h["snippet"])[:64])
            if key not in seen:
                seen.add(key)
                parts.append(f"### {h['title']} (text search)\n"
                             f"{h.get('body') or h['snippet']}")
    return "\n\n".join(parts)


def _build_vault(conv, vdir):
    from server import config, db, index, vault
    config.VAULT = vdir
    config.grimoire_dir().mkdir(parents=True, exist_ok=True)
    db.init(config.db_path())
    for n, when, text in session_docs(conv):
        rel = f"session-{n:02d}.md"
        # round 3+: the session date lives in the note TITLE (as a person
        # titles a meeting log), so every retrieval hit carries its date
        vault.write(rel, f"Date: {when}\n\n{text}",
                    {"title": f"Conversation session {n} ({when})"})
        index.upsert(rel)


def phase_retrieve():
    os.environ["GRIMOIRE_NO_WATCHER"] = "1"
    sys.path.insert(0, str(REPO))
    data = load_data()
    qs = [json.loads(x) for x in (RESULTS / "questions.jsonl").open()]
    cfile = RESULTS / "contexts.jsonl"
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, cfile.open())} if cfile.exists() else set()
    out = cfile.open("a")
    for cond in ["grimoire", "grimoire-local", "grimoire-ollama", "full", "none"]:
        os.environ.pop("GRIMOIRE_OLLAMA_URL", None)
        os.environ["GRIMOIRE_LOCAL_EMBED"] = "off"     # hashing = as-shipped floor
        if cond == "grimoire-ollama":
            os.environ["GRIMOIRE_OLLAMA_URL"] = OLLAMA_URL
            os.environ["GRIMOIRE_EMBED_MODEL"] = "nomic-embed-text"
        elif cond == "grimoire-local":
            os.environ["GRIMOIRE_LOCAL_EMBED"] = "auto"    # model2vec (pip extra)
        for ci, c in enumerate(data):
            todo = [q for q in qs if q["conv"] == ci and (q["qid"], cond) not in done]
            if not todo:
                continue
            if cond.startswith("grimoire"):
                from server import db
                tmp = Path(tempfile.mkdtemp(prefix=f"locomo-{cond}-"))
                _build_vault(c["conversation"], tmp / "vault")
                for q in todo:
                    ctx = _grimoire_context(q["question"])
                    out.write(json.dumps({"qid": q["qid"], "condition": cond,
                                          "context": ctx}) + "\n")
                db.close()
                shutil.rmtree(tmp, ignore_errors=True)
            else:
                full = "\n\n".join(
                    f"## Session {n} — {when}\n{text}"
                    for n, when, text in session_docs(c["conversation"]))
                for q in todo:
                    ctx = full if cond == "full" else ""
                    out.write(json.dumps({"qid": q["qid"], "condition": cond,
                                          "context": ctx}) + "\n")
            out.flush()
            print(f"retrieve {cond} conv{ci}: {len(todo)} contexts")
    out.close()


# ---- claude CLI -------------------------------------------------------------

_print_lock = threading.Lock()


def claude_call(prompt, model, timeout=240):
    """One CLI call from an empty cwd; returns (text, input_tokens)."""
    empty = Path(tempfile.gettempdir()) / "locomo-empty-cwd"
    empty.mkdir(exist_ok=True)
    # prompt goes via stdin: huge contexts (500k+ chars) exceed the kernel's
    # per-argv-string limit (MAX_ARG_STRLEN, 128 KiB) when passed as an arg
    p = subprocess.run(
        ["claude", "-p", "--model", model, "--output-format", "json",
         "--strict-mcp-config", "--max-turns", "1"],
        input=prompt, capture_output=True, text=True, timeout=timeout, cwd=empty)
    if p.returncode != 0:
        raise RuntimeError(f"claude exit {p.returncode}: {p.stderr[:300]}")
    j = json.loads(p.stdout)
    usage = j.get("usage") or {}
    toks = (usage.get("input_tokens", 0) + usage.get("cache_read_input_tokens", 0)
            + usage.get("cache_creation_input_tokens", 0))
    return (j.get("result") or "").strip(), toks


def _run_parallel(jobs, worker, outfile):
    out = outfile.open("a")
    lock = threading.Lock()
    done_n = [0]

    def wrapped(job):
        try:
            rec = worker(job)
        except Exception as e:
            with _print_lock:
                print(f"  ! {job.get('qid')}/{job.get('condition', '')}: {e}")
            return
        with lock:
            out.write(json.dumps(rec) + "\n")
            out.flush()
            done_n[0] += 1
            if done_n[0] % 25 == 0:
                print(f"  {done_n[0]}/{len(jobs)}")

    with ThreadPoolExecutor(PARALLEL) as ex:
        list(ex.map(wrapped, jobs))
    out.close()
    print(f"done {done_n[0]}/{len(jobs)} -> {outfile}")


# ---- phase: read ------------------------------------------------------------

def phase_read(limit=None, conditions=None):
    qs = {q["qid"]: q for q in map(json.loads, (RESULTS / "questions.jsonl").open())}
    ctxs = {(r["qid"], r["condition"]): r["context"]
            for r in map(json.loads, (RESULTS / "contexts.jsonl").open())}
    rfile = RESULTS / "reads.jsonl"
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, rfile.open())} if rfile.exists() else set()
    jobs = [{"qid": qid, "condition": cond, "context": ctx}
            for (qid, cond), ctx in sorted(ctxs.items())
            if (qid, cond) not in done
            and (not conditions or cond in conditions)]
    if limit:
        jobs = jobs[:limit]
    print(f"read: {len(jobs)} calls ({PARALLEL} parallel, model {READER_MODEL})")

    def worker(job):
        q = qs[job["qid"]]
        prompt = READER_PROMPT.format(context=job["context"], question=q["question"])
        text, toks = claude_call(prompt, READER_MODEL)
        return {"qid": job["qid"], "condition": job["condition"],
                "answer": text, "input_tokens": toks}

    _run_parallel(jobs, worker, rfile)


# ---- phase: judge -----------------------------------------------------------

def phase_judge(limit=None):
    qs = {q["qid"]: q for q in map(json.loads, (RESULTS / "questions.jsonl").open())}
    reads = [json.loads(x) for x in (RESULTS / "reads.jsonl").open()]
    jfile = RESULTS / "judged.jsonl"
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, jfile.open())} if jfile.exists() else set()
    jobs = [r for r in reads if (r["qid"], r["condition"]) not in done]
    if limit:
        jobs = jobs[:limit]
    print(f"judge: {len(jobs)} calls (model {JUDGE_MODEL})")

    def worker(job):
        q = qs[job["qid"]]
        prompt = JUDGE_PROMPT.format(question=q["question"], gold=q["gold"],
                                     answer=job["answer"] or "(no answer)")
        text, _ = claude_call(prompt, JUDGE_MODEL)
        m = re.search(r'"correct"\s*:\s*(true|false)', text)
        if not m:
            raise RuntimeError(f"unparseable judge output: {text[:120]}")
        return {"qid": job["qid"], "condition": job["condition"],
                "correct": m.group(1) == "true"}

    _run_parallel(jobs, worker, jfile)


# ---- phase: report ----------------------------------------------------------

CATS = {1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop"}


def phase_report():
    qs = {q["qid"]: q for q in map(json.loads, (RESULTS / "questions.jsonl").open())}
    toks = {(r["qid"], r["condition"]): r["input_tokens"]
            for r in map(json.loads, (RESULTS / "reads.jsonl").open())}
    acc = collections.defaultdict(lambda: [0, 0])       # (cond, cat) -> [ok, n]
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

    conds = [c for c in CONDITIONS if tot[c][1]]
    hdr = f"{'condition':18} " + " ".join(f"{CATS[c]:>12}" for c in sorted(CATS)) \
        + f" {'overall':>12} {'med tokens':>10}"
    print(hdr)
    print("-" * len(hdr))
    summary = {}
    for cond in conds:
        row = f"{cond:18} "
        for cat in sorted(CATS):
            ok, n = acc[(cond, cat)]
            row += f"{ok / n * 100 if n else 0:11.1f}% "
        ok, n = tot[cond]
        med = sorted(tk[cond])[len(tk[cond]) // 2] if tk[cond] else 0
        row += f"{ok / n * 100 if n else 0:11.1f}% {med:>10}"
        print(row)
        summary[cond] = {
            "overall": {"correct": ok, "n": n, "pct": round(ok / n * 100, 1) if n else 0},
            "by_category": {CATS[c]: {"correct": acc[(cond, c)][0], "n": acc[(cond, c)][1]}
                            for c in sorted(CATS)},
            "median_input_tokens": med}
    (RESULTS / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(f"\nwrote {RESULTS / 'summary.json'}")


# ---- main -------------------------------------------------------------------

if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("phase", choices=["sample", "retrieve", "read", "judge", "report"])
    ap.add_argument("--limit", type=int)
    ap.add_argument("--conditions", type=str,
                    help="comma-separated subset of conditions for `read`")
    a = ap.parse_args()
    if a.phase == "sample":
        phase_sample()
    elif a.phase == "retrieve":
        phase_retrieve()
    elif a.phase == "read":
        phase_read(a.limit, a.conditions.split(",") if a.conditions else None)
    elif a.phase == "judge":
        phase_judge(a.limit)
    else:
        phase_report()
