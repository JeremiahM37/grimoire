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


def fetch(base: str, path: str, method: str = "GET", body=None) -> tuple[int, str]:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


def jget(base: str, path: str, method: str = "GET", body=None):
    status, body = fetch(base, path, method, body)
    try:
        return status, json.loads(body)
    except Exception:
        return status, body


def compare(path: str, key=None, note: str = "",
            method: str = "GET", body=None) -> None:
    """Compare one endpoint. `key` normalizes away legitimately-unordered data.

    POST endpoints are compared too, and must be: a GET-only sweep reported
    "no differences" while /api/ask returned a different answer format and
    /api/actions a different tag order, because neither was ever requested.
    Writes go to the throwaway note below, never to shared fixtures.
    """
    global checks
    checks += 1
    pst, pv = jget(PY_BASE, path, method, body)
    gst, gv = jget(GO_BASE, path, method, body)
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


def compare_writes() -> None:
    """The mutating surface, run LAST and cleaned up.

    Writes are compared at fixed paths and then deleted, because a comparison
    that leaves notes behind poisons every later run: the second sweep sees a
    scratch note on one side only and reports it as an implementation
    difference. Timestamp-named endpoints (capture, audio) are deliberately
    excluded — two servers called in sequence can land in different seconds, so
    their paths differ by construction and prove nothing.
    """
    scratch = "compat-scratch.md"
    body = "# Scratch\n\nport:: 1234\n\nbody #compat"
    view = lambda d: {k: d.get(k) for k in ("path", "title", "tags")}  # noqa: E731

    compare("/api/notes", method="POST", note="create note",
            body={"path": scratch, "body": body}, key=view)
    compare(f"/api/notes/{scratch}", method="PUT", note="update note",
            body={"body": "# Scratch\n\nedited #compat"}, key=view)
    compare("/api/facts", method="POST", note="set fact (append)",
            body={"note": scratch, "key": "owner", "value": "platform"})
    compare("/api/facts", method="POST", note="set fact (update in place)",
            body={"note": scratch, "key": "OWNER", "value": "infra"})
    compare(f"/api/notes/{scratch}", note="note after fact edits",
            key=lambda d: {"body": d.get("body"), "tags": d.get("tags")})
    compare("/api/facts", method="POST", note="set fact on a missing note",
            body={"note": "nope.md", "key": "k", "value": "v"})
    compare(f"/api/notes/{scratch}/pin", method="POST", note="pin",
            key=lambda d: {k: d.get(k) for k in ("path", "pinned")})
    compare(f"/api/notes/{scratch}/pin", method="POST", note="unpin",
            key=lambda d: {k: d.get(k) for k in ("path", "pinned")})
    compare(f"/api/notes/{scratch}/duplicate", method="POST", note="duplicate",
            key=lambda d: {"path": d.get("path")})
    compare("/api/memory", method="POST", note="remember",
            body={"topic": "compat", "text": "a durable fact", "agent": "compat-check"},
            key=lambda d: {"path": d.get("path")})
    compare("/api/memory?q=durable", note="recall",
            key=lambda rows: [r["path"] for r in rows] if isinstance(rows, list) else rows)

    for rel in (scratch, "scratch-copy.md", "memory/compat.md"):
        for base in (PY_BASE, GO_BASE):
            fetch(base, "/api/notes/" + urllib.parse.quote(rel), "DELETE")
    # the trash keeps a copy, so empty it too or the next run starts dirty
    for base in (PY_BASE, GO_BASE):
        _, items = jget(base, "/api/trash")
        for item in items if isinstance(items, list) else []:
            tid = item.get("id") or item.get("trash_id")
            if tid:
                fetch(base, f"/api/trash/{tid}", "DELETE")


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

    # Search VARIANTS, not just the default shape. A gap here cost 6.4pp on the
    # scored benchmark and was invisible to the plain-query comparison: full=true
    # bodies, the limit, the operators, and above all the any-term fallback that
    # question-shaped queries depend on.
    for path, note in (
        ("/api/search?q=where%20do%20the%20notes%20live&full=true",
         "full=true bodies for a question-shaped query"),
        ("/api/search?q=grimoire&full=true", "full=true bodies"),
        ("/api/search?q=grimoire&limit=2", "limit"),
        ("/api/search?q=this%20query%20matches%20absolutely%20no%20single%20note%20term",
         "any-term OR fallback"),
        ("/api/search?q=tag:daily", "tag: operator"),
        ("/api/search?q=is:pinned", "is:pinned operator"),
        ("/api/search?q=path:journal", "path: operator"),
    ):
        compare(path, note=note, key=lambda rows: [
            {"path": r["path"], "body": r.get("body"),
             "excerpted": r.get("excerpted")} for r in rows])

    # ---- read-only POST surfaces -------------------------------------------
    # A GET-only sweep reported "no differences" while /api/ask returned a
    # different answer format and /api/actions a different tag order, because
    # neither was ever requested. These change nothing, so they run inline.
    compare("/api/ask", method="POST", note="ask (extractive answer + citations)",
            body={"q": "where do the notes live", "smart": False},
            key=lambda d: {"answer": d.get("answer"),
                           "citations": [c.get("path") for c in d.get("citations", [])]})
    compare("/api/ask", method="POST", note="ask with no query", body={"q": ""})
    for action in ("tags", "title", "summarize", "expand", "bogus"):
        compare("/api/actions", method="POST", note=f"action {action!r}",
                body={"action": action,
                      "text": "Deployment runbook. Deployment rollback via proxy.\n"
                              "The gateway listens on 8443. Restart is not enough."})
    compare("/api/query", method="POST", note="query block",
            body={"block": "TABLE FROM #daily"},
            key=lambda d: d if not isinstance(d, dict) else {
                "columns": d.get("columns"),
                "rows": sorted(map(str, d.get("rows", [])))})
    compare("/api/retrieve?q=notes&k=3", note="retrieve top-k",
            key=lambda rows: [r["path"] for r in rows] if isinstance(rows, list) else rows)
    compare("/api/sync/status", note="sync status")
    compare("/api/sync/manifest", note="sync manifest",
            key=lambda d: sorted(d) if isinstance(d, dict) else d)
    compare("/api/sync/pull", method="POST", note="sync pull",
            body={"paths": ["index.md", "definitely-missing.md"]},
            key=lambda d: {k: (v is None) for k, v in d.get("contents", {}).items()})
    compare("/api/sync/now", method="POST", note="sync with no peer configured")
    compare("/api/memory/consolidate", method="POST", note="consolidate (no LLM)",
            body={"path": "does-not-exist.md"})

    html_pages = 0
    for r in notes:
        if r.get("private"):
            continue
        html_pages += 1
        compare_html("/read/" + urllib.parse.quote(r["path"][:-3]))
    compare_html("/read")

    compare_writes()

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
