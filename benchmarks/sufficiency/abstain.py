#!/usr/bin/env python3
"""Does the reader-side verdict separate answerable from unanswerable?

The retrieval-side probe (probe.py) found that no ranking statistic can: every
signal was flat or inverted. This measures the alternative that costs nothing
extra — a grounded/ungrounded verdict emitted in the SAME completion as the
answer — against two baselines on the same questions:

  retrieval threshold   the field's standard "is the top score high enough";
                        measured by probe.py, AUC 0.439 (worse than chance)
  prose abstention      what a caller had before this change: string-matching
                        the free-text answer for "the notes don't say", which
                        is what the old prompt asked for
  verdict               the parsed SUPPORTED: line

Reader is the product's default local model over Ollama, through /api/ask —
the shipped path, not a bespoke prompt.
"""
from __future__ import annotations

import argparse
import json
import re
import sys
import time
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE.parent))
sys.path.insert(0, str(HERE.parent / "locomo"))

import goserver  # noqa: E402
from run_locomo import load_data, session_docs  # noqa: E402

# What a caller had to look for before there was a verdict. Deliberately
# generous: any of these counts as an abstention, so the baseline is measured
# at its best rather than at its most convenient.
PROSE_ABSTAIN = re.compile(
    r"(don'?t|do not|does not|doesn'?t|cannot|can'?t|no|not) "
    r"(contain|say|state|mention|include|specify|have|provide|indicate)"
    r"|no (information|mention|indication|record|details?)"
    r"|not (stated|mentioned|specified|provided|available|found|contained)"
    r"|(unable|insufficient) to",
    re.I)


def post(base: str, path: str, body: dict, timeout=300):
    req = urllib.request.Request(
        base + path, data=json.dumps(body).encode(), method="POST",
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode() or "{}")


def build_vault(vault: Path, conv) -> None:
    vault.mkdir(parents=True, exist_ok=True)
    for p in vault.glob("*.md"):
        p.unlink()
    for n, when, text in session_docs(conv["conversation"]):
        (vault / f"session-{n:02d}.md").write_text(
            f"---\ntitle: Conversation session {n} ({when})\n---\n"
            f"Date: {when}\n\n{text}\n", encoding="utf-8")


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", default=str(HERE.parent.parent / "go" / "grimoire"))
    ap.add_argument("--port", type=int, default=9133)
    ap.add_argument("--vault", default="/tmp/abstain-vault")
    ap.add_argument("--per-conv", type=int, default=5, help="questions per class per conversation")
    ap.add_argument("--out", default=str(HERE / "abstain.jsonl"))
    args = ap.parse_args()

    data = load_data()
    out = Path(args.out).open("w")
    n = 0
    failures: list[str] = []
    t0 = time.monotonic()
    for ci, conv in enumerate(data):
        qa = conv.get("qa", [])
        answerable = [q for q in qa if int(q.get("category", 0)) in (1, 2, 3, 4)][:args.per_conv]
        unanswerable = [q for q in qa if int(q.get("category", 0)) == 5][:args.per_conv]
        if not unanswerable:
            continue
        vault = Path(args.vault)
        build_vault(vault, conv)
        # A port per conversation. Reusing one meant a launch that lost the
        # bind race sent its questions to the PREVIOUS server, which still had
        # rate limiting on — the run then measured HTTP 429 instead of the
        # reader, and did it fast enough to look like it had worked.
        port = args.port + ci
        base = f"http://127.0.0.1:{port}"
        # The server rate-limits AI endpoints to 2/s with a burst of 30, which
        # is right for a self-hosted server answering a person and wrong for a
        # benchmark asking a hundred questions in a row. Turning it off is a
        # documented switch, not a workaround: without it the run silently
        # measures HTTP 429s instead of the reader.
        with goserver.launch(args.binary, vault, port, embed="ollama",
                             env={"GRIMOIRE_RATE_LIMIT": "off"}):
            for label, qs in (("answerable", answerable), ("unanswerable", unanswerable)):
                for qi, q in enumerate(qs):
                    res = None
                    try:
                        for attempt in range(3):
                            try:
                                res = post(base, "/api/ask",
                                           {"q": q["question"], "k": 6})
                                break
                            except Exception:  # noqa: BLE001, PERF203
                                # The reader is a local model behind an HTTP
                                # service; a transient 500 is its load, not a
                                # verdict. Retry, then fail loudly.
                                if attempt == 2:
                                    raise
                                time.sleep(3)
                    except Exception as exc:  # noqa: BLE001
                        # Loud, and fatal: a run that quietly drops questions
                        # reports a rate on a sample it chose by failing.
                        print(f"  c{ci} {label} {qi}: {exc}", flush=True)
                        failures.append(f"c{ci}{label[:3]}{qi}: {exc}")
                        continue
                    answer = res.get("answer", "")
                    rec = {
                        "qid": f"c{ci}{label[:3]}{qi}", "conv": ci, "label": label,
                        "question": q["question"],
                        "supported": res.get("supported", "unknown"),
                        "prose_abstained": bool(PROSE_ABSTAIN.search(answer)),
                        "answer": answer[:400],
                    }
                    out.write(json.dumps(rec) + "\n")
                    out.flush()
                    n += 1
        print(f"conversation {ci}: {n} answered ({time.monotonic()-t0:.0f}s)", flush=True)
    out.close()
    print(f"wrote {n} records to {args.out}")
    if failures:
        print(f"FAILED {len(failures)} questions — the sample is not the one "
              f"that was asked for:\n  " + "\n  ".join(failures[:10]))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
