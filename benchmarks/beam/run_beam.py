#!/usr/bin/env python3
"""BEAM long-term-memory benchmark harness — see README.md for the protocol.

    python run_beam.py dry-run                       # offline, synthetic fixture
    python run_beam.py download [--max-rows N]       # fetch BEAM from Hugging Face
    python run_beam.py run --data DIR --arms none,full,grimoire --binary ../../go/grimoire
    python run_beam.py report --out DIR

Arms (see README "Conditions"):

    none      no context                                   (floor)
    full      the whole conversation, cut to a fixed budget (null baseline)
    grimoire  the context Grimoire retrieval returns for the raw question

Every arm answers with the same reader and is graded by the same judge. The
judge is one call per question that marks each rubric item satisfied or not;
a question scores the fraction of items satisfied.

Nothing here reports a number on its own. `dry-run` uses a stub model and a
synthetic fixture: its output proves the plumbing runs, and says nothing about
memory quality.
"""
from __future__ import annotations

import argparse
import ast
import collections
import json
import os
import re
import statistics
import subprocess
import sys
import tempfile
import threading
import urllib.parse
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))             # goserver
sys.path.insert(0, str(HERE.parent / "locomo"))  # retrieve_go.api/context_for, reader/judge model ids

import goserver  # noqa: E402
from run_locomo import JUDGE_MODEL, READER_MODEL  # noqa: E402  (same models as LoCoMo)

DATA_DIR = Path(os.environ.get("BEAM_DATA", HERE / "data"))
RESULTS = Path(os.environ.get("BEAM_RESULTS", HERE / "results"))
FIXTURE = HERE / "fixtures" / "synthetic_beam.json"
HF_ROWS = "https://datasets-server.huggingface.co/rows"
# (dataset repo, split). The 10M tier lives in its own repo.
SOURCES = [("Mohammadta/BEAM", "100K"), ("Mohammadta/BEAM", "500K"),
           ("Mohammadta/BEAM", "1M"), ("Mohammadta/BEAM-10M", "10M")]
ABILITIES = ["abstention", "contradiction_resolution", "event_ordering",
             "information_extraction", "instruction_following", "knowledge_update",
             "multi_session_reasoning", "preference_following", "summarization",
             "temporal_reasoning"]
ABILITY_SHORT = {a: a[:11] for a in ABILITIES}
# Which field holds the reference answer differs by ability in the released data.
ANSWER_KEYS = ("answer", "ideal_response", "ideal_answer", "ideal_summary")
ARMS = ("none", "full", "grimoire")
FULL_BUDGET_CHARS = 400_000          # ~100k tokens at chars/4
GRIMOIRE_PORT = 9131
PARALLEL = int(os.environ.get("BEAM_PARALLEL", "12"))

READER_PROMPT = """You are answering a question about a long conversation between a user and an AI assistant. Use ONLY the conversation below.

<conversation>
{context}
</conversation>

Question: {question}

Answer the question completely: include every part it asks for. If the conversation does not contain the information needed, say so plainly instead of guessing. Keep the answer under 200 words. Do not use tools."""

JUDGE_PROMPT = """You are grading a model's answer to a question about a long conversation.

Question: {question}
Model answer: {answer}

Rubric items (each states something a correct answer must convey or do):
{items}

For each numbered rubric item, decide whether the model answer satisfies it. Minor wording differences are fine. A hedge that does not commit to the content does not satisfy an item. Reply with JSON only: {{"satisfied": [true, false, ...]}} containing exactly {n} booleans, in item order."""

SYSTEM = ("You answer questions about a long conversation using only the text provided, "
          "and you grade answers exactly as instructed. Output only what is asked.")


# ---- dataset ----------------------------------------------------------------

def _literal(v):
    """The released data stores probing questions and rubrics as Python-literal
    strings; accept JSON too in case a mirror re-serialised them."""
    if isinstance(v, str):
        try:
            return ast.literal_eval(v)
        except (ValueError, SyntaxError):
            return json.loads(v)
    return v


def normalize(row: dict, source: str) -> dict:
    """One BEAM row -> {conv, batches, questions}. Questions are flattened with
    stable qids; questions without a rubric cannot be graded and are skipped."""
    cid = str(row["conversation_id"])
    batches = []
    for bi, batch in enumerate(row["chat"], 1):
        msgs = [{"role": m.get("role", ""), "content": m.get("content", "")} for m in batch]
        anchor = next((m.get("time_anchor") for m in batch if m.get("time_anchor")), None)
        batches.append({"batch": bi, "time_anchor": anchor, "messages": msgs})

    probing = _literal(row.get("probing_questions")) or {}
    questions, skipped = [], 0
    for ab in ABILITIES:
        for i, item in enumerate(probing.get(ab, [])):
            rubric = _literal(item.get("rubric")) or []
            if isinstance(rubric, str):
                rubric = [rubric]
            if not rubric:
                skipped += 1
                continue
            ref = next((item[k] for k in ANSWER_KEYS if item.get(k)), None)
            if ref is not None and not isinstance(ref, str):
                ref = json.dumps(ref)
            questions.append({"qid": f"{cid}:{ab}:{i}", "conv": cid, "ability": ab,
                              "question": item["question"], "reference": ref,
                              "rubric": [str(r) for r in rubric]})
    return {"conv": cid, "source": source, "batches": batches,
            "questions": questions, "skipped_unrubriced": skipped}


def load_conversations(path) -> list[dict]:
    p = Path(path)
    files = (sorted(p.glob("*.jsonl")) + sorted(p.glob("*.json"))) if p.is_dir() else [p]
    if not files:
        raise FileNotFoundError(f"no .json/.jsonl BEAM files under {p}")
    convs = []
    for f in files:
        if f.suffix == ".jsonl":
            rows = [json.loads(line) for line in f.read_text().splitlines() if line.strip()]
        else:
            d = json.loads(f.read_text())
            rows = d.get("rows", d) if isinstance(d, dict) else d
        for r in rows:
            r = r["row"] if "row" in r and "chat" not in r else r   # datasets-server wrapper
            convs.append(normalize(r, f.stem))
    return convs


def download(out_dir: Path, splits=None, max_rows=None) -> None:
    """Page the Hugging Face datasets-server (public, no token) into JSONL, one
    file per split. Writes to .part then renames, so a dead connection never
    leaves a file that looks complete."""
    out_dir.mkdir(parents=True, exist_ok=True)
    for repo, split in SOURCES:
        if splits and split not in splits:
            continue
        dest = out_dir / f"{repo.split('/')[1]}-{split}.jsonl"
        part = dest.with_suffix(".jsonl.part")
        n, offset, total = 0, 0, None
        with part.open("w") as f:
            while (total is None or offset < total) and not (max_rows and n >= max_rows):
                q = urllib.parse.urlencode({"dataset": repo, "config": "default",
                                            "split": split, "offset": offset, "length": 5})
                with urllib.request.urlopen(f"{HF_ROWS}?{q}", timeout=300) as r:
                    d = json.load(r)
                total = d["num_rows_total"]
                rows = d.get("rows", [])
                if not rows:
                    break
                if max_rows:
                    rows = rows[: max_rows - n]
                for item in rows:
                    f.write(json.dumps(item["row"]) + "\n")
                    n += 1
                offset += len(rows)
        part.rename(dest)
        print(f"{repo} [{split}] -> {dest} ({n} rows of {total})")


# ---- models (pluggable endpoint) -------------------------------------------

def _claude(prompt: str, model: str, timeout: int = 300) -> str:
    """Same CLI invocation as LoCoMo's claude_call, with a system prompt that
    does not force brevity (summaries need room)."""
    empty = Path(tempfile.gettempdir()) / "beam-empty-cwd"
    empty.mkdir(exist_ok=True)
    p = subprocess.run(
        ["claude", "-p", "--model", model, "--output-format", "json",
         "--strict-mcp-config", "--max-turns", "1", "--tools", "",
         "--system-prompt", SYSTEM],
        input=prompt, capture_output=True, text=True, timeout=timeout, cwd=empty)
    if p.returncode != 0:
        raise RuntimeError(f"claude exit {p.returncode}: {p.stderr[:300]}")
    return (json.loads(p.stdout).get("result") or "").strip()


def _openai(base_url: str, model: str, prompt: str, timeout: int = 600) -> str:
    body = json.dumps({"model": model, "temperature": 0, "max_tokens": 1024,
                       "messages": [{"role": "system", "content": SYSTEM},
                                    {"role": "user", "content": prompt}]}).encode()
    req = urllib.request.Request(base_url.rstrip("/") + "/v1/chat/completions", data=body,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return (json.load(r)["choices"][0]["message"]["content"] or "").strip()


def _parse_flags(text: str, n: int) -> list[bool]:
    m = re.search(r"\{.*\}", text, re.S)
    if not m:
        raise RuntimeError(f"no JSON in judge output: {text[:120]!r}")
    flags = json.loads(m.group(0))["satisfied"]
    if len(flags) != n or not all(isinstance(x, bool) for x in flags):
        raise RuntimeError(f"judge returned {len(flags)} flags for {n} items")
    return flags


def _norm(s: str) -> str:
    return " ".join(re.sub(r"[^a-z0-9 ]+", " ", s.lower()).split())


class Model:
    """Two endpoints for real runs (`claude`, `openai`), and two stubs that
    exist only to test the harness: `stub` answers nothing useful, and
    `stub-oracle` answers with the reference, so a correct pipeline must score
    100%. Neither stub is a model and neither produces a result."""

    def __init__(self, backend: str, model: str, judge: str, base_url: str | None = None,
                 fixture_refs: dict | None = None):
        if backend not in ("claude", "openai", "stub", "stub-oracle"):
            raise ValueError(backend)
        self.backend, self.model, self.judge = backend, model, judge
        self.base_url = base_url
        self.refs = fixture_refs or {}

    def read(self, prompt: str, qid: str | None = None) -> str:
        if self.backend == "claude":
            return _claude(prompt, self.model)
        if self.backend == "openai":
            return _openai(self.base_url, self.model, prompt)
        if self.backend == "stub-oracle":
            return self.refs.get(qid) or "I don't know."
        return "I don't know."

    def grade(self, question: str, answer: str, rubric: list[str]) -> list[bool]:
        if self.backend in ("stub", "stub-oracle"):
            a = _norm(answer)
            return [_norm(re.sub(r"^LLM response should state:\s*", "", r, flags=re.I)) in a
                    for r in rubric]
        items = "\n".join(f"{i}. {r}" for i, r in enumerate(rubric, 1))
        prompt = JUDGE_PROMPT.format(question=question, answer=answer, items=items,
                                     n=len(rubric))
        text = _claude(prompt, self.judge) if self.backend == "claude" \
            else _openai(self.base_url, self.judge, prompt)
        return _parse_flags(text, len(rubric))


# ---- contexts ---------------------------------------------------------------

def transcript(conv: dict) -> str:
    parts = []
    for b in conv["batches"]:
        head = f"## Batch {b['batch']}" + (f" — {b['time_anchor']}" if b["time_anchor"] else "")
        lines = "\n".join(f"{m['role']}: {m['content']}" for m in b["messages"])
        parts.append(f"{head}\n{lines}")
    return "\n\n".join(parts)


def full_context(conv: dict, budget: int = FULL_BUDGET_CHARS) -> str:
    """Null baseline: the whole transcript, cut to the last `budget` characters.
    Truncation keeps the most recent text, the usual failure mode of long-context
    readers that get handed more than fits."""
    text = transcript(conv)
    if len(text) <= budget:
        return text
    return "[... earlier conversation omitted to fit the budget ...]\n" + text[-budget:]


def build_vault(vault: Path, conv: dict) -> None:
    """One note per batch, date in the title, as LoCoMo does per session."""
    vault.mkdir(parents=True, exist_ok=True)
    for p in vault.glob("*.md"):
        p.unlink()
    for b in conv["batches"]:
        when = b["time_anchor"] or "undated"
        body = "\n".join(f"{m['role']}: {m['content']}" for m in b["messages"])
        (vault / f"batch-{b['batch']:02d}.md").write_text(
            f"---\ntitle: Conversation batch {b['batch']} ({when})\n---\n"
            f"Date: {when}\n\n{body}\n", encoding="utf-8")


# ---- phases -----------------------------------------------------------------

def read_jsonl(path: Path) -> list[dict]:
    if not path.exists():
        return []
    with path.open() as f:
        return [json.loads(line) for line in f if line.strip()]


def _done(path: Path) -> set:
    return {r["qid"] for r in read_jsonl(path)}


def _append(f, lock, rec):
    with lock:
        f.write(json.dumps(rec) + "\n")
        f.flush()


def _read_batch(model, pairs, f, lock, arm, parallel):
    """pairs: [(question, context)]. Reader calls run in parallel; failures are
    logged and left out, so a re-run picks them up."""
    def work(pair):
        q, ctx = pair
        prompt = READER_PROMPT.format(context=ctx, question=q["question"])
        try:
            text = model.read(prompt, q["qid"])
        except Exception as e:  # noqa: BLE001
            print(f"  ! read {q['qid']} [{arm}]: {e}")
            return
        _append(f, lock, {"qid": q["qid"], "arm": arm, "answer": text,
                          "context_chars": len(ctx), "approx_tokens": len(ctx) // 4})

    with ThreadPoolExecutor(parallel) as ex:
        list(ex.map(work, pairs))


def run_arm(arm, convs, model, out: Path, args) -> None:
    afile = out / arm / "answers.jsonl"
    afile.parent.mkdir(parents=True, exist_ok=True)
    done = _done(afile)
    lock = threading.Lock()
    print(f"[{arm}] {sum(len(c['questions']) for c in convs) - len(done)} reads to do")
    with afile.open("a") as f:
        if arm == "none":
            for conv in convs:
                todo = [q for q in conv["questions"] if q["qid"] not in done]
                _read_batch(model, [(q, "") for q in todo], f, lock, arm, args.parallel)
        elif arm == "full":
            for conv in convs:
                todo = [q for q in conv["questions"] if q["qid"] not in done]
                ctx = full_context(conv)
                _read_batch(model, [(q, ctx) for q in todo], f, lock, arm, args.parallel)
        elif arm == "grimoire":
            from retrieve_go import api, context_for  # LoCoMo's exact context assembly
            vault = out / "grimoire-vault"
            with goserver.launch(args.binary, vault, args.port, args.embed,
                                 log=out / "grimoire-server.log") as base:
                for conv in convs:
                    todo = [q for q in conv["questions"] if q["qid"] not in done]
                    if not todo:
                        continue
                    build_vault(vault, conv)
                    api(base, "/api/reindex", {})
                    pairs = [(q, context_for(base, q["question"])) for q in todo]
                    _read_batch(model, pairs, f, lock, arm, args.parallel)
        else:
            raise ValueError(arm)


def judge_arm(arm, qs, model, out: Path, args) -> None:
    afile, jfile = out / arm / "answers.jsonl", out / arm / "judged.jsonl"
    if not afile.exists():
        print(f"[{arm}] no answers yet, skipping judge")
        return
    done = _done(jfile)
    jobs = [r for r in read_jsonl(afile) if r["qid"] not in done]
    lock = threading.Lock()
    print(f"[{arm}] {len(jobs)} judge calls")

    def work(r, jf):
        q = qs[r["qid"]]
        try:
            flags = model.grade(q["question"], r["answer"] or "(no answer)", q["rubric"])
        except Exception as e:  # noqa: BLE001
            print(f"  ! judge {r['qid']} [{arm}]: {e}")
            return
        _append(jf, lock, {"qid": r["qid"], "arm": arm, "flags": flags,
                           "score": sum(flags) / len(flags), "all": all(flags)})

    with jfile.open("a") as jf, ThreadPoolExecutor(args.parallel) as ex:
        list(ex.map(lambda r: work(r, jf), jobs))


def report(out: Path, arms, qs: dict) -> dict:
    summary = {}
    header = f"{'arm':10} " + " ".join(f"{ABILITY_SHORT[a]:>12}" for a in ABILITIES) \
        + f" {'overall':>9} {'strict':>8} {'n':>5} {'med ctx tok':>12}"
    print("Mean rubric satisfaction (%) per ability; strict = all rubric items met")
    print(header)
    print("-" * len(header))
    for arm in arms:
        jfile, afile = out / arm / "judged.jsonl", out / arm / "answers.jsonl"
        if not jfile.exists():
            continue
        judged = read_jsonl(jfile)
        by = collections.defaultdict(list)
        for r in judged:
            by[qs[r["qid"]]["ability"]].append(r)
        toks = [r["approx_tokens"] for r in read_jsonl(afile)]
        med_tok = int(statistics.median(toks)) if toks else 0
        cells, per = [], {}
        for a in ABILITIES:
            rs = by.get(a, [])
            pct = 100 * statistics.fmean(r["score"] for r in rs) if rs else None
            per[a] = {"n": len(rs), "score_pct": round(pct, 1) if pct is not None else None,
                      "strict_pct": round(100 * sum(r["all"] for r in rs) / len(rs), 1) if rs else None}
            cells.append(f"{pct:11.1f}%" if pct is not None else f"{'-':>12}")
        n = len(judged)
        overall = 100 * statistics.fmean(r["score"] for r in judged) if judged else 0
        strict = 100 * sum(r["all"] for r in judged) / n if judged else 0
        print(f"{arm:10} " + " ".join(cells) + f" {overall:8.1f}% {strict:7.1f}% {n:>5} {med_tok:>12}")
        summary[arm] = {"overall_score_pct": round(overall, 1), "overall_strict_pct": round(strict, 1),
                        "n": n, "median_approx_context_tokens": med_tok, "by_ability": per}
    return summary


# ---- commands ---------------------------------------------------------------

def cmd_run(args, dry: bool) -> int:
    if dry:
        src = FIXTURE
        backend = args.backend
        out = Path(args.out) if args.out else Path(tempfile.mkdtemp(prefix="beam-dry-"))
        arms = args.arms.split(",")
        if "grimoire" in arms and not args.binary:
            print("dry-run: no --binary, so the grimoire arm is skipped")
            arms.remove("grimoire")
    else:
        src = Path(args.data) if args.data else DATA_DIR
        backend = args.backend
        out = Path(args.out) if args.out else RESULTS
        arms = args.arms.split(",")
    for a in arms:
        if a not in ARMS:
            raise SystemExit(f"unknown arm {a!r}; choose from {ARMS}")
    if "grimoire" in arms and not args.binary:
        raise SystemExit("the grimoire arm needs --binary (path to the grimoire server)")

    convs = load_conversations(src)
    if args.limit:
        convs = convs[: args.limit]
    qs = {q["qid"]: q for c in convs for q in c["questions"]}
    refs = {qid: (q["reference"] or " ".join(
        re.sub(r"^LLM response should state:\s*", "", r, flags=re.I) for r in q["rubric"]))
        for qid, q in qs.items()}
    model_name = args.model or READER_MODEL
    model = Model(backend, model_name, args.judge_model or JUDGE_MODEL,
                  base_url=args.base_url, fixture_refs=refs)
    args.parallel = args.parallel or PARALLEL
    banner = ("DRY RUN - synthetic fixture, stub model. Plumbing check only, NOT a result."
              if dry else "")
    print(f"{banner}\n" if banner else "", end="")
    print(f"data={src} conversations={len(convs)} questions={len(qs)} arms={arms} "
          f"backend={backend} reader={model_name} judge={model.judge} out={out}")
    out.mkdir(parents=True, exist_ok=True)
    qfile = out / "questions.jsonl"
    if not qfile.exists():
        with qfile.open("w") as f:
            for q in qs.values():
                f.write(json.dumps(q) + "\n")

    for arm in arms:
        run_arm(arm, convs, model, out, args)
    for arm in arms:
        judge_arm(arm, qs, model, out, args)
    summary = report(out, arms, qs)
    skipped = sum(c["skipped_unrubriced"] for c in convs)
    meta = {"dry_run": dry, "status": "dry-run-not-a-result" if dry else "run",
            "backend": backend, "reader": model_name, "judge": model.judge,
            "conversations": len(convs), "questions": len(qs),
            "skipped_unrubriced": skipped, "arms": summary}
    (out / "summary.json").write_text(json.dumps(meta, indent=2) + "\n")
    print(f"\nwrote {out / 'summary.json'}")
    return 0


def cmd_report(args) -> int:
    out = Path(args.out) if args.out else RESULTS
    convs = load_conversations(Path(args.data) if args.data else DATA_DIR)
    qs = {q["qid"]: q for c in convs for q in c["questions"]}
    report(out, list(ARMS), qs)
    return 0


def main(argv=None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    sub = ap.add_subparsers(dest="cmd", required=True)

    dr = sub.add_parser("dry-run", help="offline run on the synthetic fixture")
    dr.add_argument("--arms", default="none,full",
                    help="default none,full; add grimoire with --binary")
    dr.add_argument("--backend", default="stub", choices=["stub", "stub-oracle"],
                    help="stub-oracle answers with the reference: a ceiling check for the "
                         "scoring path, not a model")
    dr.add_argument("--binary", help="grimoire server binary (optional)")
    dr.add_argument("--embed", default="off", choices=["off", "auto", "ollama"])
    dr.add_argument("--port", type=int, default=GRIMOIRE_PORT)
    dr.add_argument("--limit", type=int, help="first N conversations")
    dr.add_argument("--parallel", type=int)
    dr.add_argument("--out", help="default: a fresh temp dir")

    d = sub.add_parser("download", help="fetch BEAM from Hugging Face")
    d.add_argument("--out", default=str(DATA_DIR))
    d.add_argument("--splits", help="comma-separated, e.g. 1M,10M (default all)")
    d.add_argument("--max-rows", type=int, help="cap rows per split (testing)")

    r = sub.add_parser("run", help="real run")
    r.add_argument("--data", help="dir or file of BEAM rows (default: benchmarks/beam/data)")
    r.add_argument("--arms", default="none,full,grimoire")
    r.add_argument("--backend", default="claude", choices=["claude", "openai"])
    r.add_argument("--model", help=f"reader model (default {READER_MODEL})")
    r.add_argument("--judge-model", help=f"judge model (default {JUDGE_MODEL})")
    r.add_argument("--base-url", help="openai-compatible server, e.g. http://host:8080")
    r.add_argument("--binary", help="grimoire server binary for the grimoire arm")
    r.add_argument("--embed", default="auto", choices=["off", "auto", "ollama"])
    r.add_argument("--port", type=int, default=GRIMOIRE_PORT)
    r.add_argument("--limit", type=int, help="first N conversations (smoke test)")
    r.add_argument("--parallel", type=int)
    r.add_argument("--out")

    rp = sub.add_parser("report", help="print the table from an existing run")
    rp.add_argument("--out")
    rp.add_argument("--data")

    args = ap.parse_args(argv)
    if args.cmd == "download":
        download(Path(args.out), set(args.splits.split(",")) if args.splits else None,
                 args.max_rows)
        return 0
    if args.cmd == "dry-run":
        args.data, args.model = None, None
        args.judge_model = args.base_url = None
        return cmd_run(args, dry=True)
    if args.cmd == "run":
        return cmd_run(args, dry=False)
    return cmd_report(args)


if __name__ == "__main__":
    raise SystemExit(main())
