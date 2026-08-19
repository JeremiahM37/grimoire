#!/usr/bin/env python3
"""Standard retrieval baselines under the identical protocol.

Grimoire's own accuracy number means little on its own: this study's judge is
not LongMemEval's official one, so the figure cannot be read against the
paper's leaderboard. What CAN be read is a comparison in which every condition
shares the questions, the ingestion, the reader, the judge and the context
budget, and only the retrieval method differs. That is what this produces.

    lexical-only   BM25 over whole notes (SQLite FTS5), query-relevant excerpt
    dense-only     embedding cosine over chunks, top-k
    hybrid         both, fused — what Grimoire ships

The first two are the reference points a reader already has intuitions about;
the third is the claim. Budgets are matched by CHARACTERS rather than by k or
limit, because a comparison at equal k is a comparison of budgets, not methods.

    python retrieve_baselines.py --binary ../../go/grimoire --vault /tmp/b \\
        --condition lexical-only --leg lexical --budget 22000
"""
from __future__ import annotations

import argparse
import json
import sys
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
sys.path.insert(0, str(HERE.parent))

import goserver  # noqa: E402
from retrieve_go import build_vault  # noqa: E402  (identical ingestion)
from run_locomo import RESULTS, load_data  # noqa: E402


def api(base: str, path: str, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + path, data=data, method="POST" if data else "GET",
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=600) as r:
        raw = r.read().decode()
    return json.loads(raw) if raw.strip() else {}


def context_for(base: str, question: str, leg: str, budget: int) -> str:
    """Assemble a context from one leg (or both), stopping at a character
    budget so every condition costs the reader the same."""
    q = urllib.parse.quote(question)
    parts, seen, used = [], set(), 0
    # Each leg gets its own share of the budget. Filling first-come-first-served
    # silently turns "hybrid" into whichever leg is asked first: the dense leg
    # returns multi-chunk excerpts, so it exhausted a 24k budget on its own and
    # the lexical leg contributed nothing at all — the condition scored
    # identically to dense-only, down to the token count.
    share = budget if leg != "hybrid" else budget // 2
    cap = share

    def add(title: str, text: str, path: str) -> bool:
        nonlocal used
        key = (path, text[:64])
        if key in seen or not text:
            return True
        seen.add(key)
        if used + len(text) > cap:
            return False
        parts.append(f"### {title}\n{text}")
        used += len(text)
        return True

    if leg in ("dense", "hybrid"):
        # ask for more than the budget can hold; the budget does the cutting
        for c in api(base, f"/api/retrieve?q={q}&k=60"):
            if not add(c["title"], c["chunk"], c["path"]):
                break
    if leg == "hybrid":
        cap = budget  # the lexical leg may use whatever the dense leg left
    if leg in ("lexical", "hybrid"):
        for h in api(base, f"/api/search?q={q}&limit=30&full=true"):
            text = h.get("body") or h.get("snippet") or ""
            if not add(h["title"] + " (text search)", text, h["path"]):
                break
    return "\n\n".join(parts)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True)
    ap.add_argument("--vault", required=True)
    ap.add_argument("--port", type=int, default=9481)
    ap.add_argument("--embed", default="auto", choices=("off", "auto", "ollama"))
    ap.add_argument("--condition", required=True)
    ap.add_argument("--leg", required=True, choices=("lexical", "dense", "hybrid"))
    ap.add_argument("--budget", type=int, default=22000,
                    help="context character budget, matched across conditions")
    a = ap.parse_args()

    data = load_data()
    qs = [json.loads(x) for x in (RESULTS / "questions.jsonl").open()]
    cfile = RESULTS / "contexts.jsonl"
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, cfile.open())} if cfile.exists() else set()
    todo = [q for q in qs if (q["qid"], a.condition) not in done]
    print(f"{len(todo)} questions for {a.condition!r} (leg={a.leg}, budget={a.budget})")

    written = 0
    with goserver.launch(a.binary, a.vault, a.port, a.embed) as base:
        with cfile.open("a") as out:
            for ci, c in enumerate(data):
                batch = [q for q in todo if q["conv"] == ci]
                if not batch:
                    continue
                build_vault(Path(a.vault), c["conversation"])
                api(base, "/api/reindex", {})
                for q in batch:
                    out.write(json.dumps({
                        "qid": q["qid"], "condition": a.condition,
                        "context": context_for(base, q["question"], a.leg, a.budget)}) + "\n")
                    written += 1
                out.flush()
                print(f"  {a.condition} conv{ci}: {len(batch)} ({written}/{len(todo)})")
    print(f"wrote {written} contexts for {a.condition!r}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
