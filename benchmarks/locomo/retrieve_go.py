#!/usr/bin/env python3
"""Produce LoCoMo contexts from a RUNNING server over HTTP.

run_locomo.py's retrieve phase imports server.index in-process, which can only
ever measure the Python implementation. This writes the same
`contexts.jsonl` records for a condition of your choosing by driving a live
server, so the reader and judge phases — which are implementation-agnostic and
read that file — can score any build.

The context is assembled exactly as run_locomo._grimoire_context does: top-10
semantic chunks plus top-5 text-search hits for the raw question, de-duplicated
in the same order. Anything else would be measuring a different product.

Usage:

    python retrieve_go.py --base-url http://127.0.0.1:9121 \\
        --vault /tmp/locomo-go --condition grimoire-go
"""
from __future__ import annotations

import argparse
import json
import sys
import urllib.parse
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from run_locomo import RESULTS, load_data, session_docs  # noqa: E402


def api(base: str, path: str, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + path, data=data, method="POST" if data else "GET",
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=300) as r:
        raw = r.read().decode()
    return json.loads(raw) if raw.strip() else {}


def build_vault(vault: Path, conv) -> None:
    """One note per session, matching run_locomo._build_vault: the session date
    lives in the note TITLE so every retrieval hit carries its date."""
    vault.mkdir(parents=True, exist_ok=True)
    for p in vault.glob("*.md"):
        p.unlink()
    for n, when, text in session_docs(conv):
        (vault / f"session-{n:02d}.md").write_text(
            f"---\ntitle: Conversation session {n} ({when})\n---\n"
            f"Date: {when}\n\n{text}\n", encoding="utf-8")


def context_for(base: str, question: str) -> str:
    parts, seen = [], set()
    q = urllib.parse.quote(question)
    for c in api(base, f"/api/retrieve?q={q}&k=10"):
        key = (c["path"], c["chunk"][:64])
        if key in seen:
            continue
        seen.add(key)
        parts.append(f"### {c['title']}\n{c['chunk']}")
    # limit=5&full=true mirrors run_locomo._grimoire_context, which calls
    # fts_search(q=q, limit=5, full=True) — bodies, not snippets. Getting
    # this wrong halves the context and looks like a retrieval regression.
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
    ap.add_argument("--vault", required=True,
                    help="the vault directory the server is indexing")
    ap.add_argument("--condition", default="grimoire-go")
    a = ap.parse_args()

    data = load_data()
    qs = [json.loads(x) for x in (RESULTS / "questions.jsonl").open()]
    cfile = RESULTS / "contexts.jsonl"
    # resumable, like the in-process phase: re-running must not duplicate rows
    done = {(r["qid"], r["condition"])
            for r in map(json.loads, cfile.open())} if cfile.exists() else set()

    written = 0
    with cfile.open("a") as out:
        for ci, c in enumerate(data):
            todo = [q for q in qs
                    if q["conv"] == ci and (q["qid"], a.condition) not in done]
            if not todo:
                continue
            build_vault(Path(a.vault), c["conversation"])
            indexed = api(a.base_url, "/api/reindex", {})
            for q in todo:
                out.write(json.dumps({
                    "qid": q["qid"], "condition": a.condition,
                    "context": context_for(a.base_url, q["question"])}) + "\n")
                written += 1
            out.flush()
            print(f"retrieve {a.condition} conv{ci}: {len(todo)} contexts "
                  f"({indexed.get('indexed')} notes indexed)")
    print(f"wrote {written} contexts for condition {a.condition!r}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
