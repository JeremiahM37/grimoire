#!/usr/bin/env python3
"""Pre-registered analysis of the memory holdout (see PREREG.md in this directory).

Reads one or more copies of the adherence database (`.grimoire/adherence.db`, or
snapshots of it) and nothing else. The database holds hashes, ids and outcome
codes, never memory text. Standard library only, no model calls.

    analyze.py DB [DB...] --counts-only      arm sizes and validity checks, no outcomes
    analyze.py DB [DB...] --power            what the pre-period can detect (no arms needed)
    analyze.py DB [DB...] --start T --end T  the analysis, run once after the end date
    analyze.py --selftest
"""

from __future__ import annotations

import argparse
import bisect
import collections
import json
import math
import random
import sqlite3
import statistics
import sys

WINDOW = 5  # adherence.MaxRecordWindow
KINDS = ("fact", "rule", "procedure", "reference")


def connect(path: str) -> sqlite3.Connection:
    if path == ":memory:":
        return sqlite3.connect(path)
    return sqlite3.connect(f"file:{path}?mode=ro", uri=True)


def load(dbs: list[sqlite3.Connection], start: int, end: int, pre: bool = False):
    """Returns (decisions, dropped). A decision is a dict with session, arm
    ("shown"/"withheld"), p, kind, stage, relevance, y, and the outcome parts.
    `pre` treats every injection as a shown decision (for --power)."""
    acts: dict[str, dict[tuple, tuple]] = collections.defaultdict(dict)
    shown: dict[tuple, dict] = {}
    held: dict[tuple, dict] = {}
    for db in dbs:
        # badSQL and knownSQL in go/internal/adherence/trace_report.go
        for r in db.execute(
            "SELECT session, ts, seq, failed, tests_fail, reedit, revert, thrash, denied, correction"
            " FROM trace_actions"
        ):
            s, ts, seq, failed, tfail, reedit, revert, thrash, denied, corr = r
            parts = (
                int(failed == 1),
                int((tfail or 0) > 0),
                int(reedit == 1 or revert == 1),
                int((thrash or 0) >= 3),
                int(denied == 1),
                int(corr == 1),
            )
            known = (failed is not None and failed >= 0) or (tfail is not None and tfail >= 0) or any(parts)
            if known:
                acts[s][(ts, seq)] = parts
        where = "1=1" if pre else "p_withhold>0"
        for s, key, ts, p, kind, stage, rel in db.execute(
            f"SELECT session, key, ts, p_withhold, kind, stage, relevance FROM injections WHERE {where}"
        ):
            shown[(s, key, ts)] = {"session": s, "arm": "shown", "p": p, "kind": kind or "", "stage": stage or "", "rel": rel or 0.0, "ts": ts}
        if not pre:
            for s, key, ts, p, kind, stage, rel in db.execute(
                "SELECT session, key, ts, p_withhold, kind, stage, relevance FROM trace_withheld"
            ):
                held[(s, key, ts)] = {"session": s, "arm": "withheld", "p": p, "kind": kind, "stage": stage, "rel": rel, "ts": ts}
    ordered = {s: sorted(a.items()) for s, a in acts.items()}
    stamps = {s: [k[0] for k, _ in a] for s, a in ordered.items()}
    out, dropped = [], collections.Counter()
    for d in list(shown.values()) + list(held.values()):
        if not (start <= d["ts"] < end):
            continue
        a = ordered.get(d["session"], [])
        i = bisect.bisect_left(stamps.get(d["session"], []), d["ts"])
        win = [parts for _, parts in a[i : i + WINDOW]]
        if not win:
            dropped[d["arm"]] += 1
            continue
        d["y"] = sum(1 for p in win if any(p)) / len(win)
        d["parts"] = [sum(p[j] for p in win) / len(win) for j in range(6)]
        out.append(d)
    return out, dropped


def diff(ds: list[dict], held: list[bool], val=lambda d: d["y"]) -> float:
    """mean(withheld) - mean(shown): positive means showing the memory helped."""
    hs = hn = ss = sn = 0.0
    for d, h in zip(ds, held, strict=True):
        if h:
            hs += val(d)
            hn += 1
        else:
            ss += val(d)
            sn += 1
    if not hn or not sn:
        return float("nan")
    return hs / hn - ss / sn


def permutation_p(ds: list[dict], perms: int, rng: random.Random, val=lambda d: d["y"]) -> tuple[float, float]:
    """Randomisation test of the sharp null. Each decision's arm is redrawn with
    its own logged probability, which is exactly how the server assigned it."""
    obs = diff(ds, [d["arm"] == "withheld" for d in ds], val)
    hit = n = 0
    for _ in range(perms):
        d = diff(ds, [rng.random() < x["p"] for x in ds], val)
        if math.isnan(d):
            continue
        n += 1
        hit += abs(d) >= abs(obs) - 1e-15
    return obs, (hit + 1) / (n + 1)


def cluster_ci(ds: list[dict], reps: int, rng: random.Random) -> tuple[float, float]:
    """Percentile interval, resampling whole session-days."""
    groups: dict[tuple, list[dict]] = collections.defaultdict(list)
    for d in ds:
        groups[(d["session"], d["ts"] // 86400)].append(d)
    keys = list(groups)
    vals = []
    for _ in range(reps):
        sample = [x for k in rng.choices(keys, k=len(keys)) for x in groups[k]]
        v = diff(sample, [x["arm"] == "withheld" for x in sample])
        if not math.isnan(v):
            vals.append(v)
    vals.sort()
    if not vals:
        return float("nan"), float("nan")
    return vals[int(0.025 * len(vals))], vals[min(len(vals) - 1, int(0.975 * len(vals)))]


def two_prop_z(k1: int, n1: int, k2: int, n2: int) -> float:
    if not n1 or not n2:
        return float("nan")
    p = (k1 + k2) / (n1 + n2)
    se = math.sqrt(p * (1 - p) * (1 / n1 + 1 / n2))
    return (k1 / n1 - k2 / n2) / se if se else 0.0


def holm(ps: dict[str, float]) -> dict[str, float]:
    out, running = {}, 0.0
    for i, (k, p) in enumerate(sorted(ps.items(), key=lambda kv: kv[1])):
        running = max(running, min(1.0, p * (len(ps) - i)))
        out[k] = running
    return out


def validity(ds: list[dict], dropped: collections.Counter) -> dict:
    held = [d for d in ds if d["arm"] == "withheld"]
    shown = [d for d in ds if d["arm"] == "shown"]
    n_h, n_s = len(held) + dropped["withheld"], len(shown) + dropped["shown"]
    exp = statistics.mean(d["p"] for d in ds) if ds else 0.0
    n = n_h + n_s
    return {
        "decisions": n,
        "sessions": len({d["session"] for d in ds}),
        "withheld": n_h,
        "shown": n_s,
        "withheld_share": round(n_h / n, 4) if n else None,
        "expected_share": round(exp, 4),
        "share_z": round((n_h / n - exp) / math.sqrt(exp * (1 - exp) / n), 2) if n and 0 < exp < 1 else None,
        "no_following_action": {"withheld": dropped["withheld"], "shown": dropped["shown"]},
        "attrition_z": round(two_prop_z(dropped["withheld"], n_h, dropped["shown"], n_s), 2),
        "relevance_mean": {
            "withheld": round(statistics.mean(d["rel"] for d in held), 4) if held else None,
            "shown": round(statistics.mean(d["rel"] for d in shown), 4) if shown else None,
        },
        "analysed": {"withheld": len(held), "shown": len(shown)},
    }


def analyse(ds: list[dict], dropped: collections.Counter, perms: int, seed: int) -> dict:
    rng = random.Random(seed)
    res: dict = {"validity": validity(ds, dropped)}
    obs, p = permutation_p(ds, perms, rng)
    lo, hi = cluster_ci(ds, 2000, rng)
    base = [d["y"] for d in ds if d["arm"] == "shown"]
    res["primary"] = {
        "bad_rate_shown": round(statistics.mean(base), 4) if base else None,
        "effect_withheld_minus_shown": round(obs, 4),
        "ci95": [round(lo, 4), round(hi, 4)],
        "p": round(p, 4),
        "reading": "positive = showing the memory lowered the bad-outcome rate",
    }
    sec, ps = {}, {}
    for k in KINDS:
        sub = [d for d in ds if d["kind"] == k]
        if min(sum(d["arm"] == "withheld" for d in sub), sum(d["arm"] == "shown" for d in sub)) < 30:
            sec[k] = {"n": len(sub), "skipped": "fewer than 30 in an arm"}
            continue
        o, pv = permutation_p(sub, perms, rng)
        sec[k], ps[k] = {"n": len(sub), "effect": round(o, 4), "p": round(pv, 4)}, pv
    for k, v in holm(ps).items():
        sec[k]["p_holm"] = round(v, 4)
    res["by_kind"] = sec
    res["exploratory"] = {}
    for name, pick in (("stage=prompt", lambda d: d["stage"] != "action"), ("stage=action", lambda d: d["stage"] == "action")):
        sub = [d for d in ds if pick(d)]
        if sub:
            res["exploratory"][name] = {"n": len(sub), "effect": round(diff(sub, [d["arm"] == "withheld" for d in sub]), 4)}
    for j, name in enumerate(("failed", "tests_failed", "reedit_or_revert", "thrash", "denied", "corrected")):
        res["exploratory"][name] = {"effect": round(diff(ds, [d["arm"] == "withheld" for d in ds], lambda d, j=j: d["parts"][j]), 5)}
    return res


def power(ds: list[dict], days: float, seed: int) -> dict:
    rng = random.Random(seed)
    ys = [d["y"] for d in ds]
    out = {"decisions": len(ds), "days": round(days, 2), "sessions": len({d["session"] for d in ds}), "bad_rate": round(statistics.mean(ys), 4)}
    for p in (0.05, 0.10, 0.20):
        null = []
        for _ in range(1000):
            v = diff(ds, [rng.random() < p for _ in ds])
            if not math.isnan(v):
                null.append(v)
        se = statistics.pstdev(null)
        out[f"rate={p}"] = {f"{w}w_detectable": round(2.8 * se * math.sqrt(days / (w * 7)), 4) for w in (2, 4, 8)}
    return out


def selftest() -> None:
    def build(effect: float) -> sqlite3.Connection:
        db = sqlite3.connect(":memory:")
        db.executescript(
            "CREATE TABLE trace_actions(session, ts, seq, failed, tests_fail, reedit, revert, thrash, denied, correction);"
            "CREATE TABLE injections(session, key, ts, p_withhold, kind, stage, relevance);"
            "CREATE TABLE trace_withheld(session, key, ts, p_withhold, kind, stage, relevance);"
        )
        rng = random.Random(1)
        for s in range(40):
            for i in range(150):
                ts = 1000 + i * 10
                held = rng.random() < 0.2
                db.execute(
                    f"INSERT INTO {'trace_withheld' if held else 'injections'} VALUES(?,?,?,?,?,?,?)",
                    (f"s{s}", f"k{i}", ts, 0.2, "fact", "action", 0.5),
                )
                bad = rng.random() < 0.10 + (effect if held else 0.0)
                db.execute("INSERT INTO trace_actions VALUES(?,?,?,?,?,?,?,?,?,?)", (f"s{s}", ts + 1, i, int(bad), -1, 0, 0, 0, 0, 0))
        return db

    null = analyse(*load([build(0.0)], 0, 10**12), perms=300, seed=3)
    real = analyse(*load([build(0.15)], 0, 10**12), perms=300, seed=3)
    assert null["primary"]["p"] > 0.05, null["primary"]
    assert real["primary"]["p"] < 0.01 and real["primary"]["effect_withheld_minus_shown"] > 0.01, real["primary"]
    assert abs(null["validity"]["share_z"]) < 3 and null["validity"]["decisions"] == 6000
    print("selftest ok")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("db", nargs="*")
    ap.add_argument("--start", type=int, default=0, help="unix seconds, inclusive")
    ap.add_argument("--end", type=int, default=10**12, help="unix seconds, exclusive")
    ap.add_argument("--perms", type=int, default=5000)
    ap.add_argument("--seed", type=int, default=20261011)
    ap.add_argument("--counts-only", action="store_true")
    ap.add_argument("--power", action="store_true")
    ap.add_argument("--selftest", action="store_true")
    a = ap.parse_args()
    if a.selftest:
        selftest()
        return 0
    if not a.db:
        ap.error("at least one database")
    dbs = [connect(p) for p in a.db]
    ds, dropped = load(dbs, a.start, a.end, pre=a.power)
    if a.power:
        span = (max(d["ts"] for d in ds) - min(d["ts"] for d in ds)) / 86400 if ds else 0
        out = power(ds, max(span, 1e-9), a.seed)
    elif a.counts_only:
        out = validity(ds, dropped)
    else:
        out = analyse(ds, dropped, a.perms, a.seed)
    json.dump(out, sys.stdout, indent=2)
    print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
