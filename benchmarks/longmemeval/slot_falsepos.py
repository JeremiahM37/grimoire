#!/usr/bin/env python3
"""How often does the value-slot rule supersede something it should not?

    python3 slot_falsepos.py --binary /tmp/grimoire-slots2

`slot_probe.py` measures RECALL — of the updates LongMemEval marks, how many
does the engine catch. On its own that number is worthless: a rule that
superseded every pair would score 100%.

This is the other half. It draws pairs of user turns from the SAME haystack
that the dataset does NOT mark as evidence for each other, and counts how often
the engine calls one an update of the other. Those pairs are overwhelmingly
unrelated — two people's chat turns months apart about different topics — so
every UPDATE here is a fact destroyed.

It is not a perfect control: a haystack is one user's life, and two unmarked
turns CAN genuinely update each other (the dataset only marks what its question
needs). So this is an upper bound on the false-positive rate, and the honest
reading is "at most this bad".

The asymmetry that matters: a missed update leaves two facts on file, which a
reader can still resolve and a person can still see. A false update strikes a
true fact through. So the bar for this number is much lower than the bar for
recall is high.
"""
from __future__ import annotations

import argparse
import itertools
import json
import random
import shutil
import sys
import urllib.request
from collections import Counter
from pathlib import Path

HERE = Path(__file__).parent
sys.path.insert(0, str(HERE.parent))
from goserver import launch  # noqa: E402

DATA = Path("/tmp/lme_dl/longmemeval_s.json")
PORT = 9167

_topic_n = itertools.count()


def next_topic() -> int:
    return next(_topic_n)
SEED = 20260821


def post(base, path, body):
    req = urllib.request.Request(
        base + path, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.loads(r.read().decode() or "{}")


def probe(base, prev, nxt):
    """Returns (op, why). `why` names the rule that fired, which is the whole
    diagnostic: a false positive from the value path and one from the
    similarity path call for different fixes."""
    # See slot_probe.py: vault-wide reconciliation would compare each pair
    # against every earlier one, and the number would be contamination.
    topic = f"n{next_topic()}"
    post(base, "/api/memory", {"topic": topic, "agent": "probe", "scope": "topic",
                               "text": prev[:19000]})
    out = post(base, "/api/memory", {"topic": topic, "agent": "probe", "scope": "topic",
                                     "text": nxt[:19000]})
    results = out.get("results") or [out]
    for r in results:
        if r.get("op") in ("UPDATE", "DELETE"):
            return r["op"], r.get("why", "")
    for r in results:
        if r.get("op") == "ADD":
            return "ADD", r.get("why", "")
    return (results[0].get("op") or "?"), results[0].get("why", "")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", default="/tmp/grimoire-slots2")
    ap.add_argument("--label", default=None)
    ap.add_argument("--pairs", type=int, default=400)
    args = ap.parse_args()
    label = args.label or Path(args.binary).name

    rng = random.Random(SEED)
    data = json.loads(DATA.read_text())
    entries = [e for e in data
               if e["question_type"] == "knowledge-update"
               and not str(e["question_id"]).endswith("_abs")]

    pairs = []
    for e in entries:
        marked = set()
        ids = e.get("haystack_session_ids") or []
        for sid in e.get("answer_session_ids") or []:
            if sid in ids:
                for t in e["haystack_sessions"][ids.index(sid)]:
                    if t.get("has_answer"):
                        marked.add((t.get("content") or "").strip())
        turns = [(t.get("content") or "").strip()
                 for sess in e["haystack_sessions"] for t in sess
                 if (t.get("role") or "") == "user" and (t.get("content") or "").strip()]
        # Unmarked turns only: the marked ones are the true updates and are
        # scored by slot_probe.py, not here.
        turns = [t for t in turns if t not in marked and len(t) > 40]
        rng.shuffle(turns)
        for i in range(0, min(len(turns) - 1, 12), 2):
            pairs.append({"qid": str(e["question_id"])[:8],
                          "prev": turns[i], "next": turns[i + 1]})
    rng.shuffle(pairs)
    pairs = pairs[:args.pairs]
    print(f"{len(pairs)} unmarked same-haystack pairs")

    vault = Path("/tmp/lme-fp-probe")
    shutil.rmtree(vault, ignore_errors=True)
    rows = []
    with launch(args.binary, vault, PORT, embed="auto",
                env={"GRIMOIRE_RATE_LIMIT": "off"}) as base:
        for i, p in enumerate(pairs, 1):
            try:
                op, why = probe(base, p["prev"], p["next"])
            except Exception as e:  # noqa: BLE001
                op, why = f"ERROR:{type(e).__name__}", ""
            rows.append({**p, "op": op, "why": why})
            if i % 100 == 0:
                print(f"  {i}/{len(pairs)}", flush=True)

    counts = Counter(r["op"] for r in rows)
    n = len(rows)
    fired = counts.get("UPDATE", 0) + counts.get("DELETE", 0)
    print(f"\n## {label}\n")
    for op, c in counts.most_common():
        print(f"  {op:8} {c:4}  {100*c/n:5.1f}%")
    print(f"\n**superseded an unrelated pair: {fired}/{n} ({100*fired/n:.1f}%)**")
    if fired:
        by_rule = Counter("value" if "value" in (r.get("why") or "") else "other"
                          for r in rows if r["op"] in ("UPDATE", "DELETE"))
        print(f"  by rule: {dict(by_rule)}")
        print("\nevery one of them:")
        for r in rows:
            if r["op"] in ("UPDATE", "DELETE"):
                print(f"  [{r['qid']}] {r.get('why','')[:90]}\n"
                      f"    prev: {r['prev'][:150]}\n    next: {r['next'][:150]}")

    dest = HERE / "results_memory" / f"falsepos-{label}.json"
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_text(json.dumps(
        {"label": label, "n": n, "counts": dict(counts), "rows": rows}, indent=1))
    print(f"\nwrote {dest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
