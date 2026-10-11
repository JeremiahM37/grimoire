"""Paraphrase recall set for the clone experiment (E5 + N7).

Takes facts from the latency corpus generator (benchmarks/latency/corpus.py)
and writes three queries per fact, each made by a deterministic rule:

  R1  "Tell me what <S> <PV>."        synonym swap + question frame
  R2  "Is <V> linked to <S>?"         value-anchored yes/no frame
  R3  "What is the <AR> of <S>?"      attribute renaming, value-free

R1 is the SELECTION query: it is shown to the clonal selection (N7) and is
never reported. R2 and R3 are HELD-OUT: they are reported and never used to
choose an alias, a template or a threshold.

Disjointness from the alias templates (go/internal/index/memory_clone.go):
the E5 nouns (holding, preference, timezone, port number, host, pinned version,
release target, repository location), the E5 wh-question frames and the E5
nominal frame "<S> <noun>: <V>" never appear in these rules, and the rule
verbs and attribute renamings (responsible for, stated taste, working zone,
listening socket, deployment machine, version lock, milestone, checkout
location) never appear in E5. The value text <V> does appear in both, because
the fact's own value is the content any query must carry; that is the one
shared vocabulary and it is stated here rather than hidden.

Usage:
  python3 paraphrase.py --n 1000 --scope default --out DIR
Writes ps-<n>-<scope>.json (facts with their queries) and
selection-<n>-<scope>.json (the R1 queries, the GRIMOIRE_CLONE_SELECTION input).
"""
from __future__ import annotations

import argparse
import json
import random
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "latency"))
import corpus  # noqa: E402

SEED = 20261010
PS_SIZE = 300
FACTS_PER_NOTE = corpus.FACTS_PER_NOTE

PV = {  # rule verb phrases, not used by E5
    "owns": "is responsible for", "prefers": "favours", "tz": "is based in",
    "port": "accepts connections on", "host": "is hosted on", "version": "is locked to",
    "release": "is aimed at", "repo": "is kept in",
}
AR = {  # attribute renamings, not used by E5
    "owns": "assignment", "prefers": "stated taste", "tz": "working zone",
    "port": "listening socket", "host": "deployment machine", "version": "version lock",
    "release": "milestone", "repo": "checkout location",
}


def value_text(f: corpus.Fact) -> str:
    v = f.vals
    return {
        "owns": lambda: f"the {v['thing']}",
        "prefers": lambda: v["pref"],
        "tz": lambda: v["tz"],
        "port": lambda: f"port {v['port']}",
        "host": lambda: v["host"],
        "version": lambda: f"version {v['ver']}",
        "release": lambda: f"release {v['ver']}",
        "repo": lambda: v["path"],
    }[f.attr]()


def queries_for(f: corpus.Fact) -> dict[str, str]:
    s = f.display
    return {
        "R1": f"Tell me what {s} {PV[f.attr]}.",
        "R2": f"Is {value_text(f)} linked to {s}?",
        "R3": f"What is the {AR[f.attr]} of {s}?",
    }


def build_ps(n: int, scope: str) -> tuple[list[dict], list[dict]]:
    facts, _ = corpus.build(SEED, n, scope)
    rng = random.Random(f"clone-ps:{n}:{scope}")
    current = [k for k, f in enumerate(facts) if not f.superseded_by]
    chosen = sorted(rng.sample(current, PS_SIZE))
    ps, sel = [], []
    for k in chosen:
        f = facts[k]
        note = f"memory/facts-{k // FACTS_PER_NOTE:05d}.md"
        qs = queries_for(f)
        ps.append({"id": f.id, "note": note, "text": f.text, "attr": f.attr,
                   "display": f.display, "queries": qs})
        sel.append({"note": note, "id": f.id, "q": qs["R1"]})
    return ps, sel


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--n", type=int, required=True)
    ap.add_argument("--scope", default="default", choices=corpus.ALIAS_SCOPES + ("default",))
    ap.add_argument("--out", type=Path, required=True)
    args = ap.parse_args()
    scope = "" if args.scope == "default" else args.scope
    ps, sel = build_ps(args.n, scope)
    args.out.mkdir(parents=True, exist_ok=True)
    (args.out / f"ps-{args.n}-{args.scope}.json").write_text(json.dumps(ps, indent=1) + "\n")
    (args.out / f"selection-{args.n}-{args.scope}.json").write_text(json.dumps(sel, indent=1) + "\n")
    print(f"{len(ps)} facts, {2 * len(ps)} held-out queries, {len(sel)} selection queries -> {args.out}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
