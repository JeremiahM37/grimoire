"""Score the second fresh set (fresh_set_2.py), one half per run.

  python3 benchmarks/injection/measure_fresh2.py dev    # development half
  python3 benchmarks/injection/measure_fresh2.py held   # held-out half, final run only

The dev half is scored during pattern development. The held-out half is
scored once, after development is finished, and the run refuses to start
unless the dev result exists. Results go to results/fresh2.json under
"dev" and "held"; each run overwrites only its own key.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))

import fresh_set_2 as f  # noqa: E402
from measure_inject import _rate, run_scorer  # noqa: E402

OUT = HERE / "results" / "fresh2.json"


def score(half: str) -> dict:
    atk = f.DEV_ATTACKS if half == "dev" else f.HELD_ATTACKS
    ben = f.DEV_BENIGN if half == "dev" else f.HELD_BENIGN
    attacks = [{"id": f"fresh2-{half}-{i:02d}", "text": t} for i, t in enumerate(atk)]
    benign = [{"path": f"fresh2/{half}-{i:02d}", "body": t} for i, t in enumerate(ben)]
    a = run_scorer(attacks, [])
    b = run_scorer([], benign)
    n = len(attacks)
    raw = sum(int(r["raw"]) for r in a["attacks"])
    legacy = sum(int(r["legacy"]) for r in a["attacks"])
    cur = sum(int(r["normalized"]) for r in a["attacks"])
    missed = [t for t, r in zip(atk, a["attacks"]) if not r["normalized"]]
    fp_lines = b.get("benign_flagged_lines", [])
    fp = b["benign_docs_flagged_normalized"]
    return {
        "attacks": {"n": n, "raw": raw, "legacy_normalized": legacy, "normalized": cur,
                    "recall_raw": _rate(raw, n), "recall_normalized": _rate(cur, n),
                    "missed": missed},
        "benign": {"sentences": len(ben), "flagged": fp, "fp_rate": _rate(fp, len(ben)),
                   "flagged_lines": fp_lines},
    }


def main() -> None:
    half = sys.argv[1]
    if half not in ("dev", "held"):
        raise SystemExit("usage: measure_fresh2.py dev|held")
    data = json.loads(OUT.read_text()) if OUT.exists() else {}
    if half == "held" and "dev" not in data:
        raise SystemExit("refusing: the dev half has not been scored")
    if half == "held" and "held" in data:
        raise SystemExit("refusing: the held-out half was already scored")
    data[half] = score(half)
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
    r = data[half]
    a, b = r["attacks"], r["benign"]
    print(f"{half}: attacks {a['normalized']}/{a['n']} current ({100*a['recall_normalized']:.0f}%),"
          f" raw {a['raw']}/{a['n']}, legacy-normalized {a['legacy_normalized']}/{a['n']}")
    for t in a["missed"]:
        print(f"  missed: {t[:120]}")
    print(f"{half}: benign sentences flagged {b['flagged']}/{b['sentences']}")
    for line in b["flagged_lines"]:
        print(f"  flagged: {line[:150]}")


if __name__ == "__main__":
    main()
