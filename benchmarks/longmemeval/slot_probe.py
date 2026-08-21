#!/usr/bin/env python3
"""Does the engine recognise the update, at all? — a reader-free measurement.

    python3 slot_probe.py --binary /tmp/grimoire-slots

LongMemEval's `knowledge-update` questions are built from a specific structure:
two sessions, months apart, in which the user states the SAME fact with a
different value. The dataset marks both — `answer_session_ids` names the
sessions and `has_answer` marks the turns.

That gives a measurement with no reader, no judge and no retrieval in it:

> given the earlier statement and then the later one, does write-time
> reconciliation supersede the first with the second?

It is a pure test of the mechanism, it has **zero sampling variance** — the
same input gives the same op forever — and it runs over the WHOLE dataset's
knowledge-update set rather than a 31-question sample, because nothing here
costs a model call.

Three outcomes per pair, and only one of them is right:

    UPDATE   the later statement superseded the earlier — recall now returns
             one value, the current one
    ADD      both are on file; recall returns both and the reader must choose,
             which is the failure LongMemEval's category is designed to expose
    NOOP     the engine thought they said the same thing — the worst outcome,
             because the NEW value was discarded
"""
from __future__ import annotations

import argparse
import itertools
import json
import shutil
import sys
import urllib.request
from collections import Counter
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE.parent))
from goserver import launch  # noqa: E402

DATA = Path("/tmp/lme_dl/longmemeval_s.json")
PORT = 9166

_topic_n = itertools.count()


def next_topic() -> int:
    return next(_topic_n)


def post(base, path, body):
    req = urllib.request.Request(
        base + path, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.loads(r.read().decode() or "{}")


def evidence_pairs(entry: dict) -> list[tuple[str, str]]:
    """The user turns the dataset marks as carrying the answer, in date order.

    Only turns flagged `has_answer` are used. Taking whole sessions would
    measure the extractor's ability to find the needle in a session, which is a
    different question from the one asked here.
    """
    ids = entry.get("haystack_session_ids") or []
    dates = entry.get("haystack_dates") or []
    marked = []
    for sid in entry.get("answer_session_ids") or []:
        if sid not in ids:
            continue
        i = ids.index(sid)
        turns = entry["haystack_sessions"][i]
        for t in turns:
            if t.get("has_answer") and (t.get("role") or "") == "user":
                marked.append((dates[i] if i < len(dates) else "", t["content"].strip()))
    marked.sort(key=lambda p: p[0])
    if len(marked) < 2:
        return []
    # Consecutive pairs: with three statements of one fact, each supersedes its
    # predecessor, and that is two chances to get it right rather than one.
    return [(marked[i][1], marked[i + 1][1]) for i in range(len(marked) - 1)]


def probe(base: str, prev: str, nxt: str) -> str:
    """Write both statements into a fresh topic and report what the second did."""
    # `scope: topic` is essential, not tidiness. Reconciliation is vault-wide
    # by default, so without it every pair is also compared against every pair
    # written before it in the same server — pair 300 could be "superseded" by
    # pair 12, and both the recall and the false-positive numbers would be
    # measuring contamination. The topic is a counter rather than a hash of the
    # text: Python's string hash is per-process randomized, so two pairs could
    # collide into one topic and reintroduce exactly what this prevents.
    topic = f"p{next_topic()}"
    post(base, "/api/memory", {"topic": topic, "agent": "probe", "scope": "topic",
                               "text": prev[:19000]})
    out = post(base, "/api/memory", {"topic": topic, "agent": "probe", "scope": "topic",
                                     "text": nxt[:19000]})
    # The headline op is the first fact's outcome; a multi-sentence statement
    # produces several, and an UPDATE anywhere in it means the engine caught
    # the change.
    ops = [r.get("op") for r in (out.get("results") or [])] or [out.get("op")]
    for want in ("UPDATE", "DELETE"):
        if want in ops:
            return want
    if "ADD" in ops:
        return "ADD"
    return ops[0] or "?"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", default="/tmp/grimoire-slots")
    ap.add_argument("--label", default=None)
    ap.add_argument("--out", default=None)
    args = ap.parse_args()
    label = args.label or Path(args.binary).name

    data = json.loads(DATA.read_text())
    entries = [e for e in data
               if e["question_type"] == "knowledge-update"
               and not str(e["question_id"]).endswith("_abs")]
    pairs = []
    for e in entries:
        for prev, nxt in evidence_pairs(e):
            pairs.append({"qid": str(e["question_id"])[:8], "prev": prev, "next": nxt})
    print(f"{len(entries)} knowledge-update questions → {len(pairs)} evidence pairs")

    vault = Path("/tmp/lme-slot-probe")
    shutil.rmtree(vault, ignore_errors=True)
    rows = []
    with launch(args.binary, vault, PORT, embed="auto",
                env={"GRIMOIRE_RATE_LIMIT": "off"}) as base:
        for i, p in enumerate(pairs, 1):
            try:
                op = probe(base, p["prev"], p["next"])
            except Exception as e:  # noqa: BLE001
                op = f"ERROR:{type(e).__name__}"
            rows.append({**p, "op": op})
            if i % 20 == 0:
                print(f"  {i}/{len(pairs)}", flush=True)

    counts = Counter(r["op"] for r in rows)
    n = len(rows)
    print(f"\n## {label}\n")
    print("| outcome | pairs | share |")
    print("|---|---|---|")
    for op in ("UPDATE", "DELETE", "ADD", "NOOP"):
        if counts.get(op):
            print(f"| {op} | {counts[op]} | {100*counts[op]/n:.1f}% |")
    for op, c in counts.items():
        if op not in ("UPDATE", "DELETE", "ADD", "NOOP"):
            print(f"| {op} | {c} | {100*c/n:.1f}% |")
    caught = counts.get("UPDATE", 0) + counts.get("DELETE", 0)
    print(f"\n**recognised as an update: {caught}/{n} ({100*caught/n:.1f}%)**")

    dest = Path(args.out or HERE / "results_memory" / f"slotprobe-{label}.json")
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_text(json.dumps(
        {"label": label, "n": n, "counts": dict(counts), "rows": rows}, indent=1))
    print(f"wrote {dest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
