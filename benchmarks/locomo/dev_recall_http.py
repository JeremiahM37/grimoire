#!/usr/bin/env python3
"""Evidence-turn recall over HTTP, so the SAME measurement runs against either
implementation.

The original dev_recall.py retrieved in-process, which tied it to one
implementation and is gone with it. This
drives a running server through its HTTP API instead, so the Go build can be
measured with the identical corpus, questions and scoring.

Recall here means: of the evidence turns a question depends on, how many appear
in the context retrieval hands the reader? It uses NO LLM calls, so it is cheap,
deterministic and free of judge variance — which is exactly what you want when
comparing two implementations rather than two models.

Usage — point it at a server and the vault directory that server is indexing:

    python benchmarks/locomo/dev_recall_http.py \
        --base-url http://127.0.0.1:9121 --vault /tmp/bench-vault --convs 2
"""
from __future__ import annotations

import argparse
import ast
import json
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from run_locomo import DATA, load_data, session_docs  # noqa: E402

CATS = {1: "multi-hop", 2: "temporal", 3: "open-domain", 4: "single-hop"}


def api(base: str, path: str, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        base + path, data=data, method="POST" if data else "GET",
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=300) as r:
        raw = r.read().decode()
    return json.loads(raw) if raw.strip() else {}


def build_vault(vault: Path, conv) -> int:
    """Write one note per conversation session, exactly as the in-process
    harness does — the session date lives in the TITLE so every retrieval hit
    carries its date."""
    for p in vault.glob("*.md"):
        p.unlink()
    n_written = 0
    for n, when, text in session_docs(conv):
        body = (f"---\ntitle: Conversation session {n} ({when})\n---\n"
                f"Date: {when}\n\n{text}\n")
        (vault / f"session-{n:02d}.md").write_text(body, encoding="utf-8")
        n_written += 1
    return n_written


def context_for(base: str, question: str) -> str:
    """Context exactly as the product retrieves it: top-10 semantic chunks plus
    top-5 text-search hits for the raw question."""
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


def turn_texts(conv) -> dict[str, str]:
    out = {}
    for v in conv.values():
        if isinstance(v, list):
            for t in v:
                txt = t.get("text", "")
                if t.get("blip_caption"):
                    txt += f" [shared a photo: {t['blip_caption']}]"
                out[t["dia_id"]] = txt.strip()
    return out


def norm(s: str) -> str:
    return re.sub(r"\s+", " ", s).strip().lower()


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--vault", required=True,
                    help="the vault directory the server is indexing")
    ap.add_argument("--convs", type=int, default=2,
                    help="how many conversations to measure")
    ap.add_argument("--max-questions", type=int, default=60)
    ap.add_argument("--out", default="")
    a = ap.parse_args()

    if not DATA.exists():
        print(f"LoCoMo data not found at {DATA}; run: python run_locomo.py sample")
        return 2
    vault = Path(a.vault)
    vault.mkdir(parents=True, exist_ok=True)

    data = load_data()
    hit = total = 0
    by_cat: dict[str, list[int]] = {}
    rows = []

    for ci, c in enumerate(data[: a.convs]):
        conv = c["conversation"]
        n_notes = build_vault(vault, conv)
        indexed = api(a.base_url, "/api/reindex", {})
        print(f"conversation {ci}: {n_notes} session notes, "
              f"server indexed {indexed.get('indexed')}")
        texts = turn_texts(conv)

        asked = 0
        for qi, q in enumerate(c["qa"]):
            if int(q.get("category", 0)) == 5 or asked >= a.max_questions:
                continue
            try:
                ev = ast.literal_eval(str(q.get("evidence", "[]")))
            except (ValueError, SyntaxError):
                ev = []
            if not ev:
                continue
            asked += 1
            ctx = norm(context_for(a.base_url, q["question"]))
            cat = CATS.get(int(q["category"]), "other")
            for dia in ev:
                t = norm(texts.get(dia, ""))
                if not t:
                    continue
                total += 1
                found = 1 if t[:120] in ctx else 0
                hit += found
                by_cat.setdefault(cat, []).append(found)
            rows.append({"qid": f"c{ci}q{qi}", "category": cat,
                         "question": q["question"]})

    if not total:
        print("no evidence turns measured")
        return 2
    print(f"\nevidence-turn recall: {hit}/{total} = {hit / total:.1%}")
    for cat in sorted(by_cat):
        v = by_cat[cat]
        print(f"  {cat:<12} {sum(v)}/{len(v)} = {sum(v) / len(v):.1%}")
    if a.out:
        Path(a.out).write_text(json.dumps(
            {"recall": hit / total, "hit": hit, "total": total,
             "by_category": {k: [sum(v), len(v)] for k, v in by_cat.items()},
             "base_url": a.base_url}, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
