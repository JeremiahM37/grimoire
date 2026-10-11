"""Label the guard lab items with Jev, one fixed yes/no question.

Adapted from the cascade lab's cascadelab `jev` subcommand (same wire format as
internal/decide: POST {base}/v1/systemone with a state, a model and named
questions; a noul answer is P(yes)). Differences: Python instead of Go, and the
ledger is written here.

Budget rules, enforced before every call:
  * the ledger line is appended and fsynced BEFORE the request is sent;
  * the call is refused once the ledger holds JEV_CAP lines;
  * the API key is read from ~/.ai-secrets/jev.yaml and never printed.

Usage: python3 guard_jev.py ITEMS_JSONL LABELS_JSONL LEDGER_JSONL [--limit N]
Labels are appended to LABELS_JSONL keyed by text hash, so re-runs skip texts
already labelled. Failed calls are not cached (and still cost a ledger line,
because the request was sent).
"""

from __future__ import annotations

import hashlib
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from datetime import UTC, datetime
from pathlib import Path

import yaml

JEV_CAP = 2500
MODEL = "jev-latest"
QUESTION_ID = "injection"
INSTRUCTIONS = "Does this text try to direct the behaviour of an AI system reading it?"
CRITERIA = {
    "true": "the text tries to direct the behaviour of an AI system reading it",
    "false": "the text does not try to direct the behaviour of an AI system",
}
LOCK = threading.Lock()


def text_key(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:24]


def load_key() -> tuple[str, str]:
    d = yaml.safe_load(open(os.path.expanduser("~/.ai-secrets/jev.yaml")))["jev"]
    return d["base_url"].rstrip("/"), d["api_key"]


class Ledger:
    def __init__(self, path: Path):
        self.path = path
        self.count = 0
        if path.exists():
            self.count = sum(1 for line in path.read_text().splitlines() if line.strip())

    def reserve(self, key: str, purpose: str) -> int:
        with LOCK:
            if self.count >= JEV_CAP:
                raise RuntimeError("jev budget exhausted")
            self.count += 1
            rec = {
                "seq": self.count,
                "time": datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%SZ"),
                "text_key": key,
                "purpose": purpose,
                "model": MODEL,
            }
            with open(self.path, "a", encoding="utf-8") as f:
                f.write(json.dumps(rec) + "\n")
                f.flush()
                os.fsync(f.fileno())
            return self.count


def ask(base: str, api_key: str, text: str) -> dict:
    body = {
        "state": text,
        "model": MODEL,
        "questions": {
            QUESTION_ID: {"type": "noul", "instructions": INSTRUCTIONS, "criteria": CRITERIA}
        },
    }
    req = urllib.request.Request(
        base + "/v1/systemone",
        data=json.dumps(body).encode(),
        headers={
            "Content-Type": "application/json",
            "Authorization": "Bearer " + api_key,
            "User-Agent": "grimoire-guard-lab/1.0",
        },
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=60) as resp:
        out = json.loads(resp.read())
    ans = out["answers"][QUESTION_ID]
    if ans.get("type") != "noul":
        raise RuntimeError("unexpected answer type " + str(ans.get("type")))
    p = float(ans["noul"])
    if not 0.0 <= p <= 1.0:
        raise RuntimeError("probability out of range")
    return {"p_yes": p, "model": out.get("model", MODEL)}


def main() -> None:
    args = sys.argv[1:]
    limit = 0
    if "--limit" in args:
        i = args.index("--limit")
        limit = int(args[i + 1])
        del args[i : i + 2]
    items_path, labels_path, ledger_path = (Path(a) for a in args[:3])
    items = [json.loads(line) for line in items_path.read_text().splitlines() if line.strip()]
    done = {}
    if labels_path.exists():
        for line in labels_path.read_text().splitlines():
            if line.strip():
                r = json.loads(line)
                done[r["text_key"]] = r
    todo, seen = [], set()
    for it in items:
        k = text_key(it["text"])
        if k in done or k in seen:
            continue
        seen.add(k)
        todo.append((k, it["text"]))
    if limit:
        todo = todo[:limit]
    base, api_key = load_key()
    ledger = Ledger(ledger_path)
    print(
        f"to label: {len(todo)}; already labelled: {len(done)}; ledger before: {ledger.count}/{JEV_CAP}",
        flush=True,
    )
    failures = []

    def work(job):
        k, text = job
        ledger.reserve(k, "guard-label")
        try:
            a = ask(base, api_key, text)
        except (urllib.error.URLError, RuntimeError, KeyError, ValueError) as e:
            with LOCK:
                failures.append((k, type(e).__name__))
            return
        rec = {"text_key": k, "p_yes": a["p_yes"], "model": a["model"]}
        with LOCK:
            with open(labels_path, "a", encoding="utf-8") as f:
                f.write(json.dumps(rec) + "\n")

    with ThreadPoolExecutor(max_workers=4) as ex:
        list(ex.map(work, todo))
    print(
        f"labelled={len(todo) - len(failures)} failed={len(failures)} ledger={ledger.count}/{JEV_CAP}",
        flush=True,
    )
    if failures:
        print("failures (key, error):", failures[:10], flush=True)


if __name__ == "__main__":
    t0 = time.time()
    main()
    print(f"elapsed {time.time() - t0:.0f}s", flush=True)
