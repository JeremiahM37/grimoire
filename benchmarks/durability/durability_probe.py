#!/usr/bin/env python3
"""Does a person's correction survive the agent's next write? — reader-free.

    python3 durability_probe.py --binary /tmp/grimoire-dur

Every published agent-memory benchmark asks whether the CURRENT value of a fact
is used. None asks whether a correction a person made by hand still EXISTS after
the agent writes again, because in every other architecture a person cannot make
one — you cannot hand-edit a vector store.

Grimoire's memory is markdown, so the question is askable, and the answer used
to be no. This measures it.

The shape, per pair, in a fresh topic:

    1. the agent writes the EARLIER value            (v1)
    2. a person corrects it to the LATER value       (v2)
    3. the agent meets the earlier evidence again and writes v1 a second time
    4. read the file and see what happened to the person's line

Four outcomes, and only one of them is acceptable:

    survived      v2 is still live and unstruck; v1 is recorded alongside
    resurrected   v2 is struck through — the correction was destroyed
    inverted      v2 carries `sup=`, so the history names the PERSON as the
                  party who was corrected. A strict subset of resurrected,
                  counted separately because losing a value and libelling the
                  person who supplied it are different harms
    lost          v2 is not in the file at all

There is no reader, no judge and no model in the measurement: it reads the
markdown. The same input gives the same answer forever, so a difference between
arms is a real difference and not a sample.

Corrections come two ways and both are measured, because they exercise different
machinery:

    handedit   the bullet's text is rewritten in the file, which is what a
               person actually does. Nothing declares it; the engine has to
               notice that the id no longer hashes to its own content
    declared   the correction is written through the API with human=true

Arms are selected with --authority; `off` restores recency-only supersession and
is the control, i.e. the behaviour before any of this existed.
"""
from __future__ import annotations

import argparse
import json
import re
import shutil
import sys
import time
import urllib.parse
import urllib.request
from collections import Counter
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from goserver import launch  # noqa: E402

DATA = Path("/tmp/lme_dl/longmemeval_s.json")
PORT = 9171

BULLET = re.compile(r"^-\s+(~~)?\*\*(.+?)\*\*\s+—\s+(.*)$")
TRAILER = re.compile(r"<!--m\s+([^>]*?)\s*-->\s*$")


def post(base: str, path: str, body: dict) -> dict:
    req = urllib.request.Request(
        base + path, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=120) as r:
        return json.loads(r.read().decode() or "{}")


def evidence_pairs(entry: dict) -> list[tuple[str, str]]:
    """The user turns the dataset marks as carrying the answer, in date order.

    Lifted from longmemeval/slot_probe.py so the two probes measure the same
    population: that one asks whether an update is RECOGNISED, this one asks
    whether a correction SURVIVES, and comparing them is only meaningful if the
    pairs are the same pairs.
    """
    ids = entry.get("haystack_session_ids") or []
    dates = entry.get("haystack_dates") or []
    marked = []
    for sid in entry.get("answer_session_ids") or []:
        if sid not in ids:
            continue
        i = ids.index(sid)
        for t in entry["haystack_sessions"][i]:
            if t.get("has_answer") and (t.get("role") or "") == "user":
                marked.append((dates[i] if i < len(dates) else "", t["content"].strip()))
    marked.sort(key=lambda p: p[0])
    if len(marked) < 2:
        return []
    return [(marked[i][1], marked[i + 1][1]) for i in range(len(marked) - 1)]


_topic = 0


def next_topic() -> str:
    global _topic
    _topic += 1
    return f"dur-{_topic:04d}"


def recognised_pair(base: str, prev: str, nxt: str) -> tuple[str, str] | None:
    """The exact two fact texts the engine treats as one fact changing value.

    This is the selection step, and it is why the first cut of this probe
    measured nothing. Taking the first fact extracted from each raw turn gives
    two facts that are usually about different things, so the agent's second
    write contradicted nothing, no supersession was attempted, and BOTH arms
    scored 100% survived — a result that looked like a pass and was an empty
    denominator.

    A pair is usable only if the engine, shown the two statements in order,
    reports UPDATE. That is the same recognition slot_probe measures at roughly
    a quarter of pairs, so most of the dataset drops out here — and it must,
    because durability is only meaningful where an overwrite would otherwise
    happen. The pairs that drop out are reported rather than hidden.
    """
    topic = next_topic()
    post(base, "/api/memory", {"topic": topic, "agent": "probe",
                               "scope": "topic", "text": prev[:19000]})
    out = post(base, "/api/memory", {"topic": topic, "agent": "probe",
                                     "scope": "topic", "text": nxt[:19000]})
    for r in out.get("results") or []:
        if r.get("op") == "UPDATE" and r.get("target"):
            why = r.get("why") or ""
            # `why` is "supersedes: <the old text>" — the old fact verbatim,
            # which is what step 1 must write for step 3 to contradict it.
            old = why.split(": ", 1)[1].strip() if ": " in why else ""
            if old and r.get("text"):
                return old, r["text"].strip()
    return None


def note_path(vault: Path, topic: str) -> Path:
    return vault / "memory" / f"{topic}.md"


def bullets(path: Path) -> list[dict]:
    if not path.exists():
        return []
    out = []
    for line in path.read_text().splitlines():
        m = BULLET.match(line.rstrip())
        if not m:
            continue
        struck, rest = bool(m.group(1)), m.group(3)
        fields = {}
        t = TRAILER.search(rest)
        if t:
            rest = rest[: t.start()]
            for f in t.group(1).split():
                k, _, v = f.partition("=")
                fields[k] = v
        text = rest.strip()
        if struck:
            text = re.sub(r"~~\s*$", "", text).strip()
        out.append({"text": text, "struck": struck, **fields})
    return out


def wait_for_index(base: str, rel: str, want: str, timeout: float = 15.0) -> bool:
    """Block until the watcher has taken an external edit into the index.

    A hand edit that has not been reindexed is not yet a correction as far as
    reconciliation is concerned, and writing the third step before it lands
    would measure a race rather than a rule.

    The match is scoped to the trial note, and that is not fussiness. The
    selection step writes the same later value into a scratch topic, so an
    unscoped search finds it there and reports success while the note under test
    still holds the old text — which is how the first run of this probe scored
    both arms identically and looked like a clean result.
    """
    deadline = time.time() + timeout
    url = f"{base}/api/memory?q={urllib.parse.quote(want[:60])}&limit=50"
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=20) as r:
                for f in json.loads(r.read().decode() or "[]"):
                    if (f.get("text", "").strip() == want.strip()
                            and f.get("path") == rel):
                        return True
        except Exception:  # noqa: BLE001
            pass
        time.sleep(0.3)
    return False


def classify(rows: list[dict], v2: str) -> str:
    match = [b for b in rows if b["text"].strip() == v2.strip()]
    if not match:
        return "lost"
    b = match[0]
    if b.get("sup"):
        return "inverted"
    if b["struck"]:
        return "resurrected"
    return "survived"


def run_pair(base: str, vault: Path, v1: str, v2: str, mode: str) -> dict:
    topic = next_topic()
    post(base, "/api/memory", {"topic": topic, "agent": "claude",
                               "scope": "topic", "text": v1})
    path = note_path(vault, topic)

    if mode == "handedit":
        body = path.read_text()
        # Rewrite the bullet's text and nothing else — id, stamp and agent stay
        # exactly as the agent minted them, which is precisely the state a
        # person's edit leaves behind.
        new = body.replace(v1, v2, 1)
        if new == body:
            return {"outcome": "skip:no-bullet", "topic": topic}
        path.write_text(new)
        if not wait_for_index(base, f"memory/{topic}.md", v2):
            return {"outcome": "skip:not-indexed", "topic": topic}
    else:
        post(base, "/api/memory", {"topic": topic, "agent": "me", "scope": "topic",
                                   "text": v2, "human": True})

    out = post(base, "/api/memory", {"topic": topic, "agent": "claude",
                                     "scope": "topic", "text": v1})
    results = out.get("results") or []
    challenged = any(r.get("challenges") for r in results)
    rows = bullets(path)
    return {"outcome": classify(rows, v2), "challenged": challenged, "topic": topic}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", default="/tmp/grimoire-dur")
    ap.add_argument("--mode", choices=["handedit", "declared"], default="handedit")
    ap.add_argument("--authority", choices=["on", "off"], default="on")
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--out", default=None)
    args = ap.parse_args()

    if not DATA.exists():
        print(f"missing {DATA} — see longmemeval/PROTOCOL.md for the download")
        return 2
    data = json.loads(DATA.read_text())
    entries = [e for e in data if e["question_type"] == "knowledge-update"
               and not str(e["question_id"]).endswith("_abs")]
    raw = [(e, p) for e in entries for p in evidence_pairs(e)]
    if args.limit:
        raw = raw[: args.limit]
    print(f"{len(entries)} knowledge-update questions → {len(raw)} evidence pairs")

    vault = Path("/tmp/grimoire-durability")
    shutil.rmtree(vault, ignore_errors=True)
    env = {"GRIMOIRE_RATE_LIMIT": "off", "GRIMOIRE_NO_WATCHER": "0"}
    if args.authority == "off":
        env["GRIMOIRE_MEMORY_AUTHORITY"] = "off"

    rows = []
    with launch(args.binary, vault, PORT, embed="auto", env=env) as base:
        for i, (e, (prev, nxt)) in enumerate(raw, 1):
            try:
                pair = recognised_pair(base, prev, nxt)
                if pair is None:
                    rows.append({"outcome": "skip:not-recognised-as-update"})
                else:
                    v1, v2 = pair
                    rows.append({"qid": str(e["question_id"])[:8], "v1": v1, "v2": v2,
                                 **run_pair(base, vault, v1, v2, args.mode)})
            except Exception as exc:  # noqa: BLE001
                rows.append({"outcome": f"ERROR:{type(exc).__name__}"})
            if i % 10 == 0:
                print(f"  {i}/{len(raw)}", flush=True)

    counts = Counter(r["outcome"] for r in rows)
    scored = {k: v for k, v in counts.items() if not k.startswith(("skip:", "ERROR"))}
    n = sum(scored.values())
    label = f"{args.mode} · authority={args.authority}"
    print(f"\n## {label}\n")
    if not n:
        print("no scorable pairs")
        return 1
    print("| outcome | pairs | share |")
    print("|---|---|---|")
    for k in ("survived", "resurrected", "inverted", "lost"):
        if scored.get(k):
            print(f"| {k} | {scored[k]} | {100*scored[k]/n:.1f}% |")
    dropped = {k: v for k, v in counts.items() if k.startswith(("skip:", "ERROR"))}
    for k, v in sorted(dropped.items()):
        print(f"| _{k}_ | {v} | (not scored) |")

    res = scored.get("resurrected", 0) + scored.get("inverted", 0)
    chal = sum(1 for r in rows if r.get("challenged"))
    print(f"\n**resurrection rate: {res}/{n} ({100*res/n:.1f}%)**")
    print(f"**inversion rate: {scored.get('inverted', 0)}/{n} "
          f"({100*scored.get('inverted', 0)/n:.1f}%)**")
    print(f"**challenges opened: {chal}/{n} ({100*chal/n:.1f}%)** — the cost side: "
          f"each one is a decision a person now has to make")
    if args.out:
        Path(args.out).write_text(json.dumps(rows, indent=1))
        print(f"\nrows → {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
