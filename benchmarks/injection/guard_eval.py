"""Evaluate the `guard` detectors with grouped 4-fold cross-validation.

Subcommands:
  prepare  write per-fold Laya training rows (Jev labels as soft targets)
  score    POST every needed item to a laya-serve model and write scores
  report   compute per-held-out-group recall and benign false positives

Protocol (see docs/lab/guard.md):
  * attack groups are held out whole (grouped 4-fold; a group never splits);
  * benign text is held out once each (fold by hash of its id);
  * for fold k, 25% of the other folds' benign text is a CALIBRATION set that
    is not used to train the Laya head;
  * a classifier's threshold is the highest calibration-benign score, so the
    calibration false-positive rate is 0 (<= 1% for any n here);
  * recall is per held-out group; false positives are over the held-out
    benign of fold k, pooled over k so every benign text is counted once.
The pattern scanner has no threshold; its flags come from guard_bench_test.go.
"""

from __future__ import annotations

import hashlib
import json
import sys
import urllib.request
from collections import defaultdict
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from guard_jev import CRITERIA, INSTRUCTIONS, QUESTION_ID, text_key  # noqa: E402

FOLDS = 4


def h(s: str, salt: str) -> int:
    return hashlib.sha256((salt + s).encode()).digest()[0]


def cal_set(item: dict, k: int) -> bool:
    """Calibration benign for fold k: other folds' benign, ~25% by hash."""
    return item["kind"] == "benign" and item["fold"] != k and h(item["id"], "cal") % 4 == 0


def train_set(item: dict, k: int) -> bool:
    if item["fold"] == k:
        return False
    if item["kind"] == "attack":
        return True
    return not cal_set(item, k)


class Work:
    def __init__(self, root: Path):
        self.root = root
        self.items = [
            json.loads(line)
            for line in (root / "data/items.jsonl").read_text().splitlines()
            if line.strip()
        ]
        self.labels = {}
        for line in (root / "jev-labels.jsonl").read_text().splitlines():
            if line.strip():
                r = json.loads(line)
                self.labels[r["text_key"]] = r["p_yes"]
        for it in self.items:
            it["key"] = text_key(it["text"])
            it["jev"] = self.labels[it["key"]]


def prepare(w: Work) -> None:
    for k in range(FOLDS):
        d = w.root / "laya" / f"fold{k}"
        d.mkdir(parents=True, exist_ok=True)
        rows = []
        for it in w.items:
            if not train_set(it, k):
                continue
            p = it["jev"]
            rows.append(
                {
                    "state": it["text"],
                    "questions": {
                        QUESTION_ID: {
                            "type": "noul",
                            "instructions": INSTRUCTIONS,
                            "criteria": CRITERIA,
                        }
                    },
                    "gold": {QUESTION_ID: {"probabilities": {"true": p, "false": round(1 - p, 6)}}},
                }
            )
        seen, uniq = set(), []
        for r in rows:  # duplicate texts appear once
            if r["state"] not in seen:
                seen.add(r["state"])
                uniq.append(r)
        with open(d / "train.jsonl", "w", encoding="utf-8") as f:
            for r in uniq:
                f.write(json.dumps(r, ensure_ascii=False) + "\n")
        n_att = sum(1 for it in w.items if train_set(it, k) and it["kind"] == "attack")
        print(f"fold {k}: train rows={len(uniq)} attacks={n_att} benign={len(uniq) - n_att}")


def score(w: Work, base: str, models: dict[int, str], out: Path) -> None:
    """models maps fold -> served model name. Scores every item the fold uses for
    testing or calibration; training items are not scored by that fold's model."""
    rows = []
    for k, name in models.items():
        for it in w.items:
            if it["fold"] != k and not cal_set(it, k):
                continue
            body = {
                "state": it["text"],
                "model": name,
                "questions": {
                    QUESTION_ID: {
                        "type": "noul",
                        "instructions": INSTRUCTIONS,
                        "criteria": CRITERIA,
                    }
                },
            }
            req = urllib.request.Request(
                base.rstrip("/") + "/v1/systemone",
                data=json.dumps(body).encode(),
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=120) as resp:
                ans = json.loads(resp.read())["answers"][QUESTION_ID]
            rows.append(
                {
                    "fold": k,
                    "id": it["id"],
                    "key": it["key"],
                    "score": float(ans["noul"]),
                    "role": "test" if it["fold"] == k else "cal",
                }
            )
    with open(out, "w", encoding="utf-8") as f:
        for r in rows:
            f.write(json.dumps(r) + "\n")
    print(f"scored {len(rows)} rows -> {out}")


def auc(pos: list[float], neg: list[float]) -> float:
    """ROC AUC by the Mann-Whitney count: ties count half."""
    if not pos or not neg:
        return float("nan")
    wins = 0.0
    for p in pos:
        for n in neg:
            wins += 1.0 if p > n else 0.5 if p == n else 0.0
    return wins / (len(pos) * len(neg))


def report(w: Work, laya_path: Path, scanner_path: Path) -> dict:
    scanner = {
        json.loads(line)["id"]: json.loads(line)["scanner"]
        for line in scanner_path.read_text().splitlines()
        if line.strip()
    }
    laya = defaultdict(dict)  # (fold) -> id -> score
    have_laya = laya_path.exists()
    if have_laya:
        for line in laya_path.read_text().splitlines():
            if line.strip():
                r = json.loads(line)
                laya[r["fold"]][r["id"]] = r["score"]
    res = {
        "groups": defaultdict(lambda: defaultdict(int)),
        "benign": defaultdict(lambda: defaultdict(int)),
        "thresholds": {},
    }
    for k in range(FOLDS):
        cal = [it for it in w.items if it["kind"] == "benign" and cal_set(it, k)]
        t_jev = max(it["jev"] for it in cal)
        t_laya = max(laya[k][it["id"]] for it in cal) if have_laya else None
        res["thresholds"][k] = {"jev": t_jev, "laya": t_laya, "n_cal_benign": len(cal)}
        for it in w.items:
            if it["fold"] != k:
                continue
            flags = {"scanner": bool(scanner[it["id"]]), "jev": it["jev"] > t_jev}
            if have_laya:
                flags["laya"] = laya[k][it["id"]] > t_laya
                flags["union"] = flags["scanner"] or flags["laya"]
            flags["union_jev"] = flags["scanner"] or flags["jev"]
            bucket = res["groups"] if it["kind"] == "attack" else res["benign"]
            name = it["group"] if it["kind"] == "attack" else it["group"]
            cell = bucket[name]
            cell["n"] += 1
            for m, v in flags.items():
                cell[m] += int(v)
    # Threshold-free view: pooled over held-out folds, each item scored by the
    # model that did not train on it (Jev is zero-shot, so its score is fixed).
    pools = {"jev": ([], []), "laya": ([], [])}
    for it in w.items:
        k = it["fold"]
        if it["kind"] == "benign" and cal_set(it, k):
            continue
        lab = 1 if it["kind"] == "attack" else 0
        (pools["jev"][0] if lab == 0 else pools["jev"][1]).append(it["jev"])
        if have_laya and it["id"] in laya[k]:
            (pools["laya"][0] if lab == 0 else pools["laya"][1]).append(laya[k][it["id"]])
    res["auc"] = {
        "jev": auc(pools["jev"][1], pools["jev"][0]),
        "laya": auc(pools["laya"][1], pools["laya"][0]) if have_laya else None,
    }
    return res


def main() -> None:
    cmd = sys.argv[1]
    root = Path("/mnt/bulk/lab-work/guard")
    w = Work(root)
    if cmd == "prepare":
        prepare(w)
    elif cmd == "score":
        base = sys.argv[2]
        models = {int(a.split("=")[0]): a.split("=")[1] for a in sys.argv[3:]}
        score(w, base, models, root / "laya-scores.jsonl")
    elif cmd == "report":
        res = report(w, root / "laya-scores.jsonl", root / "scanner.jsonl")
        print(json.dumps(res, indent=1, default=dict))
    else:
        raise SystemExit(__doc__)


if __name__ == "__main__":
    main()
