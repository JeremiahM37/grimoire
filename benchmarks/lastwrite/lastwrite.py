#!/usr/bin/env python3
"""Zero-learning "newest statement wins" floor for knowledge-update recognition.

    python3 lastwrite.py --selftest
    LME_DATA=/path/to/longmemeval_s.json python3 lastwrite.py recognise
    LME_DATA=/path/to/longmemeval_s.json python3 lastwrite.py recognise --ablate-cues
    LME_DATA=/path/to/longmemeval_s.json python3 lastwrite.py answer [--ablate-cues]

No model, no network, no learning. The resolver is a slot table: for each
(subject, attribute) it keeps the latest dated statement and nothing else. Two
questions are asked of it, matching the two numbers Grimoire reports:

* `recognise` - the reader-free measurement from
  benchmarks/longmemeval/slot_probe.py and slot_falsepos.py. Given the earlier
  and the later statement of a LongMemEval knowledge-update fact, does the
  later one supersede the earlier? Same 72 evidence pairs, the same dev/held-out
  split and the same 400-pair unmarked control (seed 20260821) as the Grimoire
  probes.
* `answer` - per knowledge-update question, answer from the latest statement in
  the haystack that fills the question's slot, then score it.

`--ablate-cues` removes update cue words (now, changed, moved, switched,
updated, "no longer", instead) from every statement before the slot table is
built. If a score drops, the resolver was reading the update word, not the
slot. Both arms are reported by running the subcommand twice.

Slot extraction is deliberately simple and is the whole of the method:

* Subject: `my`/`our X is Y` -> subject `user`; `the X is Y` -> subject `the`.
* Attribute: the last two content words before the copula, with determiners
  and time adverbs dropped (so "my current commute time is" -> "commute time").
* Value: the text after the copula, cut at the first comma, semicolon, or
  clause connector (but, because, and I, which, since, so, that), filler words
  dropped, at most eight tokens. Cue words are NOT dropped here, so "is now 45
  minutes" keeps "now" unless the ablation removed it. That is the point.

Coverage (how many pairs get a slot on both sides) is printed next to every
score. A low score with low coverage says the extractor is the bottleneck, not
the policy, and this floor makes no claim beyond that.

Scoring for `answer` is a deterministic token-containment match, NOT the
LongMemEval judge (an LLM, so not deterministic): the answer is correct when
every content token of the gold answer appears in the predicted value. This is
lenient ("not Chicago" would match "Chicago"), which only raises this floor.

Dataset: LongMemEval `longmemeval_s` from
https://huggingface.co/datasets/xiaowu0162/longmemeval, read from `LME_DATA`
(default `/tmp/lme_dl/longmemeval_s.json`, the path slot_probe.py uses). Never
committed. Absent dataset -> the subcommands say so and exit 2; `--selftest`
always runs on synthetic data.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import random
import re
import sys
from pathlib import Path

HERE = Path(__file__).parent
DATA = Path(os.environ.get("LME_DATA", "/tmp/lme_dl/longmemeval_s.json"))
CONTROL_SEED = 20260821          # same seed as slot_falsepos.py
CONTROL_PAIRS = 400              # same size as slot_falsepos.py

CUE_RE = re.compile(
    r"\bno longer\b|\bnow\b|\bchanged\b|\bmoved\b|\bswitched\b|\bupdated\b|\binstead\b",
    re.I)
COPULA = r"(?:is|are|was|were|will be|has been|have been|became)"
POSS = re.compile(r"\b(?:my|our)\s+(?P<attr>[a-z][a-z' -]{0,60}?)\s+" + COPULA
                  + r"\s+(?P<val>[^.!?\n]+)", re.I)
DEFN = re.compile(r"\bthe\s+(?P<attr>[a-z][a-z' -]{0,60}?)\s+" + COPULA
                  + r"\s+(?P<val>[^.!?\n]+)", re.I)
QATTR = re.compile(r"\b(?:my|our)\s+(?P<attr>[a-z][a-z' -]{0,60}?)\s*\?\s*$", re.I)
STOP_ATTR = {"the", "a", "an", "my", "our", "your", "current", "latest", "new",
             "old", "previous", "usual", "actual", "real", "total", "overall",
             "now", "currently", "right", "today", "these", "days", "lately",
             "recently", "just", "still", "also", "really", "actually"}
FILLER_VAL = {"currently", "just", "recently", "also", "still", "really",
              "actually", "finally"}
VAL_CUT = re.compile(r",|;| but | because | and i | which | since | so | that ", re.I)
STOP_TOK = {"a", "an", "the", "of", "and", "to", "in", "on", "at", "is", "was",
            "it", "its", "for"}


# ---- slot extraction --------------------------------------------------------

def strip_cues(text: str) -> str:
    return re.sub(r"\s+", " ", CUE_RE.sub(" ", text)).strip()


def norm_attr(raw: str) -> str:
    toks = [t for t in re.findall(r"[a-z0-9']+", raw.lower()) if t not in STOP_ATTR]
    return " ".join(toks[-2:])


def norm_val(raw: str) -> str:
    head = VAL_CUT.split(raw.lower().strip())[0]
    toks = [t for t in re.findall(r"[a-z0-9.'-]+", head) if t not in FILLER_VAL]
    return " ".join(toks[:8])


def slot(text: str, ablate: bool = False):
    """(subject, attribute, value) for a statement, or None if no slot is found."""
    if ablate:
        text = strip_cues(text)
    subj = "user"
    m = POSS.search(text)
    if not m:
        m = DEFN.search(text)
        subj = "the"
        if not m:
            return None
    attr, val = norm_attr(m.group("attr")), norm_val(m.group("val"))
    if not attr or not val:
        return None
    return subj, attr, val


def question_key(question: str, ablate: bool = False):
    """(subject, attribute) the question asks about, or None."""
    if ablate:
        question = strip_cues(question)
    m = QATTR.search(question.strip())
    if not m:
        return None
    attr = norm_attr(m.group("attr"))
    return ("user", attr) if attr else None


# ---- the resolver -----------------------------------------------------------

def last_write(statements, ablate: bool = False):
    """Slot table keeping only the latest statement per (subject, attribute).

    `statements` is a list of (sort_key, text); the sort key orders them in time.
    Returns a dict (subject, attribute) -> value.
    """
    store = {}
    for _key, text in sorted(statements, key=lambda s: s[0]):
        s = slot(text, ablate)
        if s is not None:
            store[(s[0], s[1])] = s[2]
    return store


def recognise(prev: str, nxt: str, ablate: bool = False) -> str:
    """UPDATE if the later statement fills the same slot with a different value,
    NOOP if the same value, ADD if no shared slot (both stay on file)."""
    a, b = slot(prev, ablate), slot(nxt, ablate)
    if a is None or b is None or a[:2] != b[:2]:
        return "ADD"
    return "UPDATE" if a[2] != b[2] else "NOOP"


# ---- LongMemEval item handling (same logic as slot_probe.py / slot_falsepos.py)

def is_kupdate(e: dict) -> bool:
    return e.get("question_type") == "knowledge-update" and not str(
        e["question_id"]).endswith("_abs")


def parse_date(raw: str, si: int):
    m = re.search(r"(\d{4})/(\d{1,2})/(\d{1,2})(?:\s*\([^)]*\))?\s*(\d{1,2}):(\d{2})", raw or "")
    if not m:
        return (0, 0, 0, 0, 0)
    return tuple(int(x) for x in m.groups())


def evidence_pairs(entry: dict):
    """Consecutive user turns marked has_answer, in date order (slot_probe.py)."""
    ids = entry.get("haystack_session_ids") or []
    dates = entry.get("haystack_dates") or []
    marked = []
    for sid in entry.get("answer_session_ids") or []:
        if sid not in ids:
            continue
        i = ids.index(sid)
        for t in entry["haystack_sessions"][i]:
            if t.get("has_answer") and (t.get("role") or "") == "user":
                marked.append((dates[i] if i < len(dates) else "", t["content"].strip()))
    marked.sort(key=lambda p: p[0])
    if len(marked) < 2:
        return []
    return [(marked[i][1], marked[i + 1][1]) for i in range(len(marked) - 1)]


def control_pairs(data: list, n: int = CONTROL_PAIRS):
    """Unmarked same-haystack user-turn pairs (slot_falsepos.py, same RNG order)."""
    rng = random.Random(CONTROL_SEED)
    pairs = []
    for e in (x for x in data if is_kupdate(x)):
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
        turns = [t for t in turns if t not in marked and len(t) > 40]
        rng.shuffle(turns)
        for i in range(0, min(len(turns) - 1, 12), 2):
            pairs.append({"qid": str(e["question_id"])[:8],
                          "prev": turns[i], "next": turns[i + 1]})
    rng.shuffle(pairs)
    return pairs[:n]


def held_out_bucket(qid: str) -> int:
    """sha256(qid) % 2. PROTOCOL-slots.md does not name which bucket is held-out;
    this harness treats bucket 1 as held-out and prints both bucket sizes so the
    35/37 split can be checked against the report."""
    return int(hashlib.sha256(qid.encode()).hexdigest(), 16) % 2


# ---- scoring ----------------------------------------------------------------

def contains_match(gold: str, pred: str) -> bool:
    g = {t for t in re.findall(r"[a-z0-9]+", gold.lower()) if t not in STOP_TOK}
    p = {t for t in re.findall(r"[a-z0-9]+", pred.lower()) if t not in STOP_TOK}
    return bool(g) and g <= p


def wilson(k: int, n: int, z: float = 1.96):
    if n == 0:
        return 0.0, 0.0
    p = k / n
    den = 1 + z * z / n
    c = (p + z * z / (2 * n)) / den
    h = z * ((p * (1 - p) / n + z * z / (4 * n * n)) ** 0.5) / den
    return max(0.0, c - h), min(1.0, c + h)


def pct(k, n):
    return f"{k}/{n} ({100 * k / n:.1f}%)" if n else "0/0"


# ---- subcommands ------------------------------------------------------------

def load_data() -> list | None:
    if not DATA.exists():
        print(f"dataset not found at {DATA}; set LME_DATA (see README). "
              f"Use --selftest for the synthetic check.", file=sys.stderr)
        return None
    return json.loads(DATA.read_text())


def run_recognise(data: list, ablate: bool) -> dict:
    entries = [e for e in data if is_kupdate(e)]
    rows = []
    for e in entries:
        for prev, nxt in evidence_pairs(e):
            rows.append({"qid": str(e["question_id"]), "set": "marked",
                         "op": recognise(prev, nxt, ablate),
                         "both_slots": slot(prev, ablate) is not None and slot(nxt, ablate) is not None})
    controls = control_pairs(data)
    for p in controls:
        rows.append({"qid": p["qid"], "set": "control",
                     "op": recognise(p["prev"], p["next"], ablate),
                     "both_slots": slot(p["prev"], ablate) is not None and slot(p["next"], ablate) is not None})
    marked = [r for r in rows if r["set"] == "marked"]
    held = [r for r in marked if held_out_bucket(r["qid"]) == 1]
    dev = [r for r in marked if held_out_bucket(r["qid"]) == 0]
    fp = [r for r in rows if r["set"] == "control" and r["op"] == "UPDATE"]
    return {
        "ablate_cues": ablate,
        "marked_pairs": len(marked),
        "dev_bucket0": len(dev), "heldout_bucket1": len(held),
        "recognised_all": sum(r["op"] == "UPDATE" for r in marked),
        "recognised_heldout": sum(r["op"] == "UPDATE" for r in held),
        "recognised_dev": sum(r["op"] == "UPDATE" for r in dev),
        "control_pairs": len(controls),
        "false_positives": len(fp),
        "coverage_marked": sum(r["both_slots"] for r in marked),
        "coverage_control": sum(r["both_slots"] for r in rows if r["set"] == "control"),
        "rows": rows,
    }


def run_answer(data: list, ablate: bool) -> dict:
    rows = []
    for e in (x for x in data if is_kupdate(x)):
        key = question_key(e["question"], ablate)
        stmts = []
        dates = e.get("haystack_dates") or []
        for si, sess in enumerate(e["haystack_sessions"]):
            d = parse_date(dates[si] if si < len(dates) else "", si)
            for ti, t in enumerate(sess):
                if (t.get("role") or "") == "user":
                    stmts.append(((d, si, ti), t["content"]))
        table = last_write(stmts, ablate)
        pred = table.get(key) if key else None
        gold = str(e.get("answer", ""))
        rows.append({"qid": str(e["question_id"]), "gold": gold, "pred": pred,
                     "answered": pred is not None,
                     "correct": pred is not None and contains_match(gold, pred)})
    return {"ablate_cues": ablate, "n": len(rows), "answered": sum(r["answered"] for r in rows),
            "correct": sum(r["correct"] for r in rows), "rows": rows}


def print_recognise(res: dict, label: str):
    n = res["marked_pairs"]
    print(f"\n## last-write recognition ({label})\n")
    print(f"marked evidence pairs: {n}   (dev bucket0 {res['dev_bucket0']}, held-out bucket1 {res['heldout_bucket1']})")
    print(f"recognised as UPDATE, all:       {pct(res['recognised_all'], n)}   [Grimoire slot probe: 28/72 in the 'both' row, 26/72 in the best row]")
    print(f"recognised as UPDATE, held-out:  {pct(res['recognised_heldout'], res['heldout_bucket1'])}   [Grimoire: 17/37 best row]")
    print(f"false positives, {res['control_pairs']} unmarked pairs: {pct(res['false_positives'], res['control_pairs'])}   [Grimoire: 3/400 best row, 5/400 both]")
    print(f"slot coverage: marked {pct(res['coverage_marked'], 2 * n)} of pair sides, control {pct(res['coverage_control'], 2 * res['control_pairs'])} of pair sides")


def print_answer(res: dict, label: str):
    n = res["n"]
    lo, hi = wilson(res["correct"], n)
    print(f"\n## last-write answer ({label})\n")
    print(f"questions: {n}   answered (slot found): {pct(res['answered'], n)}")
    print(f"correct (abstain counts wrong): {pct(res['correct'], n)}   95% Wilson CI {100*lo:.1f}-{100*hi:.1f}%")
    if res["answered"]:
        print(f"precision among answered: {pct(res['correct'], res['answered'])}")


# ---- selftest ---------------------------------------------------------------

def synthetic_entries():
    def turn(text, has=False):
        return {"role": "user", "content": text, "has_answer": has}

    def asst(text):
        return {"role": "assistant", "content": text, "has_answer": False}

    return [
        {   # commute: old value in an earlier session, listed LATER in the array
            "question_id": "syn-commute", "question_type": "knowledge-update",
            "question": "What is my current commute time?", "answer": "45 minutes",
            "haystack_session_ids": ["s2", "s1", "s3"],
            "haystack_dates": ["2023/03/01 (Wed) 09:00", "2023/01/05 (Thu) 10:00", "2023/02/01 (Wed) 08:00"],
            "answer_session_ids": ["s1", "s2"],
            "haystack_sessions": [
                [turn("I moved to the new flat. My commute time is now 45 minutes.", True), asst("noted")],
                [turn("My commute time is 30 minutes on the bus.", True)],
                [turn("I went to the gym today.")],
            ],
        },
        {   # gym: latest statement is in the session that is FIRST in the array
            # (2023/05/10 is listed first), so only the date can pick the winner
            "question_id": "syn-gym", "question_type": "knowledge-update",
            "question": "What is my gym schedule?", "answer": "4",
            "haystack_session_ids": ["a", "b"],
            "haystack_dates": ["2023/05/10 (Wed) 11:00", "2023/04/02 (Sun) 11:00"],
            "answer_session_ids": ["a", "b"],
            "haystack_sessions": [
                [turn("My gym schedule is 4 days a week now.", True)],
                [turn("My gym schedule is 2 days a week.", True)],
            ],
        },
        {   # no slot for the question: must abstain, not guess. Two long unmarked
            # user turns give the control sampler something to draw from.
            "question_id": "syn-none-abs", "question_type": "knowledge-update",
            "question": "Where did I park?", "answer": "level 3",
            "haystack_session_ids": ["c"], "haystack_dates": ["2023/06/01 (Thu) 08:00"],
            "answer_session_ids": [],
            "haystack_sessions": [[
                turn("Lovely weather today, I walked along the canal for an hour after lunch."),
                turn("The canal path was flooded near the old mill bridge, so I turned back early."),
            ]],
        },
        {   # abstention item is excluded by is_kupdate
            "question_id": "syn-x_abs", "question_type": "knowledge-update",
            "question": "My budget?", "answer": "n/a", "haystack_session_ids": [],
            "haystack_dates": [], "answer_session_ids": [], "haystack_sessions": [],
        },
    ]


def selftest() -> int:
    fails = []

    def check(name, cond):
        print(("PASS " if cond else "FAIL ") + name)
        if not cond:
            fails.append(name)

    # slot extraction
    check("possessive slot", slot("My commute is 30 minutes.") == ("user", "commute", "30 minutes"))
    check("current/adverb dropped from attribute",
          slot("My current commute time is 30 minutes.")[1] == "commute time")
    check("clause cut at comma", slot("My rent is 900 dollars, paid monthly.")[2] == "900 dollars")
    check("no slot -> None", slot("Lovely weather today.") is None)
    check("question key", question_key("What is my current commute time?") == ("user", "commute time"))
    check("question without 'my' -> None", question_key("Where is the station?") is None)

    # ablation: cue word stays in the value unless stripped
    check("cue kept without ablation", slot("My commute is now 45 minutes.")[2] == "now 45 minutes")
    check("cue removed by ablation", slot("My commute is now 45 minutes.", True)[2] == "45 minutes")

    # pair recognition
    check("different value -> UPDATE",
          recognise("My commute is 30 minutes.", "My commute is now 45 minutes.") == "UPDATE")
    check("same slot, same value -> NOOP (ablated)",
          recognise("My commute is 45 minutes.", "My commute is now 45 minutes.", True) == "NOOP")
    check("same text difference only by cue word -> UPDATE without ablation (cue dependence)",
          recognise("My commute is 45 minutes.", "My commute is now 45 minutes.") == "UPDATE")
    check("different slot -> ADD",
          recognise("My rent is 900 dollars.", "My commute is 45 minutes.") == "ADD")
    check("unparseable -> ADD", recognise("Nice day.", "My commute is 45 minutes.") == "ADD")

    # last write uses date, not array order
    entries = synthetic_entries()
    check("abstention id excluded", len([e for e in entries if is_kupdate(e)]) == 3)
    stmts = []
    e = entries[0]
    for si, sess in enumerate(e["haystack_sessions"]):
        d = parse_date(e["haystack_dates"][si], si)
        for ti, t in enumerate(sess):
            if t["role"] == "user":
                stmts.append(((d, si, ti), t["content"]))
    table = last_write(stmts, ablate=True)
    check("latest-by-date wins over array order (ablated)",
          table.get(("user", "commute time")) == "45 minutes")
    check("without ablation the cue stays in the stored value",
          last_write(stmts).get(("user", "commute time")) == "now 45 minutes")

    res = run_answer(entries, ablate=False)
    by = {r["qid"]: r for r in res["rows"]}
    check("answer: commute correct (cue kept, gold tokens still contained)", by["syn-commute"]["correct"])
    check("answer: gym picks the latest by date", by["syn-gym"]["pred"] == "4 days a week now")
    check("answer: gym matches gold '4' (contains-match is lenient on extra tokens)",
          by["syn-gym"]["correct"] is True)
    check("answer: no slot abstains, scored wrong",
          by["syn-none-abs"]["answered"] is False and by["syn-none-abs"]["correct"] is False)
    check("contains_match: all gold tokens required", contains_match("45 minutes", "around 30 minutes") is False)

    # the cue ablation can change an answer
    ab = run_answer(entries, ablate=True)
    check("ablation runs and answers the same items", ab["n"] == res["n"])
    check("ablation changes the commute value", {r["qid"]: r["pred"] for r in ab["rows"]}["syn-commute"] == "45 minutes")

    # control pairs are deterministic
    c1 = control_pairs(entries)
    c2 = control_pairs(entries)
    check("control sample deterministic", c1 == c2 and len(c1) == 1)

    # recognition over synthetic data: entries carry one marked pair each
    rec = run_recognise(entries, ablate=False)
    check("marked pairs counted", rec["marked_pairs"] == 2)
    check("held-out + dev = marked",
          rec["dev_bucket0"] + rec["heldout_bucket1"] == rec["marked_pairs"])
    check("false positives bounded by control size", rec["false_positives"] <= rec["control_pairs"])
    check("wilson interval sane", wilson(0, 10)[0] == 0.0 and wilson(10, 10)[1] == 1.0)

    if fails:
        print(f"\n{len(fails)} FAILED")
        return 1
    print("\nall selftest checks passed")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--selftest", action="store_true")
    sub = ap.add_subparsers(dest="cmd")
    for name in ("recognise", "answer"):
        p = sub.add_parser(name)
        p.add_argument("--ablate-cues", action="store_true")
        p.add_argument("--out", default=None)
    args = ap.parse_args()
    if args.selftest:
        return selftest()
    if not args.cmd:
        ap.print_help()
        return 2
    data = load_data()
    if data is None:
        return 2
    label = "ablated" if args.ablate_cues else "as written"
    if args.cmd == "recognise":
        res = run_recognise(data, args.ablate_cues)
        print_recognise(res, label)
    else:
        res = run_answer(data, args.ablate_cues)
        print_answer(res, label)
    out = Path(args.out) if args.out else HERE / "results" / f"{args.cmd}{'-ablated' if args.ablate_cues else ''}.json"
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(res, indent=1))
    print(f"\nwrote {out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
