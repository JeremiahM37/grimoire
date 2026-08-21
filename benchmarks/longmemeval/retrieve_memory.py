"""Retrieve LongMemEval context from the MEMORY ENGINE instead of from chunks.

Every published Grimoire number — and every condition in REPORT.md — ingests a
haystack as plain session notes and retrieves passages from them. That measures
retrieval. It does not touch the memory engine at all, which is the half of the
product that decides what a later statement DOES to an earlier one, and which
is the half every memory layer (mem0, Zep, Letta) is actually built around.

So this arm ingests the same haystacks through `POST /api/memory/batch` — facts
extracted per turn and reconciled against everything already written — and
answers from `GET /api/memory` instead of from chunks.

# The ablation

Two arms differing in ONE thing:

    reconciled   writes reconcile vault-wide: a later fact may supersede an
                 earlier one, and a superseded fact stops being recalled
    append       writes are scoped to their own session id, so nothing can
                 supersede anything written in another session — an
                 append-only store with identical extraction, identical
                 retrieval and an identical reader

`scope: session` is a shipped feature being used for its documented purpose
(bounding what a write may supersede), not a benchmark-only switch. That
matters: the control arm is a configuration a user could actually run.

# Two deliberate methodology choices

**User turns only.** The assistant's turns are the model's own prose; a fact
extracted from them is a fact about what a chatbot said. mem0-style extraction
makes the same choice. Assistant turns still shape the conversation the user
replies to, so nothing is lost that the user did not also state.

**Sessions are ingested in DATE order, not haystack order.** Reconciliation is
order-dependent by construction — that is what "supersede" means — so feeding
the sessions in storage order would let a stale fact overwrite a current one
and would measure the shuffle rather than the mechanism. `haystack_dates` gives
the real order and is used.

**Facts are stamped with wall-clock time, not the session date**, because the
API takes no stamp override and adding one for a benchmark would be a product
change made to flatter a number. The consequence is that recency decay is flat
across the whole haystack, which can only HURT this arm: the mechanism under
test has to win on supersession alone, with no help from "prefer the newer
fact" in ranking.
"""
from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request


def _post(base: str, path: str, body: dict, timeout: float = 300) -> dict:
    req = urllib.request.Request(
        base + path, data=json.dumps(body).encode(),
        headers={"Content-Type": "application/json"}, method="POST")
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode() or "{}")


def _get(base: str, path: str, timeout: float = 120):
    with urllib.request.urlopen(base + path, timeout=timeout) as r:
        return json.loads(r.read().decode() or "null")


def sessions_in_date_order(entry: dict) -> list[tuple[str, list]]:
    """(date, turns) per session, oldest first."""
    dates = entry.get("haystack_dates") or []
    sessions = entry.get("haystack_sessions") or []
    pairs = []
    for i, turns in enumerate(sessions):
        date = dates[i] if i < len(dates) else ""
        pairs.append((date, turns))
    # Lexicographic on "2023/05/20 (Sat) 02:21" sorts correctly: the fields are
    # fixed-width and most-significant-first. Sessions with no date sort first,
    # which is the conservative placement — they cannot overwrite a dated fact.
    pairs.sort(key=lambda p: p[0])
    return pairs


def user_statements(turns: list) -> list[str]:
    """The user's own turns, cleaned to one statement per item."""
    out = []
    for t in turns:
        if (t.get("role") or "") != "user":
            continue
        text = (t.get("content") or "").strip()
        if not text:
            continue
        # The write API bounds a statement at 20000 characters. Truncating is
        # better than dropping: the head of a long turn is where the user
        # states the thing, and a dropped turn is a silently missing fact.
        out.append(text[:19000])
    return out


def ingest(base: str, entry: dict, *, reconciled: bool, topic: str = "lme") -> dict:
    """Write one haystack into the memory store. Returns write statistics."""
    stats = {"sessions": 0, "items": 0, "ops": {}}
    for i, (date, turns) in enumerate(sessions_in_date_order(entry)):
        items = user_statements(turns)
        if not items:
            continue
        # The batch endpoint takes a full write per item and caps at 100.
        def item(text: str) -> dict:
            it = {"topic": topic, "agent": "lme", "text": text}
            if not reconciled:
                # The control arm. Each session gets its own scope, so a write
                # can only supersede a fact from the SAME session — across
                # sessions the store is append-only. Not a benchmark switch:
                # this is the shipped `scope` field doing what it documents.
                it["scope"] = "session"
                it["session"] = f"s{i:03d}"
            return it

        for start in range(0, len(items), 50):
            chunk = items[start:start + 50]
            try:
                out = _post(base, "/api/memory/batch",
                            {"items": [item(t) for t in chunk]})
            except urllib.error.HTTPError as e:
                # One rejected batch must not abandon a haystack that takes a
                # minute to build. The API refuses a statement outside
                # 1..20000 characters, and a transcript occasionally has one;
                # the honest handling is to count it and keep going, so the
                # loss is visible in the record rather than invisible in a
                # missing store.
                stats["rejected"] = stats.get("rejected", 0) + len(chunk)
                continue
            except (urllib.error.URLError, TimeoutError) as e:
                raise RuntimeError(f"ingest failed at session {i}: {e}") from e
            for res in (out.get("results") or []):
                for r in (res.get("results") or [res]):
                    op = r.get("op", "?")
                    stats["ops"][op] = stats["ops"].get(op, 0) + 1
        stats["sessions"] += 1
        stats["items"] += len(items)
    return stats


def recall(base: str, question: str, limit: int) -> list[dict]:
    q = urllib.parse.urlencode({"q": question, "limit": limit})
    return _get(base, "/api/memory?" + q) or []


def context_from(facts: list[dict]) -> str:
    """Render recalled facts as the reader's context.

    One fact per line with its date, which is the shape the store holds them
    in. No scores and no ids: the reader is answering a question, not auditing
    a ranking, and a number beside every line invites it to reason about the
    ranking instead of the content.
    """
    lines = []
    for f in facts:
        stamp = (f.get("stamp") or "").strip()
        text = (f.get("text") or "").strip()
        if not text:
            continue
        lines.append(f"- {text}" + (f"  ({stamp})" if stamp else ""))
    return "\n".join(lines)


def stored_facts(base: str) -> dict:
    """How many facts the store holds, and how many are still believed.

    Read from `/api/memory/export`, not from a recall: recall clamps at 200 by
    design, so counting its rows measures the clamp. That mistake produced a
    "stored=200" for every haystack in the first run of this harness, which is
    exactly the shape of a number nobody checks.
    """
    out = _get(base, "/api/memory/export", timeout=300) or {}
    entries = out.get("entries") or []
    live = sum(1 for e in entries if not e.get("superseded_by"))
    return {"total": len(entries), "live": live,
            "superseded": len(entries) - live}
