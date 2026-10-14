#!/usr/bin/env python3
"""Injection-stream health, counts only.

Reads the adherence trace (a copy of the database, or a daily snapshot) and,
optionally, Claude Code transcripts, and reports how the stream of injected
memory behaves: how much of it repeats, which kinds of item it carries, whether
it reached the model, and whether repeats follow a compaction.

It prints counts and shares only, never a path, a fact text or a session id.
By default it reads rows from before the memory holdout started (2026-10-11),
so no holdout arm is involved; --all reads every row and still never splits by
arm.

    python3 stream.py --db adherence-20261013.db
    python3 stream.py --db snap.db --transcripts ~/.claude/projects
    python3 stream.py --selftest
"""
import argparse
import collections
import json
import os
import sqlite3
import sys
import tempfile

HOLDOUT_START = 1791747366  # 2026-10-11, the holdout's first logged second


def category(target):
    if target.startswith("fact:"):
        return "stored fact"
    path = target.split(":", 1)[-1]
    if path.startswith("Agent Memory/MEMORY.md"):
        return "memory index"
    if path.startswith("Agent Memory/"):
        return "agent memory file"
    if path.startswith("Projects/"):
        return "project note"
    return "other note"


def stream_stats(rows):
    """rows: (session, target, stage) in time order."""
    sessions = collections.defaultdict(list)
    for session, target, stage in rows:
        sessions[session].append((target, stage))
    total = len(rows)
    out = {"injections": total, "sessions": len(sessions)}
    if not total:
        return out
    by_stage = collections.Counter(stage or "prompt" for _, _, stage in rows)
    repeats = collections.Counter()
    for items in sessions.values():
        seen = set()
        for target, stage in items:
            if target in seen:
                repeats[stage or "prompt"] += 1
            seen.add(target)
    out["by_stage"] = dict(by_stage)
    out["repeat_share"] = round(sum(repeats.values()) / total, 3)
    out["repeat_share_by_stage"] = {s: round(repeats[s] / n, 3) for s, n in by_stage.items()}
    targets = collections.Counter(target for _, target, _ in rows)
    ranked = [n for _, n in targets.most_common()]
    out["distinct_items"] = len(targets)
    out["top_share"] = {k: round(sum(ranked[:k]) / total, 3) for k in (1, 10, 50)}
    kinds = collections.Counter()
    for target, n in targets.items():
        kinds[category(target)] += n
    out["by_kind"] = {k: round(n / total, 3) for k, n in kinds.most_common()}
    biggest = max(sessions, key=lambda s: len(sessions[s]))
    rest = [r for r in rows if r[0] != biggest]
    if rest and len(sessions) > 1:
        inner = stream_stats(rest)
        out["repeat_share_without_largest_session"] = inner["repeat_share"]
    return out


def transcript_stats(tool_targets, root):
    """tool_targets: tool_use_id -> [target]. Walks Claude Code transcripts and
    reports delivery (a hook context attached to that tool call) and, per
    transcript, whether a repeat came after a compaction."""
    wanted = set(tool_targets)
    found_calls, delivered = set(), set()
    first = same = after = compactions = 0
    for directory, _, files in os.walk(root):
        for name in files:
            if not name.endswith(".jsonl"):
                continue
            sequence = []
            try:
                handle = open(os.path.join(directory, name), errors="ignore")
            except OSError:
                continue
            with handle:
                for line in handle:
                    if "compact_boundary" not in line and "toolu_" not in line:
                        continue
                    try:
                        record = json.loads(line)
                    except ValueError:
                        continue
                    if record.get("subtype") == "compact_boundary":
                        sequence.append(("compact", None))
                        continue
                    attachment = record.get("attachment")
                    if (isinstance(attachment, dict) and attachment.get("type") == "hook_additional_context"
                            and attachment.get("toolUseID") in wanted):
                        delivered.add(attachment["toolUseID"])
                    message = record.get("message")
                    if isinstance(message, dict) and isinstance(message.get("content"), list):
                        for part in message["content"]:
                            if (isinstance(part, dict) and part.get("type") == "tool_use"
                                    and part.get("id") in wanted):
                                found_calls.add(part["id"])
                                sequence.append(("call", part["id"]))
            last, epoch = {}, 0
            for kind, value in sequence:
                if kind == "compact":
                    epoch += 1
                    compactions += 1
                    continue
                for target in tool_targets[value]:
                    if target not in last:
                        first += 1
                    elif last[target] == epoch:
                        same += 1
                    else:
                        after += 1
                    last[target] = epoch
    shown = first + same + after
    return {
        "tool_calls_logged": len(wanted),
        "tool_calls_found": len(found_calls),
        "delivered_share": round(len(delivered & found_calls) / len(found_calls), 3) if found_calls else None,
        "compactions": compactions,
        "first_showing": round(first / shown, 3) if shown else None,
        "repeat_same_window": round(same / shown, 3) if shown else None,
        "repeat_after_compaction": round(after / shown, 3) if shown else None,
    }


def load(db, include_all):
    connection = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    where = "" if include_all else f"WHERE ts < {HOLDOUT_START}"
    rows = connection.execute(f"SELECT session, target, stage, tu FROM injections {where} ORDER BY session, ts, id").fetchall()
    connection.close()
    return rows


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--db", help="a copy of adherence.db or a daily snapshot")
    parser.add_argument("--transcripts", help="Claude Code projects directory, for delivery and compaction")
    parser.add_argument("--all", action="store_true", help="include rows from after the holdout started")
    parser.add_argument("--selftest", action="store_true")
    args = parser.parse_args(argv)
    if args.selftest:
        return selftest()
    if not args.db:
        parser.error("--db is required")
    rows = load(args.db, args.all)
    report = stream_stats([(s, t, stage) for s, t, stage, _ in rows])
    if args.transcripts:
        tool_targets = collections.defaultdict(list)
        for _, target, stage, tu in rows:
            if stage == "action" and tu:
                tool_targets[tu].append(target)
        report["transcripts"] = transcript_stats(tool_targets, os.path.expanduser(args.transcripts))
    print(json.dumps(report, indent=2))
    return 0


def selftest():
    checks = 0
    rows = [("s1", "fact:a", "action"), ("s1", "fact:a", "action"), ("s1", "note:Projects/x.md", ""),
            ("s2", "note:Agent Memory/m.md", "action"), ("s2", "note:Agent Memory/m.md", "")]
    out = stream_stats(rows)
    assert out["injections"] == 5 and out["sessions"] == 2; checks += 1
    assert out["repeat_share"] == 0.4, out; checks += 1
    assert out["repeat_share_by_stage"] == {"action": round(1 / 3, 3), "prompt": 0.5}, out; checks += 1
    assert out["by_kind"]["agent memory file"] == 0.4 and out["by_kind"]["stored fact"] == 0.4; checks += 1
    assert out["top_share"][1] == 0.4; checks += 1
    with tempfile.TemporaryDirectory() as root:
        lines = [
            {"message": {"content": [{"type": "tool_use", "id": "toolu_1"}]}},
            {"attachment": {"type": "hook_additional_context", "toolUseID": "toolu_1"}},
            {"message": {"content": [{"type": "tool_use", "id": "toolu_2"}]}},
            {"subtype": "compact_boundary"},
            {"message": {"content": [{"type": "tool_use", "id": "toolu_3"}]}},
            {"attachment": {"type": "hook_additional_context", "toolUseID": "toolu_3"}},
        ]
        with open(os.path.join(root, "t.jsonl"), "w") as handle:
            handle.write("\n".join(json.dumps(line) for line in lines))
        got = transcript_stats({"toolu_1": ["fact:a"], "toolu_2": ["fact:a"], "toolu_3": ["fact:a"]}, root)
        assert got["tool_calls_found"] == 3 and got["delivered_share"] == round(2 / 3, 3), got; checks += 1
        assert got["first_showing"] == round(1 / 3, 3) and got["repeat_same_window"] == round(1 / 3, 3); checks += 1
        assert got["repeat_after_compaction"] == round(1 / 3, 3) and got["compactions"] == 1; checks += 1
    with tempfile.TemporaryDirectory() as root:
        db = os.path.join(root, "a.db")
        connection = sqlite3.connect(db)
        connection.execute("CREATE TABLE injections (id INTEGER PRIMARY KEY, session TEXT, target TEXT, stage TEXT, tu TEXT, ts INTEGER)")
        connection.executemany("INSERT INTO injections (session, target, stage, tu, ts) VALUES (?,?,?,?,?)",
                               [("s", "fact:a", "action", "toolu_9", HOLDOUT_START - 5),
                                ("s", "fact:a", "action", "toolu_10", HOLDOUT_START + 5)])
        connection.commit()
        connection.close()
        assert len(load(db, False)) == 1 and len(load(db, True)) == 2; checks += 1
    print(f"selftest ok ({checks} checks)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
