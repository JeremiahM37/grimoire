"""Offline paraphrases for the clone experiment from the LOCAL model.

Asks qwen3.5:4b (Ollama at 100.127.85.58, think: false) for two alternative
questions per fact. The prompt carries only the fact text: the paraphrase
queries R1-R3 are never shown to the model. The output is a JSON map
{fact id: [paraphrase, ...]} that the server reads through GRIMOIRE_CLONE_LLM_FILE;
the server applies the grounding check (go/internal/index/memory_clone.go),
so a paraphrase that introduces a name or number is refused there, not here.

Budget: at most CAP generations in total, across all runs, counted in the
ledger file. Every call appends one line to the ledger before it is made, so
an interrupted run still counts what it spent.

Usage:
  python3 llm_paraphrase.py --ps PS.json --out OUT.json --ledger LEDGER.jsonl [--limit N]
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import sys
import urllib.request
from pathlib import Path

ENDPOINT = "http://100.127.85.58:11434/api/chat"
MODEL = "qwen3.5:4b"
CAP = 600
SCRIPT = "llm_paraphrase"

PROMPT = (
    "Here is one fact from a personal memory store:\n"
    "{fact}\n\n"
    "Write two different questions a person might ask later to recall this fact. "
    "Use only the names and numbers that appear in the fact. Do not add any other "
    "name or number. Output exactly two lines, one question per line, with no "
    "numbering and no other text."
)


def ledger_count(path: Path) -> int:
    if not path.exists():
        return 0
    return sum(1 for line in path.read_text().splitlines()
               if line.strip() and json.loads(line).get("script") == SCRIPT)


def ask(fact: str) -> str:
    body = {
        "model": MODEL,
        "messages": [{"role": "user", "content": PROMPT.format(fact=fact)}],
        "stream": False,
        "think": False,
        "options": {"temperature": 0, "num_predict": 120, "seed": 7},
    }
    req = urllib.request.Request(ENDPOINT, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.loads(r.read())["message"]["content"]


def parse(text: str) -> list[str]:
    out = []
    for line in text.splitlines():
        line = re.sub(r"^\s*(?:[-*]|\d+[.)])\s*", "", line).strip()
        if line and line.endswith("?"):
            out.append(line)
    return out[:2]


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--ps", type=Path, required=True)
    ap.add_argument("--out", type=Path, required=True)
    ap.add_argument("--ledger", type=Path, required=True)
    ap.add_argument("--limit", type=int, default=10**9)
    args = ap.parse_args()
    facts = json.loads(args.ps.read_text())[: args.limit]
    done = json.loads(args.out.read_text()) if args.out.exists() else {}
    args.ledger.parent.mkdir(parents=True, exist_ok=True)
    for f in facts:
        if f["id"] in done:
            continue
        spent = ledger_count(args.ledger)
        if spent >= CAP:
            print(f"cap reached: {spent} generations", file=sys.stderr)
            break
        with args.ledger.open("a") as led:
            led.write(json.dumps({"ts": dt.datetime.now(dt.UTC).isoformat(), "script": SCRIPT,
                                  "model": MODEL, "fact_id": f["id"], "n": spent + 1}) + "\n")
        try:
            paras = parse(ask(f["text"]))
        except Exception as e:  # a failed call still counted
            print(f"{f['id']}: {e}", file=sys.stderr)
            paras = []
        done[f["id"]] = paras
        args.out.write_text(json.dumps(done, indent=1) + "\n")
    print(f"{len(done)} facts with paraphrases; ledger generations: {ledger_count(args.ledger)}/{CAP}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
