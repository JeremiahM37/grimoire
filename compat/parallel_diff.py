#!/usr/bin/env python3
"""Diff a running Go server against a running Python server, endpoint by endpoint.

This is the acceptance gate for the port. Recorded fixtures prove the pure
functions match; this proves the whole assembled service matches, on real
content, including the rendered HTML byte for byte.

Both servers must be indexing IDENTICAL vault content — otherwise this reports
content drift as implementation divergence. Copy the vault, don't share it:

    cp -a ~/notes /tmp/govault && rm -rf /tmp/govault/.grimoire
    GRIMOIRE_VAULT=/tmp/govault GRIMOIRE_PORT=9121 GRIMOIRE_NO_WATCHER=1 \
        go/grimoire &
    .venv/bin/python compat/parallel_diff.py

Exits non-zero on any difference, so it can gate a release.
"""
from __future__ import annotations

import json
import os
import urllib.error
import urllib.parse
import urllib.request

PY_BASE = os.environ.get("GRIMOIRE_PY_URL", "http://127.0.0.1:9111")
GO_BASE = os.environ.get("GRIMOIRE_GO_URL", "http://127.0.0.1:9121")

checks: int = 0
diffs: list[str] = []


def fetch(base: str, path: str) -> tuple[int, str]:
    try:
        with urllib.request.urlopen(base + path, timeout=30) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def jget(base: str, path: str):
    status, body = fetch(base, path)
    try:
        return status, json.loads(body)
    except Exception:
        return status, body


def compare(path: str, key=None, note: str = "") -> None:
    """Compare one endpoint. `key` normalizes away legitimately-unordered data."""
    global checks
    checks += 1
    pst, pv = jget(PY_BASE, path)
    gst, gv = jget(GO_BASE, path)
    if pst != gst:
        diffs.append(f"{path}: status {pst} vs {gst}")
        return
    if key:
        pv, gv = key(pv), key(gv)
    if pv != gv:
        diffs.append(f"{path}: {note}\n    py: {json.dumps(pv, ensure_ascii=False)[:200]}"
                     f"\n    go: {json.dumps(gv, ensure_ascii=False)[:200]}")


def compare_html(path: str) -> None:
    """Whole-page byte comparison, reporting the first divergent character."""
    global checks
    checks += 1
    pst, pv = fetch(PY_BASE, path)
    gst, gv = fetch(GO_BASE, path)
    if pst != gst:
        diffs.append(f"{path}: status {pst} vs {gst}")
        return
    if pv == gv:
        return
    for i, (a, b) in enumerate(zip(pv, gv, strict=False)):
        if a != b:
            diffs.append(f"{path}: html differs at char {i}\n"
                         f"    py: ...{pv[max(0, i-70):i+70]!r}\n"
                         f"    go: ...{gv[max(0, i-70):i+70]!r}")
            return
    diffs.append(f"{path}: html length {len(pv)} vs {len(gv)}")


def main() -> int:
    # vault path differs by construction (separate copies); everything else must match
    compare("/api/health", key=lambda d: {k: v for k, v in d.items() if k != "vault"})
    compare("/api/notes", note="contents AND order", key=lambda rows: [
        {k: r[k] for k in ("path", "title", "private", "pinned")} for r in rows])
    compare("/api/tags", key=lambda rows: sorted((r["tag"], r["c"]) for r in rows))
    compare("/api/aliases")
    compare("/api/graph", key=lambda d: {
        "nodes": sorted(n["id"] for n in d["nodes"]),
        "edges": sorted((e["src"], e["dst"]) for e in d["edges"]),
        "unresolved": sorted(d["unresolved"])})
    compare("/api/tasks", key=lambda rows: sorted(
        (r["path"], r["line"], r["text"], r["done"]) for r in rows))
    compare("/api/tasks?include_done=1", key=lambda rows: sorted(
        (r["path"], r["line"], r["done"]) for r in rows))
    compare("/api/facts", key=lambda rows: sorted(
        (r["note"], r["key"], r["value"]) for r in rows))
    compare("/api/daily/dates", key=sorted)
    compare("/api/briefing", key=lambda d: {k: sorted(x["path"] for x in v) for k, v in d.items()})
    compare("/api/memory", key=lambda rows: sorted(r["path"] for r in rows))
    for q in ("str", "index", "note", "proj"):
        compare(f"/api/complete?q={q}", note=f"complete {q!r}",
                key=lambda rows: sorted((r["path"], r["stem"]) for r in rows))

    _, notes = jget(PY_BASE, "/api/notes")
    if not isinstance(notes, list):
        print(f"could not list notes from {PY_BASE}: {notes}")
        return 2

    for r in notes:
        p = urllib.parse.quote(r["path"])
        compare(f"/api/notes/{p}", note="note payload", key=lambda d: {
            "path": d.get("path"), "title": d.get("title"), "body": d.get("body"),
            "tags": d.get("tags"), "private": d.get("private"),
            "encrypted": d.get("encrypted"),
            "links": sorted(x.get("target") for x in d.get("links", [])),
            # backlinks are compared IN ORDER: their ordering is deterministic
            "backlinks": [b.get("path") for b in d.get("backlinks", [])]})
        compare(f"/api/notes/{p}/unlinked", note="unlinked mentions",
                key=lambda rows: sorted((x["path"], x["name"]) for x in rows)
                if isinstance(rows, list) else rows)
        compare(f"/api/notes/{p}/history", note="history length",
                key=lambda rows: len(rows) if isinstance(rows, list) else rows)

    for q in ("strategy", "grimoire", "interview", "recipes", "terraform",
              "platform", "memory", "vault"):
        compare(f"/api/search?q={q}", note=f"search {q!r}",
                key=lambda rows: sorted(r["path"] for r in rows))

    html_pages = 0
    for r in notes:
        if r.get("private"):
            continue
        html_pages += 1
        compare_html("/read/" + urllib.parse.quote(r["path"][:-3]))
    compare_html("/read")

    print(f"compared {checks} endpoints ({html_pages} full HTML pages, byte for byte)")
    if diffs:
        print(f"\n{len(diffs)} DIFFERENCES:\n")
        for d in diffs:
            print(" -", d, "\n")
        return 1
    print("\nNO DIFFERENCES — go and python agree on every compared endpoint")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
