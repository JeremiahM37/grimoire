"""Live-model-call budget shared by the evalkit harnesses.

Every live call is written to a ledger BEFORE it is made, so a call that
fails or times out still counts. The ledger is append-only JSONL and is the
single record of how many live calls the evalkit has spent. Two caps apply:

    run_cap    per harness invocation (default 60)
    total_cap  across every run that shares the ledger (default 150)

Both are checked before each call and the call is refused if either would be
exceeded. There are no retries here: a failed call is spent and reported.

Live calls are Haiku only, through `claude -p --model haiku` with no tools and
a one-line system prompt (about 0.5k tokens each). They are reachable only from
an explicit `live` run, so every dry run and every test is offline.
"""
from __future__ import annotations

import json
import os
import subprocess
import tempfile
import threading
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
LEDGER = Path(os.environ.get("EVALKIT_LEDGER", HERE / "evalkit-ledger.jsonl"))
TOTAL_CAP = 150
RUN_CAP = 60
MODEL = "haiku"
_LOCK = threading.Lock()


class BudgetExceeded(RuntimeError):
    """Raised before a call that would exceed a cap. The call is not made."""


def spent(ledger: Path | None = None) -> int:
    p = Path(ledger or LEDGER)
    if not p.exists():
        return 0
    return sum(1 for line in p.read_text().splitlines() if line.strip())


class Budget:
    def __init__(self, harness: str, run_cap: int = RUN_CAP,
                 total_cap: int = TOTAL_CAP, ledger: Path | None = None):
        self.harness = harness
        self.run_cap = run_cap
        self.total_cap = total_cap
        self.ledger = Path(ledger or LEDGER)
        self.run_used = 0

    def spend(self, label: str) -> None:
        """Reserve one call. Raises BudgetExceeded instead of making it."""
        with _LOCK:
            total = spent(self.ledger)
            if self.run_used + 1 > self.run_cap:
                raise BudgetExceeded(f"{self.harness}: run cap {self.run_cap} reached")
            if total + 1 > self.total_cap:
                raise BudgetExceeded(f"total cap {self.total_cap} reached ({total} spent)")
            self.ledger.parent.mkdir(parents=True, exist_ok=True)
            rec = {"ts": time.strftime("%Y-%m-%dT%H:%M:%S"), "harness": self.harness,
                   "label": label, "model": MODEL}
            with self.ledger.open("a") as f:
                f.write(json.dumps(rec) + "\n")
            self.run_used += 1


class Live:
    """A Haiku completion behind the budget. The only path to a live model."""

    def __init__(self, budget: Budget, system: str, timeout: int = 240):
        self.budget = budget
        self.system = system
        self.timeout = timeout

    def complete(self, prompt: str, label: str = "") -> str:
        self.budget.spend(label)
        empty = Path(tempfile.gettempdir()) / "evalkit-empty-cwd"
        empty.mkdir(exist_ok=True)
        p = subprocess.run(
            ["claude", "-p", "--model", MODEL, "--output-format", "json",
             "--strict-mcp-config", "--max-turns", "1",
             "--tools", "", "--system-prompt", self.system],
            input=prompt, capture_output=True, text=True, timeout=self.timeout,
            cwd=empty)
        if p.returncode != 0:
            raise RuntimeError(f"claude exit {p.returncode}: {p.stderr[:300]}")
        return (json.loads(p.stdout).get("result") or "").strip()
