"""Paraphrase recall for the clone experiment: one server per (size, scope, mode).

For each run it writes the latency corpus into a fresh vault, starts the real
grimoire binary with the mode's GRIMOIRE_EXP (and the selection / paraphrase
files), and sends the HELD-OUT paraphrase queries R2 and R3 for the paraphrase
set (ps.py) over one keep-alive connection. R1 is never sent: it is the
selection query. Reports gold@10, recall p50/p95, and what the server said it
did.

Flag-on proof: the server logs a "clone:" line at the end of every reindex
only when the clone flag is on, and a "clone-n7:" line only when N7 ran. The
run fails if a flag-on mode produced no such line, or a flag-off mode did.

Usage:
  python3 run_clone.py --binary BIN --work DIR --out DIR --sizes 1000,10000
      --scopes default,session --modes off,clone,clone-n7 [--llm-file F] [--port 19060]
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import statistics
import sys
import urllib.parse
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent / "latency"))
import bench  # noqa: E402
import corpus  # noqa: E402

SEED = bench.SEED

MODES = {
    "off": ("", False, False, False),
    "clone": ("clone", False, False, False),
    "clone-llm": ("clone,clone-llm", True, False, False),
    "clone-n7": ("clone,clone-n7", False, True, False),
    "clone-llm-n7": ("clone,clone-llm,clone-n7", True, True, False),
}


def pct(xs: list[float], p: float) -> float:
    return bench.pct(xs, p)


def run_one(binary: Path, work: Path, n: int, scope: str, mode: str, ps_dir: Path,
            llm_file: Path | None, port: int) -> dict:
    exp_value, use_llm, use_n7, _ = MODES[mode]
    tag = f"{n}-{scope or 'default'}-{mode}"
    vault = work / f"vault-{tag}"
    if vault.exists():
        shutil.rmtree(vault)
    facts, _ = corpus.build(SEED, n, scope)
    corpus.write_vault(vault, facts)
    ps = json.loads((ps_dir / f"ps-{n}-{scope or 'default'}.json").read_text())
    sel = ps_dir / f"selection-{n}-{scope or 'default'}.json"

    os.environ.pop("GRIMOIRE_EXP", None)
    os.environ.pop("GRIMOIRE_CLONE_SELECTION", None)
    os.environ.pop("GRIMOIRE_CLONE_LLM_FILE", None)
    if exp_value:
        os.environ["GRIMOIRE_EXP"] = exp_value
    if use_n7:
        os.environ["GRIMOIRE_CLONE_SELECTION"] = str(sel)
    if use_llm:
        os.environ["GRIMOIRE_CLONE_LLM_FILE"] = str(llm_file)

    log = work / f"server-{tag}.log"
    proc, out, cold_s, health = bench.start(binary, vault, port, log)
    try:
        log_text = log.read_text()
        clone_line = next((ln for ln in log_text.splitlines() if re.search(r"\bclone: ", ln)), "")
        n7_line = next((ln for ln in log_text.splitlines() if "clone-n7:" in ln), "")
        flag_live = bool(clone_line)
        if flag_live != bool(exp_value):
            raise RuntimeError(f"{tag}: flag live={flag_live}, expected {bool(exp_value)}; see {log}")
        if use_n7 and not n7_line:
            raise RuntimeError(f"{tag}: clone-n7 did not run; see {log}")
        cli = bench.Client(port)
        held = [(p, k) for p in ps for k in ("R2", "R3")]
        lat, at10, misses = [], 0, 0
        # warm-up: 20 discarded
        for p, k in held[:20]:
            cli.request("GET", "/api/memory?q=" + urllib.parse.quote(p["queries"][k]))
        per_attr: dict[str, list[int]] = {}
        for p, k in held[20:]:
            ms, hits = cli.request("GET", "/api/memory?q=" + urllib.parse.quote(p["queries"][k]))
            lat.append(ms)
            top = [h.get("id") for h in (hits or [])[:10]]
            hit = p["id"] in top
            at10 += hit
            per_attr.setdefault(p["attr"], []).append(int(hit))
    finally:
        bench.stop(proc, out)
    measured = len(held) - 20
    return {
        "n": n, "scope": scope or "default", "mode": mode, "flag_live": flag_live,
        "exp": exp_value or "(unset)", "queries": measured,
        "gold_at10": round(at10 / measured, 4),
        "gold_at10_by_attr": {a: round(sum(v) / len(v), 3) for a, v in sorted(per_attr.items())},
        "p50_ms": round(pct(lat, 50), 3), "p95_ms": round(pct(lat, 95), 3),
        "mean_ms": round(statistics.fmean(lat), 3),
        "cold_start_s": round(cold_s, 3),
        "clone_log": clone_line.split(" ", 2)[-1] if clone_line else "",
        "clone_n7_log": n7_line.split(" ", 2)[-1] if n7_line else "",
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True, type=Path)
    ap.add_argument("--work", required=True, type=Path)
    ap.add_argument("--out", required=True, type=Path)
    ap.add_argument("--ps-dir", type=Path, default=HERE.parent.parent / ".." / "lab-work" / "clone" / "ps")
    ap.add_argument("--sizes", default="1000,10000")
    ap.add_argument("--scopes", default="default,session")
    ap.add_argument("--modes", default="off,clone,clone-n7")
    ap.add_argument("--llm-file", type=Path)
    ap.add_argument("--port", type=int, default=19060)
    args = ap.parse_args()
    args.work.mkdir(parents=True, exist_ok=True)
    args.out.mkdir(parents=True, exist_ok=True)
    results = []
    for n in [int(x) for x in args.sizes.split(",")]:
        for scope in args.scopes.split(","):
            sc = "" if scope == "default" else scope
            for mode in args.modes.split(","):
                if "llm" in mode and (n != 1000 or sc):
                    continue  # LLM paraphrases exist for the 1k default corpus only
                r = run_one(args.binary, args.work, n, sc, mode, args.ps_dir,
                            args.llm_file, args.port)
                results.append(r)
                print(json.dumps({k: r[k] for k in ("n", "scope", "mode", "gold_at10", "p50_ms",
                                                     "clone_log")}), flush=True)
                (args.out / "paraphrase-results.json").write_text(json.dumps(results, indent=1) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
