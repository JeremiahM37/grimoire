"""A per-commit gate on retrieval quality.

The benchmarks in benchmarks/ answer "how good is this?" — 500 questions, an
LLM reader, a blind judge, an afternoon of tokens. They cannot run on a push,
so between benchmark rounds nothing stood between a retrieval refactor and a
silent five-point loss. The port already produced exactly that once: dropping
the search fallbacks cost 6.4 points of LoCoMo recall and was found weeks
later.

This is the cheap half of that signal. A fixed 24-note corpus, 20 questions
whose evidence note is known, scored on rank alone — no reader, no judge, no
network, a few seconds. It cannot tell you the system is good. It tells you
the system still finds what it found yesterday, which is the thing a commit
can break.

Floors are set one step below what the shipped configuration MEASURES here, so
a floor failure means retrieval changed, not that it was always marginal.
Re-measure with:

    pytest -q tests/retrieval -s          # prints the measured table
"""
from __future__ import annotations

import json
import os
import socket
import sys
import urllib.parse
import urllib.request
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
from benchmarks.goserver import launch  # noqa: E402

HERE = Path(__file__).parent
BINARY = os.environ.get("GRIMOIRE_BINARY", str(HERE.parents[1] / "go" / "grimoire"))

# Measured 2026-08-19 against the shipped configuration (hybrid retrieval,
# model2vec potion-base-8M): lexical@3 1.000, paraphrase@5 1.000,
# overall@5 1.000, MRR 0.850 — every question's evidence note found, most of
# them first. The floors sit one miss below that, so a failure means a question
# that used to be answered no longer is, rather than a fixture that was always
# marginal. The same questions score 0.800 on the lexical leg alone, which is
# what test_the_floors_can_tell_a_broken_retrieval_apart holds them to.
FLOORS = {"lexical@3": 1.00, "paraphrase@5": 0.90, "overall@5": 0.95, "mrr": 0.78}


def load(name: str) -> list[dict]:
    return [json.loads(line) for line in (HERE / name).read_text().splitlines() if line.strip()]


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def get(base: str, path: str, **params) -> dict:
    url = f"{base}{path}?{urllib.parse.urlencode(params)}"
    with urllib.request.urlopen(url, timeout=30) as response:
        return json.loads(response.read())


@pytest.fixture(scope="module")
def server(tmp_path_factory):
    vault = tmp_path_factory.mktemp("gate-vault")
    for note in load("corpus.jsonl"):
        path = vault / note["path"]
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(note["body"])
    if not Path(BINARY).exists():
        pytest.skip(f"{BINARY} not built")
    with launch(BINARY, vault, free_port(), embed="auto") as base:
        yield base


def test_the_gate_runs_the_shipped_embedder(server):
    """The chain falls back silently when a model is missing, and the hashing
    floor would fail every semantic question for a reason that has nothing to
    do with the commit under test. Say so instead of failing obscurely."""
    health = get(server, "/api/health")
    if health.get("embedder", "").startswith("hash:"):
        pytest.skip("semantic embedder unavailable — model2vec did not load; "
                    f"health reports {health.get('embedder')!r}")
    assert health["notes"] == len(load("corpus.jsonl"))


def test_retrieval_still_finds_the_evidence(server, capsys):
    if get(server, "/api/health").get("embedder", "").startswith("hash:"):
        pytest.skip("semantic embedder unavailable")

    rows, ranks = [], []
    for question in load("questions.jsonl"):
        hits = get(server, "/api/retrieve", q=question["q"], k=5)
        paths = [h["path"] for h in hits]
        rank = paths.index(question["expect"]) + 1 if question["expect"] in paths else 0
        rows.append({**question, "rank": rank, "got": paths[:3]})
        ranks.append(rank)

    def rate(kind: str, cutoff: int) -> float:
        subset = [r for r in rows if kind == "any" or r["kind"] == kind]
        return sum(1 for r in subset if 0 < r["rank"] <= cutoff) / len(subset)

    measured = {
        "lexical@3": rate("lexical", 3),
        "paraphrase@5": rate("paraphrase", 5),
        "overall@5": rate("any", 5),
        "mrr": sum(1 / r for r in ranks if r) / len(ranks),
    }
    with capsys.disabled():
        print("\n                 measured   floor")
        for name, value in measured.items():
            print(f"  {name:<14} {value:8.3f}   {FLOORS[name]:.2f}")
        for row in rows:
            if not 0 < row["rank"] <= 5:
                print(f"  MISS  {row['q']!r} -> {row['got']}, wanted {row['expect']}")

    failures = [f"{name}: {measured[name]:.3f} < {floor:.2f}"
                for name, floor in FLOORS.items() if measured[name] < floor]
    assert not failures, (
        "retrieval regressed against the recorded floors: " + "; ".join(failures)
        + "\nIf the change is deliberate and the new numbers are better on the "
          "real benchmarks, re-record FLOORS in this file with the reason.")


def test_the_floors_can_tell_a_broken_retrieval_apart(server):
    """A gate that everything passes is not a gate.

    The single most costly retrieval regression this project has had was the
    loss of a search leg, so the floors are checked against exactly that: the
    lexical leg alone, over the same questions. If half the retrieval still
    clears the floors, they are set too low to catch losing the other half."""
    if get(server, "/api/health").get("embedder", "").startswith("hash:"):
        pytest.skip("semantic embedder unavailable")

    found = 0
    for question in load("questions.jsonl"):
        hits = get(server, "/api/search", q=question["q"], limit=5)
        if question["expect"] in [h["path"] for h in hits][:5]:
            found += 1
    lexical_only = found / len(load("questions.jsonl"))
    assert lexical_only < FLOORS["overall@5"], (
        f"lexical-only scores {lexical_only:.3f}, at or above the overall@5 floor "
        f"of {FLOORS['overall@5']:.2f} — the floors would not notice the semantic "
        "leg disappearing")
