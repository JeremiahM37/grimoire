"""Build the labelled item set for the `guard` lab (E7).

Every item is one piece of text with:
  id      stable id
  text    the text a reader would see
  kind    "attack" or "benign"
  group   the held-out unit: a corpus family, an exfil family, or a fresh set
  fold    0..3, the grouped fold that holds this item out for testing

Attack groups come from the corpus's own family labels (corpus.py,
exfil_corpus.py) and from the two independently written fresh sets (which have
no family labels, so each set is one group). Attacks are placed into four folds
by group, so a fold's attack families are never seen in training.

Benign text is paragraphs from the repository's own docs and root markdown,
excluding the injection-defence documents (they quote attacks), plus the two
fresh hard-negative sets. Benign items get a fold by hash so each benign text
is held out exactly once.

Usage: python3 guard_dataset.py OUT_DIR   (writes OUT_DIR/items.jsonl)
"""

from __future__ import annotations

import hashlib
import json
import random
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
REPO = HERE.parent.parent
sys.path.insert(0, str(HERE))

import corpus  # noqa: E402
import exfil_corpus  # noqa: E402
import fresh_set  # noqa: E402
import fresh_set_2  # noqa: E402

# Attack group -> fold. Grouped 4-fold, groups never split across folds.
GROUP_FOLD = {
    "fresh": 0,
    "override": 0,
    "setup": 0,
    "fresh2": 1,
    "escape": 1,
    "exfiltration": 2,
    "memory": 2,
    "policy": 2,
    "authority": 3,
    "error": 3,
    "helpful": 3,
    "quiet": 3,
}

EXFIL_TARGET = "https://collect.example.net/u"
BENIGN_EXCLUDE = ("INJECTION", "MEMORY-LIMITS")
BENIGN_SAMPLE = 300


def attacks() -> list[dict]:
    out = []
    for a in corpus.ATTACKS:
        out.append(
            {
                "id": f"corpus-{a.id}",
                "text": a.payload.replace("{canary}", a.canary()),
                "kind": "attack",
                "group": a.family,
                "source": "corpus",
            }
        )
    for a in exfil_corpus.ATTACKS:
        out.append(
            {
                "id": f"exfil-{a.id}",
                "text": a.payload.replace("{target}", EXFIL_TARGET),
                "kind": "attack",
                "group": a.family,
                "source": "exfil",
            }
        )
    for i, t in enumerate(fresh_set.FRESH_ATTACKS):
        out.append(
            {
                "id": f"fresh-{i:02d}",
                "text": t,
                "kind": "attack",
                "group": "fresh",
                "source": "fresh",
            }
        )
    for i, t in enumerate(fresh_set_2.FRESH2_ATTACKS):
        out.append(
            {
                "id": f"fresh2-{i:02d}",
                "text": t,
                "kind": "attack",
                "group": "fresh2",
                "source": "fresh2",
            }
        )
    return out


def benign() -> list[dict]:
    out = []
    for i, t in enumerate(fresh_set.FRESH_BENIGN):
        out.append(
            {
                "id": f"hardneg1-{i:02d}",
                "text": t,
                "kind": "benign",
                "group": "hardneg1",
                "source": "fresh",
            }
        )
    for i, t in enumerate(fresh_set_2.FRESH2_BENIGN):
        out.append(
            {
                "id": f"hardneg2-{i:02d}",
                "text": t,
                "kind": "benign",
                "group": "hardneg2",
                "source": "fresh2",
            }
        )
    paras = []
    files = sorted((REPO / "docs").glob("*.md")) + sorted(REPO.glob("*.md"))
    for p in files:
        if any(x in p.name.upper() for x in BENIGN_EXCLUDE):
            continue
        for block in p.read_text(encoding="utf-8", errors="replace").split("\n\n"):
            b = " ".join(block.split())
            if 60 <= len(b) <= 600 and not b.startswith(("#", "|", "```", "-", "*")):
                paras.append((str(p.relative_to(REPO)), b))
    rng = random.Random(1)
    rng.shuffle(paras)
    for i, (path, b) in enumerate(paras[:BENIGN_SAMPLE]):
        out.append(
            {"id": f"doc-{i:03d}", "text": b, "kind": "benign", "group": "docs", "source": path}
        )
    return out


def fold_of(item: dict) -> int:
    if item["kind"] == "attack":
        return GROUP_FOLD[item["group"]]
    h = hashlib.sha256(item["id"].encode()).digest()[0]
    return h % 4


def main() -> None:
    out_dir = Path(sys.argv[1])
    out_dir.mkdir(parents=True, exist_ok=True)
    items = attacks() + benign()
    for it in items:
        it["fold"] = fold_of(it)
    with open(out_dir / "items.jsonl", "w", encoding="utf-8") as f:
        for it in items:
            f.write(json.dumps(it, ensure_ascii=False) + "\n")
    na = sum(1 for i in items if i["kind"] == "attack")
    print(f"items={len(items)} attacks={na} benign={len(items) - na}")
    for g in sorted({i["group"] for i in items}):
        sel = [i for i in items if i["group"] == g]
        print(f"  {g:14s} n={len(sel):3d} fold={sel[0]['fold']}")


if __name__ == "__main__":
    main()
