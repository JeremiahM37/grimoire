#!/usr/bin/env python3
"""Retrieval-recall diagnostic on the DEV split (LoCoMo cat 1-4 questions NOT
in the frozen scored sample). LoCoMo annotates each question with the dialog
turns that contain its answer (`evidence` dia_ids), so retrieval quality is
measurable locally with zero LLM calls: recall = fraction of evidence turns
whose text appears in the retrieved context.

Iteration on retrieval/chunking code is tuned against THIS split only; the
scored 500-question sample is never used for tuning (PROTOCOL.md).

    python dev_recall.py            # hashing embedder (as-shipped)
    python dev_recall.py --ollama   # nomic-embed-text via GRIMOIRE_OLLAMA_URL
"""
import argparse
import ast
import collections
import json
import os
import re
import shutil
import sys
import tempfile
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE.parent.parent))
os.environ["GRIMOIRE_NO_WATCHER"] = "1"

from run_locomo import (  # noqa: E402
    OLLAMA_URL,
    RESULTS,
    _build_vault,
    _grimoire_context,
    load_data,
)

CATS = {1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop"}


def dev_questions(data):
    sampled = {json.loads(x)["qid"] for x in (RESULTS / "questions.jsonl").open()}
    out = []
    for ci, c in enumerate(data):
        for qi, q in enumerate(c["qa"]):
            qid = f"c{ci}q{qi}"
            if int(q["category"]) == 5 or qid in sampled:
                continue
            try:
                ev = ast.literal_eval(str(q.get("evidence", "[]")))
            except (ValueError, SyntaxError):
                ev = []
            if ev:
                out.append({"qid": qid, "conv": ci, "category": int(q["category"]),
                            "question": q["question"], "evidence": ev})
    return out


def turn_texts(conv):
    """dia_id -> turn text (with photo caption, as ingested)."""
    out = {}
    for v in conv.values():
        if isinstance(v, list):
            for t in v:
                txt = t.get("text", "")
                if t.get("blip_caption"):
                    txt += f" [shared a photo: {t['blip_caption']}]"
                out[t["dia_id"]] = txt.strip()
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--embedder", choices=["hash", "local", "ollama"], default="hash")
    a = ap.parse_args()
    os.environ.pop("GRIMOIRE_OLLAMA_URL", None)
    os.environ["GRIMOIRE_LOCAL_EMBED"] = "off"
    if a.embedder == "ollama":
        os.environ["GRIMOIRE_OLLAMA_URL"] = OLLAMA_URL
        os.environ["GRIMOIRE_EMBED_MODEL"] = "nomic-embed-text"
    elif a.embedder == "local":
        os.environ["GRIMOIRE_LOCAL_EMBED"] = "auto"

    data = load_data()
    qs = dev_questions(data)
    print(f"dev questions: {len(qs)}  embedder: {a.embedder}")

    from server import db
    rec = collections.defaultdict(list)     # category -> per-question recall
    ctx_lens = []
    for ci, c in enumerate(data):
        mine = [q for q in qs if q["conv"] == ci]
        if not mine:
            continue
        turns = turn_texts(c["conversation"])
        tmp = Path(tempfile.mkdtemp(prefix="locomo-dev-"))
        _build_vault(c["conversation"], tmp / "vault")
        for q in mine:
            ctx = _grimoire_context(q["question"])
            ctx_lens.append(len(ctx))
            norm = re.sub(r"\s+", " ", ctx)
            hits = [1 for d in q["evidence"]
                    if turns.get(d) and re.sub(r"\s+", " ", turns[d]) in norm]
            rec[q["category"]].append(len(hits) / len(q["evidence"]))
        db.close()
        shutil.rmtree(tmp, ignore_errors=True)

    allr = [r for v in rec.values() for r in v]
    print(f"{'category':>12} {'n':>5} {'mean recall':>12} {'full recall':>12}")
    for cat in sorted(rec):
        v = rec[cat]
        print(f"{CATS[cat]:>12} {len(v):>5} {sum(v) / len(v) * 100:>11.1f}% "
              f"{sum(1 for r in v if r == 1) / len(v) * 100:>11.1f}%")
    print(f"{'ALL':>12} {len(allr):>5} {sum(allr) / len(allr) * 100:>11.1f}% "
          f"{sum(1 for r in allr if r == 1) / len(allr) * 100:>11.1f}%")
    print(f"median context chars: {sorted(ctx_lens)[len(ctx_lens) // 2]}")


if __name__ == "__main__":
    main()
