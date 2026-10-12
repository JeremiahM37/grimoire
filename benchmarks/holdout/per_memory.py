#!/usr/bin/env python3
"""EXPLORATORY per-memory effect estimates for the memory holdout.

Not part of the pre-registered analysis (see PREREG.md, Amendments). Nothing this
script prints is a finding of that analysis. It asks which memories, when shown,
seem to lower or raise the bad-outcome rate, using the same database, loader
(`analyze.load`) and outcome definition as `analyze.py`.

Identity. A memory is identified by `target` ("fact:ID" or "note:PATH"). It is the
only memory column both arms share: `injections` and `trace_withheld` both have
session, key, ts, target, kind, stage, relevance and p_withhold, but only
`injections` has `fact_id` and `path`, so those cannot identify withheld rows.
`key` is the per-injection dedupe key, not the memory. `analyze.load` does not
return `target`, so it is attached here by joining each loaded decision back to the
rows it came from on (arm, session, ts, kind, stage, relevance, p_withhold). A
decision whose join is ambiguous (two different targets) is left unattributed and
counted. Outcomes come only from `analyze.load`.

Estimator. For each memory with at least --min-per-arm decisions with an outcome in
each arm: effect = weighted mean Y withheld minus weighted mean Y shown. Weights are
inverse probabilities from the logged p_withhold (Hajek form, self-normalised per
arm). With constant p this is the plain difference of means. Standard errors are
the usual IPW sandwich-style terms, treating decisions as independent. They ignore
the clustering of decisions within a session, so they are optimistic.

Shrinkage. Empirical-Bayes normal-normal model. The between-memory variance tau^2
is the DerSimonian-Laird method-of-moments estimate. It is set to 0 and flagged
when negative. Each effect is shrunk toward the precision-weighted overall effect
with weight tau^2 / (tau^2 + se^2).

Heterogeneity. A one-sided permutation test (2,000 by default, seed 20261011). Arms
are redrawn with each decision's logged probability, as the server assigned them,
and the statistic is the n-weighted variance of the per-memory raw effects.

By kind and by source type use the same estimator with decisions grouped by kind,
or by source type derived from the target: fact:* = grimoire_fact, note under
"Agent Memory/" or a "/memory/" directory = agent_memory_file, other note = other_note.

Guard. Refuses to read outcomes before the pre-registered end (2026-11-08 00:00
UTC). There is no override. `--selftest` uses synthetic data only.

    per_memory.py DB [DB...] [--start T] [--end T] [--min-per-arm 5] [--perms 2000]
    per_memory.py --selftest
"""

from __future__ import annotations

import argparse
import collections
import datetime
import json
import math
import random
import sqlite3
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))  # import analyze.py beside this file
import analyze  # noqa: E402

END_UTC = datetime.datetime(2026, 11, 8, tzinfo=datetime.UTC).timestamp()
SEED = 20261011
SELFTEST_PERMS = 200  # selftest only; the real run defaults to 2000
TOP = 20


# --- identity -------------------------------------------------------------------


def attach_identity(dbs: list[sqlite3.Connection], ds: list[dict]) -> int:
    """Sets d["memory"] on each decision it can attribute. Returns the number
    unattributed (ambiguous join)."""
    idx: dict[tuple, set[str]] = collections.defaultdict(set)
    seen: dict[tuple, tuple] = {}
    queries = (
        (
            "shown",
            "SELECT session, key, ts, target, kind, stage, relevance, p_withhold FROM injections WHERE p_withhold>0",
        ),
        (
            "withheld",
            "SELECT session, key, ts, target, kind, stage, relevance, p_withhold FROM trace_withheld",
        ),
    )
    for arm, q in queries:
        for db in dbs:
            for s, key, ts, target, kind, stage, rel, p in db.execute(q):
                seen[(arm, s, key, ts)] = (target, kind or "", stage or "", rel or 0.0, p)
    for (arm, s, _key, ts), (target, kind, stage, rel, p) in seen.items():
        idx[(arm, s, ts, kind, stage, rel, p)].add(target)
    unattributed = 0
    for d in ds:
        targets = idx.get(
            (d["arm"], d["session"], d["ts"], d["kind"], d["stage"], d["rel"], d["p"]), set()
        )
        if len(targets) == 1:
            d["memory"] = next(iter(targets))
        else:
            unattributed += 1
    return unattributed


def source_of(target: str) -> str:
    if target.startswith("fact:"):
        return "grimoire_fact"
    if target.startswith("note:"):
        path = target[len("note:") :]
        if "Agent Memory/" in path or "/memory/" in path:
            return "agent_memory_file"
        return "other_note"
    return "unknown"


# --- estimator ------------------------------------------------------------------


def group_effect(rows: list[dict], min_per_arm: int):
    """(effect, se, n_shown, n_withheld) for one group, or None if it has fewer
    than min_per_arm decisions with an outcome in either arm."""
    s1 = [d for d in rows if d["arm"] == "withheld"]
    s0 = [d for d in rows if d["arm"] == "shown"]
    if min(len(s1), len(s0)) < min_per_arm:
        return None
    w1 = [1 / d["p"] for d in s1]
    w0 = [1 / (1 - d["p"]) for d in s0]
    b1, b0 = sum(w1), sum(w0)
    mu1 = sum(w * d["y"] for w, d in zip(w1, s1, strict=True)) / b1
    mu0 = sum(w * d["y"] for w, d in zip(w0, s0, strict=True)) / b0
    se1 = math.sqrt(sum((w * (d["y"] - mu1)) ** 2 for w, d in zip(w1, s1, strict=True))) / b1
    se0 = math.sqrt(sum((w * (d["y"] - mu0)) ** 2 for w, d in zip(w0, s0, strict=True))) / b0
    return mu1 - mu0, math.hypot(se1, se0), len(s0), len(s1)


def empirical_bayes(effects: dict[str, tuple]) -> dict:
    """effects: name -> (effect, se, n_shown, n_withheld). Returns the shrinkage
    summary and one row per name."""
    names = sorted(effects)
    k = len(names)
    th = {n: effects[n][0] for n in names}
    se2 = {n: max(effects[n][1], 1e-12) ** 2 for n in names}
    out = {"tau2": None, "tau2_clamped_to_zero": False, "overall": None, "rows": {}}
    if k < 2:
        return out | {
            "tau2": 0.0,
            "overall": th[names[0]] if names else None,
            "rows": {n: _eb_row(th[n], se2[n], 0.0, th[n], effects[n]) for n in names},
        }
    w = {n: 1 / se2[n] for n in names}
    sw = sum(w.values())
    mu_fixed = sum(w[n] * th[n] for n in names) / sw
    q = sum(w[n] * (th[n] - mu_fixed) ** 2 for n in names)
    c = sw - sum(w[n] ** 2 for n in names) / sw
    raw = (q - (k - 1)) / c
    tau2 = max(0.0, raw)
    out["tau2_clamped_to_zero"] = raw < 0
    wr = {n: 1 / (tau2 + se2[n]) for n in names}
    mu = sum(wr[n] * th[n] for n in names) / sum(wr.values())
    out["tau2"], out["overall"] = tau2, mu
    out["rows"] = {n: _eb_row(th[n], se2[n], tau2, mu, effects[n]) for n in names}
    return out


def _eb_row(theta, se2, tau2, mu, eff) -> dict:
    b = tau2 / (tau2 + se2) if tau2 + se2 > 0 else 0.0
    return {
        "raw_effect": theta,
        "shrunk_effect": mu + b * (theta - mu),
        "se": math.sqrt(se2),
        "n_shown": eff[2],
        "n_withheld": eff[3],
    }


def _stat_T(ys, ps, held, ranges, min_per_arm):
    """n-weighted variance of per-group raw effects (IPW, same as group_effect)."""
    thetas, ns = [], []
    for s, e in ranges:
        a1 = b1 = a0 = b0 = 0.0
        n1 = n0 = 0
        for i in range(s, e):
            y = ys[i]
            p = ps[i]
            if held[i]:
                w = 1 / p
                a1 += w * y
                b1 += w
                n1 += 1
            else:
                w = 1 / (1 - p)
                a0 += w * y
                b0 += w
                n0 += 1
        if n1 >= min_per_arm and n0 >= min_per_arm:
            thetas.append(a1 / b1 - a0 / b0)
            ns.append(n1 + n0)
    if len(thetas) < 2:
        return None
    nt = sum(ns)
    tb = sum(n * t for n, t in zip(ns, thetas, strict=True)) / nt
    return sum(n * (t - tb) ** 2 for n, t in zip(ns, thetas, strict=True)) / nt


def heterogeneity_p(
    ds: list[dict], min_per_arm: int, perms: int, seed: int
) -> tuple[float, float, int]:
    """(observed statistic, one-sided permutation p, number of valid permutations)."""
    ordered = sorted(ds, key=lambda d: d["memory"])
    ys = [d["y"] for d in ordered]
    ps = [d["p"] for d in ordered]
    ranges, start = [], 0
    for i in range(1, len(ordered) + 1):
        if i == len(ordered) or ordered[i]["memory"] != ordered[start]["memory"]:
            ranges.append((start, i))
            start = i
    obs = _stat_T(ys, ps, [d["arm"] == "withheld" for d in ordered], ranges, min_per_arm)
    if obs is None:
        return None, None, 0
    rng = random.Random(seed)
    hit = n = 0
    for _ in range(perms):
        t = _stat_T(ys, ps, [rng.random() < p for p in ps], ranges, min_per_arm)
        if t is None:
            continue
        n += 1
        hit += t >= obs
    return obs, (hit + 1) / (n + 1), n


def estimate(ds: list[dict], key, min_per_arm: int) -> tuple[dict[str, tuple], list[str]]:
    groups: dict[str, list[dict]] = collections.defaultdict(list)
    for d in ds:
        groups[key(d)].append(d)
    eff, skipped = {}, []
    for g, rows in groups.items():
        r = group_effect(rows, min_per_arm)
        if r is None:
            skipped.append(g)
        else:
            eff[g] = r
    return eff, skipped


def _entries(eb: dict) -> list[dict]:
    return [
        {"memory": n, **{k: round(v, 4) if isinstance(v, float) else v for k, v in r.items()}}
        for n, r in sorted(eb["rows"].items(), key=lambda kv: -kv[1]["shrunk_effect"])
    ]


def analyse(ds: list[dict], unattributed: int, min_per_arm: int, perms: int, seed: int) -> dict:
    dec = [d for d in ds if "memory" in d]
    eff, skipped = estimate(dec, lambda d: d["memory"], min_per_arm)
    eb = empirical_bayes(eff)
    ranked = _entries(eb)
    obs, p, nvalid = heterogeneity_p(dec, min_per_arm, perms, seed) if eff else (None, None, 0)
    kind_eff, _ = estimate(dec, lambda d: d["kind"] or "unknown", min_per_arm)
    src_eff, _ = estimate(dec, lambda d: source_of(d["memory"]), min_per_arm)
    rnd = lambda x: None if x is None else round(x, 4)  # noqa: E731
    return {
        "exploratory": True,
        "not_a_pre_registered_finding": True,
        "identity_column": "target",
        "min_per_arm": min_per_arm,
        "decisions": len(ds),
        "unattributed_decisions": unattributed,
        "memories_estimated": len(eff),
        "memories_below_min_per_arm": len(skipped),
        "between_memory_variance": rnd(eb["tau2"]),
        "between_memory_variance_clamped_to_zero": eb["tau2_clamped_to_zero"],
        "overall_effect": rnd(eb["overall"]),
        "heterogeneity_statistic": rnd(obs),
        "heterogeneity_p": rnd(p),
        "heterogeneity_valid_permutations": nvalid,
        "helpful": ranked[:TOP],
        "harmful": ranked[-TOP:][::-1],
        "by_kind": _group_table(kind_eff),
        "by_source": _group_table(src_eff),
    }


def _group_table(eff: dict) -> dict:
    eb = empirical_bayes(eff)
    return {
        "between_group_variance": None if eb["tau2"] is None else round(eb["tau2"], 4),
        "groups": [
            {"group": g, **{k: round(v, 4) if isinstance(v, float) else v for k, v in r.items()}}
            for g, r in sorted(eb["rows"].items())
        ],
    }


def invalid_p(ds: list[dict]) -> tuple[list[dict], int]:
    ok = [d for d in ds if 0 < d["p"] < 1]
    return ok, len(ds) - len(ok)


# --- selftest -------------------------------------------------------------------


def _synthetic_db(
    seed: int, planted: dict[int, tuple[float, float]], n_mem: int = 200, per_mem: int = 120
):
    """planted: memory index -> (bad rate when shown, bad rate when withheld)."""
    db = sqlite3.connect(":memory:")
    db.executescript(
        "CREATE TABLE trace_actions(session, ts, seq, failed, tests_fail, reedit, revert, thrash, denied, correction);"
        "CREATE TABLE injections(session, key, ts, p_withhold, kind, stage, relevance, target);"
        "CREATE TABLE trace_withheld(session, key, ts, p_withhold, kind, stage, relevance, target);"
    )
    rng = random.Random(seed)
    for m in range(n_mem):
        shown_rate, held_rate = planted.get(m, (0.10, 0.10))
        target = f"fact:M{m:03d}"
        for j in range(per_mem):
            s = f"s{m}-{j}"  # one decision per session: each outcome is that decision's next action
            ts = 1000 + (m * per_mem + j) * 10
            held = rng.random() < 0.10
            bad = rng.random() < (held_rate if held else shown_rate)
            table = "trace_withheld" if held else "injections"
            db.execute(
                f"INSERT INTO {table} VALUES(?,?,?,?,?,?,?,?)",
                (s, f"k{m}", ts, 0.10, "fact", "action", 0.5, target),
            )
            db.execute(
                "INSERT INTO trace_actions VALUES(?,?,?,?,?,?,?,?,?,?)",
                (s, ts + 1, 0, int(bad), -1, 0, 0, 0, 0, 0),
            )
    return db


def selftest() -> None:
    # IPW with constant p reduces to the plain difference of means.
    rows = [
        {"arm": "withheld", "y": 1.0, "p": 0.1},
        {"arm": "withheld", "y": 0.0, "p": 0.1},
        {"arm": "shown", "y": 0.0, "p": 0.1},
        {"arm": "shown", "y": 0.0, "p": 0.1},
        {"arm": "shown", "y": 1.0, "p": 0.1},
        {"arm": "shown", "y": 1.0, "p": 0.1},
    ]
    eff = group_effect(rows, 2)
    assert eff is not None and abs(eff[0] - (0.5 - 0.5)) < 1e-12, eff
    assert group_effect(rows, 3) is None

    helpful = set(range(0, 20))
    harmful = set(range(20, 40))
    planted = {m: (0.10, 0.50) for m in helpful} | {m: (0.50, 0.10) for m in harmful}
    db = _synthetic_db(101, planted)
    ds, _ = analyze.load([db], 0, 10**12)
    assert invalid_p(ds)[1] == 0
    unattr = attach_identity([db], ds)
    assert unattr == 0, unattr
    real = analyse(ds, unattr, 5, SELFTEST_PERMS, SEED)
    assert real["heterogeneity_p"] < 0.05, real["heterogeneity_p"]
    top = [int(e["memory"][len("fact:M") :]) for e in real["helpful"]]
    bot = [int(e["memory"][len("fact:M") :]) for e in real["harmful"]]
    n_help = sum(m in helpful for m in top)
    n_harm = sum(m in harmful for m in bot)
    assert n_help >= 12, (n_help, top)
    assert n_harm >= 12, (n_harm, bot)

    null_p = []
    for seed in range(20):
        db = _synthetic_db(1000 + seed, {})
        ds, _ = analyze.load([db], 0, 10**12)
        attach_identity([db], ds)
        null_p.append(analyse(ds, 0, 5, SELFTEST_PERMS, SEED)["heterogeneity_p"])
    above = sum(p > 0.05 for p in null_p)
    assert above >= 18, (above, null_p)

    print("constant-p IPW equals difference of means: ok")
    print(
        f"planted: memories={real['memories_estimated']} tau2={real['between_memory_variance']} "
        f"heterogeneity_p={real['heterogeneity_p']} (perms={SELFTEST_PERMS})"
    )
    print(
        f"planted helpful in top {TOP}: {n_help}/{TOP}; planted harmful in bottom {TOP}: {n_harm}/{TOP}"
    )
    print(f"null: heterogeneity_p > 0.05 in {above}/20 seeds")
    print("SELFTEST PASS")


# --- CLI ------------------------------------------------------------------------


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument("db", nargs="*")
    ap.add_argument("--start", type=int, default=0, help="unix seconds, inclusive")
    ap.add_argument("--end", type=int, default=10**12, help="unix seconds, exclusive")
    ap.add_argument("--min-per-arm", type=int, default=5)
    ap.add_argument("--perms", type=int, default=2000)
    ap.add_argument("--seed", type=int, default=SEED)
    ap.add_argument("--selftest", action="store_true")
    a = ap.parse_args()
    if a.selftest:
        selftest()
        return 0
    if datetime.datetime.now(datetime.UTC).timestamp() < END_UTC:
        print(
            "per_memory.py refuses to read outcomes before the pre-registered end "
            "(2026-11-08 00:00 UTC). Use --selftest for the synthetic check.",
            file=sys.stderr,
        )
        return 2
    if not a.db:
        ap.error("at least one database")
    dbs = [analyze.connect(p) for p in a.db]
    ds, _dropped = analyze.load(dbs, a.start, a.end)
    ds, bad_p = invalid_p(ds)
    unattr = attach_identity(dbs, ds)
    out = analyse(ds, unattr, a.min_per_arm, a.perms, a.seed)
    out["invalid_p_dropped"] = bad_p
    json.dump(out, sys.stdout, indent=2)
    print()
    return 0


if __name__ == "__main__":
    sys.exit(main())
