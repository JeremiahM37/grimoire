"""Token overhead of datamarking on the benign corpus.

Runs go/internal/trust TestSpotlightOverheadFences to get the fenced form of
every benign text with the flag off and on, then counts tokens with the Laya
tokenizer (the local model family the lab uses). Writes a summary to stdout and
to OUT_DIR/overhead.json.

Usage: python3 guard_overhead.py ITEMS_JSONL OUT_DIR
"""

from __future__ import annotations

import json
import os
import statistics
import subprocess
import sys
from pathlib import Path

TOKENIZER = "/mnt/bulk/laya/ft2-mixed/tokenizer"
GO_DIR = Path(__file__).resolve().parents[2] / "go"


def main() -> None:
    items_path, out_dir = Path(sys.argv[1]), Path(sys.argv[2])
    out_dir.mkdir(parents=True, exist_ok=True)
    items = [json.loads(line) for line in items_path.read_text().splitlines() if line.strip()]
    benign = [it for it in items if it["kind"] == "benign"]
    seen, uniq = set(), []
    for it in benign:
        if it["text"] not in seen:
            seen.add(it["text"])
            uniq.append({"id": it["id"], "text": it["text"]})
    bin_path = out_dir / "benign.jsonl"
    bin_path.write_text(
        "".join(json.dumps(u, ensure_ascii=False) + "\n" for u in uniq), encoding="utf-8"
    )
    fenced = out_dir / "fenced.jsonl"
    env = dict(
        os.environ,
        GUARD_OVERHEAD_IN=str(bin_path),
        GUARD_OVERHEAD_OUT=str(fenced),
        PATH="/usr/local/go/bin:" + os.environ.get("PATH", ""),
    )
    env.pop("GRIMOIRE_SESSION", None)
    env.pop("GRIMOIRE_EXP", None)
    subprocess.run(
        ["go", "test", "./internal/trust/", "-run", "TestSpotlightOverheadFences", "-count=1"],
        cwd=GO_DIR,
        env=env,
        check=True,
        timeout=300,
    )

    from transformers import AutoTokenizer  # the laya venv provides transformers

    tok = AutoTokenizer.from_pretrained(TOKENIZER)
    off, on, raw = [], [], []
    rows = [
        json.loads(line) for line in fenced.read_text(encoding="utf-8").splitlines() if line.strip()
    ]
    for r in rows:
        off.append(len(tok.encode(r["off"], add_special_tokens=False)))
        on.append(len(tok.encode(r["on"], add_special_tokens=False)))
        raw.append(
            len(
                tok.encode(
                    next(u["text"] for u in uniq if u["id"] == r["id"]), add_special_tokens=False
                )
            )
        )
    summary = {
        "n_texts": len(rows),
        "tokenizer": TOKENIZER,
        "raw_text_tokens_total": sum(raw),
        "fence_off_tokens_total": sum(off),
        "fence_on_tokens_total": sum(on),
        "fence_on_over_off": round(sum(on) / sum(off), 3),
        "extra_tokens_per_text_median": statistics.median(
            b - a for a, b in zip(off, on, strict=True)
        ),
        "fence_off_tokens_median": statistics.median(off),
        "fence_on_tokens_median": statistics.median(on),
        "extra_tokens_per_text_mean": round(
            statistics.mean(b - a for a, b in zip(off, on, strict=True)), 1
        ),
    }
    (out_dir / "overhead.json").write_text(json.dumps(summary, indent=1))
    print(json.dumps(summary, indent=1))


if __name__ == "__main__":
    main()
