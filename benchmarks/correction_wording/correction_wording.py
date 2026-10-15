#!/usr/bin/env python3
"""How should a corrected fact be shown to a model? Four wordings, one question.

    python3 correction_wording.py --selftest
    OPENAI_COMPAT_BASE_URL=http://127.0.0.1:11434/v1 OPENAI_COMPAT_MODEL=<name> \
        python3 correction_wording.py run [--n 30] [--max-calls 120] [--out DIR]

Each item is an invented slot fact ("the courier depot for Ward 7 is Velmip-42")
that is corrected later in the same store ("... now Brisko-19"). The invented
values exist only in this file, so no model can know them and the answer has
to come from the context. The four arms differ only in how the corrected slot
line is worded:

    Y               "<slot>: Y"                          new value only
    now Y (changed) "<slot>: now Y (changed DATE)"
    was X, now Y    "<slot>: was X, now Y"
    X no longer     "<slot>: X is no longer true; Y"     negated old value

The question asks for the current value. Scoring per reply:

    contains Y      the new value appears (the wanted answer)
    contains X      the old value appears (proactive interference, or a
                    restated old value being used)
    strict correct  contains Y and does not contain X

Why it is worth measuring: earlier values in context intrude on recall of new
ones (proactive interference, arXiv 2506.08184 and 2603.00270), and restating
the old value may raise its use. The arm that restates X is the one that makes
that visible. The model is only a probe here: nothing is learned or stored.

Backend: any OpenAI-compatible server, read from the environment. There is NO
default model. A `run` needs OPENAI_COMPAT_BASE_URL and OPENAI_COMPAT_MODEL.
Hosts whose name contains "anthropic" or "claude" are refused, so a hosted
Claude model cannot be reached by accident. `--max-calls` caps a run. `--selftest`
uses a fake model and makes no network call.
"""
from __future__ import annotations

import argparse
import json
import math
import os
import random
import re
import tempfile
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).parent
ARMS = ["Y", "now Y (changed DATE)", "was X, now Y", "X no longer true; Y"]

SLOTS = [
    "office coffee machine", "courier depot for Ward 7", "backup router firmware",
    "team standup room", "lab freezer model", "bike storage code", "guest wifi network",
    "print server host", "spare key holder", "visitor parking level", "kettle model",
    "archive box vendor", "weekly review template", "pantry inventory app",
    "recycling pickup day", "reading room booking system", "loading dock scheduler",
    "staff badge printer", "meeting notes tool", "shared drive sync client",
    "cafeteria menu service", "night shift contact", "greenhouse sensor model",
    "fleet tablet case", "map tile server", "training room projector", "tea supplier",
    "locker code format", "stairwell camera vendor", "wiki search engine", "ferry timetable feed",
]
SYL = ["vel", "tar", "kor", "mip", "sen", "dro", "qua", "lix", "bra", "fen", "zor",
       "hal", "ume", "pix", "tov", "gan", "rib", "sol", "wen", "yar"]
DISTRACTORS = [
    "Team lunch moved to Thursday this week.",
    "The printer on floor two needs toner before Friday.",
    "Quarterly survey closes on the last working day of the month.",
    "Visitors must sign in at the front desk before 10am.",
    "The fire drill is scheduled for the first Monday of next month.",
]


def invented(rng: random.Random) -> str:
    return "".join(rng.choice(SYL) for _ in range(3)).capitalize() + f"-{rng.randint(10, 99)}"


def make_items(n: int, seed: int) -> list[dict]:
    """n items with distinct slots and invented, non-overlapping X and Y values.

    Dates are invented 2026 dates, so no item can be answered from the model's
    training data even if a value were guessable.
    """
    rng = random.Random(seed)
    items = []
    for i in range(n):
        slot = SLOTS[i % len(SLOTS)] if i < len(SLOTS) else f"{SLOTS[i % len(SLOTS)]} unit {i}"
        while True:
            x, y = invented(rng), invented(rng)
            if x != y and x not in y and y not in x:
                break
        m1, d1 = rng.randint(1, 8), rng.randint(1, 28)
        m2, d2 = rng.randint(9, 10), rng.randint(1, 28)
        items.append({"id": i, "slot": slot, "x": x, "y": y,
                      "date_old": f"2026-{m1:02d}-{d1:02d}",
                      "date_new": f"2026-{m2:02d}-{d2:02d}"})
    return items


def render(arm: str, it: dict) -> str:
    s, x, y = it["slot"], it["x"], it["y"]
    if arm == "Y":
        return f"{s}: {y}"
    if arm == "now Y (changed DATE)":
        return f"{s}: now {y} (changed {it['date_new']})"
    if arm == "was X, now Y":
        return f"{s}: was {x}, now {y}"
    if arm == "X no longer true; Y":
        return f"{s}: {x} is no longer true; {y}"
    raise ValueError(arm)


def build_prompt(arm: str, idx: int, items: list[dict], order: list[int]) -> str:
    """One prompt per (arm, item). Distractors and line order are the same for
    every arm, so the only thing that differs is the wording of the slot lines."""
    target = items[idx]
    lines = []
    for j, k in enumerate(order):
        lines.append(render(arm, items[k]))
        if j % 6 == 5:
            lines.append(DISTRACTORS[(j // 6) % len(DISTRACTORS)])
    context = "\n".join(lines)
    return ("Facts recorded in the team's notes, one per line:\n"
            f"{context}\n\n"
            f"Question: What is the current value for the {target['slot']}? "
            "Answer with the value only.")


def wilson(k: int, n: int, z: float = 1.96):
    if n == 0:
        return 0.0, 0.0
    p = k / n
    den = 1 + z * z / n
    c = (p + z * z / (2 * n)) / den
    h = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / den
    return max(0.0, c - h), min(1.0, c + h)


def score(reply: str, it: dict) -> dict:
    r = reply.lower()
    has_y = it["y"].lower() in r
    has_x = it["x"].lower() in r
    return {"has_y": has_y, "has_x": has_x, "strict": has_y and not has_x}


# ---- backend ----------------------------------------------------------------

def guard_host(base: str) -> None:
    host = (urllib.parse.urlparse(base).hostname or "").lower()
    if "anthropic" in host or "claude" in host:
        raise SystemExit("refusing: this harness does not call hosted Claude endpoints")


def chat(messages: list[dict]) -> str:
    base = os.environ.get("OPENAI_COMPAT_BASE_URL", "")
    model = os.environ.get("OPENAI_COMPAT_MODEL", "")
    if not base or not model:
        raise SystemExit("set OPENAI_COMPAT_BASE_URL and OPENAI_COMPAT_MODEL "
                         "(no default; a local OpenAI-compatible server only)")
    guard_host(base)
    headers = {"Content-Type": "application/json"}
    key = os.environ.get("OPENAI_COMPAT_API_KEY")
    if key:
        headers["Authorization"] = f"Bearer {key}"
    body = json.dumps({"model": model, "messages": messages,
                       "temperature": 0, "max_tokens": 128}).encode()
    req = urllib.request.Request(base.rstrip("/") + "/chat/completions", data=body,
                                 headers=headers, method="POST")
    with urllib.request.urlopen(req, timeout=180) as r:
        out = json.loads(r.read().decode())
    return out["choices"][0]["message"].get("content") or ""


# ---- run --------------------------------------------------------------------

def run(model, n: int, seed: int, max_calls: int, out_dir: Path) -> dict:
    total = n * len(ARMS)
    if total > max_calls:
        raise SystemExit(f"{total} calls needed, --max-calls is {max_calls}; lower --n or raise the cap")
    items = make_items(n, seed)
    order = list(range(n))
    random.Random(seed + 1).shuffle(order)
    per_arm = {}
    rows = []
    for arm in ARMS:
        results = []
        for idx, it in enumerate(items):
            prompt = build_prompt(arm, idx, items, order)
            reply = model([{"role": "user", "content": prompt}])
            s = score(reply, it)
            results.append(s)
            rows.append({"arm": arm, "item": it["id"], "reply": reply.strip()[:300], **s})
        k_y = sum(r["has_y"] for r in results)
        k_x = sum(r["has_x"] for r in results)
        k_s = sum(r["strict"] for r in results)
        per_arm[arm] = {"n": n, "has_y": k_y, "has_x": k_x, "strict": k_s,
                        "neither": sum(not r["has_y"] and not r["has_x"] for r in results)}
    out = {"seed": seed, "n": n, "model": os.environ.get("OPENAI_COMPAT_MODEL", "fake"),
           "arms": per_arm, "rows": rows}
    out_dir.mkdir(parents=True, exist_ok=True)
    dest = out_dir / f"correction_wording-seed{seed}-n{n}.json"
    dest.write_text(json.dumps(out, indent=1))
    print_table(out)
    print(f"wrote {dest}")
    return out


def print_table(out: dict) -> None:
    n = out["n"]
    print(f"\nmodel: {out['model']}   items per arm: {n}   (temperature 0, one sample per item)\n")
    print("| arm | contains Y (95% CI) | contains X (95% CI) | strict correct | neither |")
    print("|---|---|---|---|---|")
    for arm, a in out["arms"].items():
        ly, hy = wilson(a["has_y"], n)
        lx, hx = wilson(a["has_x"], n)
        print(f"| {arm} | {a['has_y']}/{n} ({100*ly:.0f}-{100*hy:.0f}%) | "
              f"{a['has_x']}/{n} ({100*lx:.0f}-{100*hx:.0f}%) | "
              f"{a['strict']}/{n} | {a['neither']}/{n} |")


# ---- selftest ---------------------------------------------------------------

def fake_model(messages: list[dict]) -> str:
    """Deterministic stand-in: returns the FIRST invented value on the target's line.
    That is the interference failure mode in miniature, so the arms must differ."""
    prompt = messages[-1]["content"]
    slot = re.search(r"current value for the (.+?)\?", prompt).group(1)
    for line in prompt.split("\n"):
        if line.startswith(slot + ":"):
            m = re.search(r"[A-Z][a-z]+-\d\d", line)
            return m.group(0) if m else "unknown"
    return "unknown"


def selftest() -> int:
    fails = []

    def check(name, cond):
        print(("PASS " if cond else "FAIL ") + name)
        if not cond:
            fails.append(name)

    items = make_items(30, seed=7)
    check("30 items with distinct slots",
          len({it["slot"] for it in items}) == 30)
    check("X and Y never overlap", all(
        it["x"] != it["y"] and it["x"] not in it["y"] and it["y"] not in it["x"] for it in items))
    check("values are invented, not dictionary words", all(
        re.fullmatch(r"[A-Z][a-z]{6,}-\d\d", it["x"]) and re.fullmatch(r"[A-Z][a-z]{6,}-\d\d", it["y"])
        for it in items))
    check("make_items deterministic", make_items(30, seed=7) == items)

    it = items[0]
    check("render Y arm shows only Y",
          render("Y", it) == f"{it['slot']}: {it['y']}" and it["x"] not in render("Y", it))
    check("render now-changed arm carries the change date",
          f"changed {it['date_new']}" in render("now Y (changed DATE)", it))
    check("render was-now arm shows X and Y",
          render("was X, now Y", it).endswith(f"was {it['x']}, now {it['y']}"))
    check("render negated arm states X is no longer true",
          f"{it['x']} is no longer true; {it['y']}" in render("X no longer true; Y", it))

    order = list(range(30))
    random.Random(8).shuffle(order)
    p_y = build_prompt("Y", 0, items, order)
    p_x = build_prompt("was X, now Y", 0, items, order)
    check("prompts differ only in slot wording across arms",
          p_y.count("\n") == p_x.count("\n") and "Team lunch" in p_y)
    check("question names the target slot", f"for the {it['slot']}?" in p_y)

    check("score: Y only is strict",
          score(f"Answer: {it['y']}", it) == {"has_y": True, "has_x": False, "strict": True})
    check("score: X intrusion is not strict",
          score(f"It was {it['x']}, now {it['y']}", it)["strict"] is False)
    check("score: case-insensitive", score(it["y"].upper(), it)["has_y"] is True)

    lo, hi = wilson(0, 30)
    check("wilson at zero starts at 0", lo == 0.0 and hi > 0)

    # full run through the fake model, all four arms
    tmp = Path(tempfile.mkdtemp(prefix="cw-selftest-"))
    out = run(fake_model, 30, seed=7, max_calls=10_000, out_dir=tmp)
    arms = out["arms"]
    check("Y arm: fake answers the only value, all strict", arms["Y"]["strict"] == 30)
    check("now-changed arm: fake answers Y, all strict", arms["now Y (changed DATE)"]["strict"] == 30)
    check("was-now arm: fake picks X first, all intrusions", arms["was X, now Y"]["has_x"] == 30
          and arms["was X, now Y"]["strict"] == 0)
    check("negated arm: fake picks X first, all intrusions", arms["X no longer true; Y"]["strict"] == 0)

    # the call cap refuses an oversized run before any call is made
    try:
        run(fake_model, 40, seed=7, max_calls=100, out_dir=tmp)
        check("max-calls cap enforced", False)
    except SystemExit:
        check("max-calls cap enforced", True)

    # the backend guard refuses Claude hosts and needs explicit config
    try:
        guard_host("https://api.anthropic.com/v1")
        check("claude host refused", False)
    except SystemExit:
        check("claude host refused", True)
    check("local host allowed", guard_host("http://127.0.0.1:11434/v1") is None)

    if fails:
        print(f"\n{len(fails)} FAILED")
        return 1
    print("\nall selftest checks passed")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("mode", nargs="?", choices=["run"])
    ap.add_argument("--selftest", action="store_true")
    ap.add_argument("--n", type=int, default=30)
    ap.add_argument("--seed", type=int, default=20261014)
    ap.add_argument("--max-calls", type=int, default=120)
    ap.add_argument("--out", default=str(HERE / "results"))
    args = ap.parse_args()
    if args.selftest:
        return selftest()
    if args.mode != "run":
        ap.print_help()
        return 2
    run(chat, args.n, args.seed, args.max_calls, Path(args.out))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
