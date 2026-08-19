#!/usr/bin/env python3
"""Evidence recall on the held-out LongMemEval dev split.

Why this metric, and why a dev split
------------------------------------
The scored 200-question sample is frozen (results/questions.jsonl) and must
never be tuned against. LongMemEval ships 500 questions; removing the 30
abstention items and the 200 scored ones leaves 270 held-out questions, which
is what this measures. No question here is ever scored in a report.

Every LongMemEval instance marks the individual TURNS its answer depends on
(`has_answer` on a turn) as well as the sessions. That makes a retrieval
metric available that needs no LLM at all: of the turns a question's answer
actually depends on, how many appear in the context retrieval hands the
reader? Being LLM-free makes it cheap, deterministic and free of judge
variance — the properties you want when comparing two retrieval
implementations rather than two models.

Turn level is the level that matters, and measuring at session level first was
a mistake worth recording: sessions are ~2k tokens and retrieval returns
CHUNKS, so a question can have every evidence SESSION represented in its
context while the particular chunk holding the fact was never retrieved.
Session coverage read 94.8% on this split where turn coverage reads far lower.
Both are reported; the turn numbers are the ones to act on.

A turn counts as present under the same rule the LoCoMo dev harness uses: the
first 120 characters of its normalized text appear in the normalized context.

The number of distinct SESSIONS in the context is reported too. Coverage alone
proved to be an insufficient gate: a change that left LoCoMo coverage flat cost
it 20.7 points of multi-hop accuracy, and the only thing that had moved was
breadth. Depth and breadth trade against each other inside a fixed budget, so
both belong in the report.

Two numbers per level, and the second is the point:

  recall        mean fraction of a question's evidence turns retrieved
  FULL coverage fraction of questions where EVERY evidence turn was retrieved

For a counting or aggregation question ("how many X did I ...") partial recall
is worth nothing: a reader that sees four of the five kitchen repairs answers
"four" with total confidence. Mean recall hides that; full coverage does not.

Usage:
    python dev_session_recall.py --binary ../../go/grimoire --vault /tmp/lme-dev \
        --k 10 --search-limit 5 [--limit N] [--tag baseline]
"""
from __future__ import annotations

import argparse
import json
import re
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent))

import goserver  # noqa: E402
from retrieve_go import build_vault  # noqa: E402  (identical ingestion)
from run_lme import RESULTS, load_data  # noqa: E402

# Enumerative questions are the ones that need WIDE retrieval rather than deep:
# the answer is a count, a total, or a set, so missing one piece of evidence
# changes the answer. Detected lexically here purely to SLICE the report — no
# product behaviour depends on this list.
ENUM_RE = re.compile(
    r"\b(how many|how much|how often|how frequently|total number|in total|"
    r"altogether|list all|name all|all the|every time|number of|"
    r"what percentage|combined)\b", re.I)


def api(base: str, path: str, body=None, timeout=600):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + path, data=data, method="POST" if data else "GET",
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        raw = r.read().decode()
    return json.loads(raw) if raw.strip() else {}


def context_for(base: str, question: str, k: int, search_limit: int) -> str:
    """The shipped recipe from retrieve_go.context_for, with k and the search
    limit exposed so a larger-top-k ablation needs no product change."""
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


# Notes are written as session-NNN.md and titled "Chat session N+1 (date)", so
# the retrieved context names its sessions. Reading the indices back out of the
# context measures exactly what the READER can see, not what an internal API
# happened to return.
SESSION_RE = re.compile(r"### Chat session (\d+) \(")


def covered_indices(context: str) -> set[int]:
    return {int(m) - 1 for m in SESSION_RE.findall(context)}


def evidence_indices(inst) -> set[int]:
    pos = {sid: i for i, sid in enumerate(inst["haystack_session_ids"])}
    return {pos[s] for s in inst["answer_session_ids"] if s in pos}


def norm(s: str) -> str:
    return re.sub(r"\s+", " ", s).strip().lower()


def evidence_turns(inst) -> list[str]:
    """The turns the answer actually depends on, as they appear in a note."""
    out = []
    for sess in inst["haystack_sessions"]:
        for t in sess:
            if str(t.get("has_answer")).lower() == "true":
                txt = norm(t.get("content") or "")
                if txt:
                    out.append(txt)
    return out


def turns_found(ctx_norm: str, turns: list[str]) -> int:
    """Same presence rule as the LoCoMo dev harness: the first 120 normalized
    characters of the turn appear in the normalized context."""
    return sum(1 for t in turns if t[:120] in ctx_norm)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url")
    ap.add_argument("--binary")
    ap.add_argument("--embed", default="auto", choices=("off", "auto", "ollama"))
    ap.add_argument("--port", type=int, default=9131)
    ap.add_argument("--vault", required=True)
    ap.add_argument("--k", type=int, default=10)
    ap.add_argument("--search-limit", type=int, default=5)
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--tag", default="run")
    ap.add_argument("--out", default=str(HERE / "dev_results"))
    a = ap.parse_args()

    data = load_data()
    scored = {json.loads(line)["qid"] for line in (RESULTS / "questions.jsonl").open()}
    dev = [(i, x) for i, x in enumerate(data)
           if not str(x["question_id"]).endswith("_abs")
           and x["question_id"] not in scored]
    if a.limit:
        dev = dev[: a.limit]
    print(f"dev questions: {len(dev)}  (k={a.k}, search_limit={a.search_limit}, "
          f"embed={a.embed}, tag={a.tag})")

    outdir = Path(a.out)
    outdir.mkdir(parents=True, exist_ok=True)
    rows = []

    def sweep(base: str):
        t0 = time.monotonic()
        for n, (_idx, inst) in enumerate(dev, 1):
            build_vault(Path(a.vault), inst)
            api(base, "/api/reindex", {})
            ctx = context_for(base, inst["question"], a.k, a.search_limit)
            ctx_norm = norm(ctx)
            ev = evidence_indices(inst)
            got = covered_indices(ctx)
            hit = ev & got
            turns = evidence_turns(inst)
            tfound = turns_found(ctx_norm, turns)
            rows.append({
                "qid": inst["question_id"],
                "category": inst["question_type"],
                "enumerative": bool(ENUM_RE.search(inst["question"])),
                "n_turns": len(turns),
                "n_turns_hit": tfound,
                "turn_recall": (tfound / len(turns)) if turns else None,
                "turn_full": bool(turns) and tfound == len(turns),
                "n_evidence": len(ev),
                "n_hit": len(hit),
                "recall": (len(hit) / len(ev)) if ev else None,
                "full": bool(ev) and hit == ev,
                "ctx_chars": len(ctx),
                "n_sessions_in_ctx": len(got),
            })
            if n % 20 == 0:
                el = time.monotonic() - t0
                print(f"  {n}/{len(dev)}  {el:.0f}s  ({el/n:.1f}s/q)")

    if a.base_url:
        sweep(a.base_url)
    elif a.binary:
        with goserver.launch(a.binary, a.vault, a.port, a.embed) as base:
            sweep(base)
    else:
        return ap.error("give --base-url or --binary")

    path = outdir / f"{a.tag}.jsonl"
    with path.open("w") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")
    report(rows, a.tag)
    print(f"\nraw: {path}")
    return 0


def report(rows, tag):
    def agg(sel):
        s = [r for r in sel if r["turn_recall"] is not None]
        if not s:
            return None
        return (len(s),
                100 * sum(r["turn_recall"] for r in s) / len(s),
                100 * sum(1 for r in s if r["turn_full"]) / len(s),
                100 * sum(1 for r in s if r["full"]) / len(s),
                sum(r["ctx_chars"] for r in s) / len(s),
                sum(r["n_sessions_in_ctx"] for r in s) / len(s))

    print(f"\n=== {tag} — evidence recall on held-out dev ===")
    hdr = (f"{'slice':30} {'n':>4} {'turn rec':>9} {'TURN full':>10} "
           f"{'sess full':>10} {'ctx chars':>10} {'sessions':>9}")
    print(hdr); print("-" * len(hdr))
    for cat in sorted({r["category"] for r in rows}):
        a = agg([r for r in rows if r["category"] == cat])
        if a:
            print(f"{cat:30} {a[0]:>4} {a[1]:>8.1f}% {a[2]:>9.1f}% "
                  f"{a[3]:>9.1f}% {a[4]:>10.0f} {a[5]:>9.2f}")
    print("-" * len(hdr))
    for name, sel in (("ENUMERATIVE", [r for r in rows if r["enumerative"]]),
                      ("non-enumerative", [r for r in rows if not r["enumerative"]]),
                      ("multi-turn evidence (>1)", [r for r in rows if r["n_turns"] > 1]),
                      ("ALL", rows)):
        a = agg(sel)
        if a:
            print(f"{name:30} {a[0]:>4} {a[1]:>8.1f}% {a[2]:>9.1f}% "
                  f"{a[3]:>9.1f}% {a[4]:>10.0f} {a[5]:>9.2f}")


if __name__ == "__main__":
    raise SystemExit(main())
