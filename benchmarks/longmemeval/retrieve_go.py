#!/usr/bin/env python3
"""Produce LongMemEval contexts from a RUNNING server over HTTP.

Same idea as benchmarks/locomo/retrieve_go.py: run_lme.py's retrieve phase
builds the vault in-process, which can only measure the Python implementation.
This drives a live server instead, so the reader and judge phases — which read
contexts.jsonl and do not care what produced it — can score any build.

Each question gets its own haystack of ~50 chat sessions, so the vault is
rebuilt per question. That is inherent to the benchmark, not an inefficiency
here: the point is that the answer sits in one session among fifty.

Usage:

    python retrieve_go.py --base-url http://127.0.0.1:9121 \\
        --vault /tmp/lme-go --condition grimoire-go
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

from run_lme import RESULTS, _session_note, load_data  # noqa: E402


def api(base: str, path: str, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + path, data=data, method="POST" if data else "GET",
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=600) as r:
        raw = r.read().decode()
    return json.loads(raw) if raw.strip() else {}


def build_vault(vault: Path, inst) -> int:
    """One note per haystack session, matching run_lme._build_vault: the session
    date lives in the note TITLE so every retrieval hit carries its date."""
    vault.mkdir(parents=True, exist_ok=True)
    for p in vault.glob("*.md"):
        p.unlink()
    sessions = inst["haystack_sessions"]
    dates = inst["haystack_dates"]
    for i, (sess, when) in enumerate(zip(sessions, dates)):
        (vault / f"session-{i:03d}.md").write_text(
            f"---\ntitle: Chat session {i + 1} ({when})\n---\n"
            f"Date: {when}\n\n{_session_note(sess)}\n", encoding="utf-8")
    return len(sessions)


def context_for(base: str, question: str) -> str:
    """Top-10 semantic chunks plus top-5 text-search bodies, exactly as
    run_locomo._grimoire_context assembles them."""
    parts, seen = [], set()
    q = urllib.parse.quote(question)
    for c in api(base, f"/api/retrieve?q={q}&k=10"):
        key = (c["path"], c["chunk"][:64])
        if key in seen:
            continue
        seen.add(key)
        parts.append(f"### {c['title']}\n{c['chunk']}")
    for h in api(base, f"/api/search?q={q}&limit=5&full=true"):
        text = h.get("body") or h.get("snippet") or ""
        key = (h["path"], text[:64])
        if key in seen:
            continue
        seen.add(key)
        parts.append(f"### {h['title']} (text search)\n{text}")
    return "\n\n".join(parts)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--vault", required=True)
    ap.add_argument("--condition", default="grimoire-go")
    ap.add_argument("--limit", type=int, default=0)
    a = ap.parse_args()

    data = load_data()
    qs = [json.loads(x) for x in (RESULTS / "questions.jsonl").open()]
    cfile = RESULTS / "contexts.jsonl"
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, cfile.open())} if cfile.exists() else set()

    todo = [q for q in qs if (q["qid"], a.condition) not in done]
    if a.limit:
        todo = todo[: a.limit]
    print(f"{len(todo)} questions to retrieve for {a.condition!r}")

    written = 0
    with cfile.open("a") as out:
        for q in todo:
            n = build_vault(Path(a.vault), data[q["idx"]])
            api(a.base_url, "/api/reindex", {})
            out.write(json.dumps({
                "qid": q["qid"], "condition": a.condition,
                "context": context_for(a.base_url, q["question"])}) + "\n")
            out.flush()
            written += 1
            if written % 25 == 0:
                print(f"  {written}/{len(todo)} (last haystack: {n} sessions)")
    print(f"wrote {written} contexts for condition {a.condition!r}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
