"""Regenerate the README screenshots.

    .venv/bin/python tools/screenshots.py

Screenshots go stale silently — the ones this replaced were taken before the Go
rewrite and stayed in the README for a month after the UI they showed had
changed. Scripting the capture makes refreshing them a command rather than an
afternoon, and pins what they contain: a seeded demo vault, never a real one,
so no personal note can end up in a public README.

The server is launched on a spare port against a throwaway vault, so this never
touches a running instance or the notes you actually keep.
"""
from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.request
from pathlib import Path

from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / "docs" / "screenshots"
BINARY = os.environ.get("GRIMOIRE_BINARY", str(ROOT / "go" / "grimoire"))
PASSPHRASE = "demo vault passphrase not a real one"

# A demo vault with enough shape to photograph: folders, wiki-links, tags,
# tasks, code and tables. seed-demo alone is five notes, which photographs as an
# empty app. Nothing here is real — the point of scripting this is that a public
# README can never accidentally show a private note.
NOTES = [
    ("engineering/deployment-runbook.md", {"title": "Deployment Runbook", "tags": ["ops", "runbook"], "pinned": True}, """
# Deployment Runbook

owner:: platform-team
port:: 8443

## Rolling back a bad deploy

1. `docker compose rollback` pins the previous image.
2. If the proxy still returns 502, the namespaces are stale — do a full
   `--force-recreate`, not a plain restart.
3. Confirm the error rate on [[Monitoring]] before declaring it over.

> A restart that "worked" but left traffic broken is the failure mode this
> runbook exists for.

- [x] Alert routing verified
- [ ] Runbook rehearsed this quarter
"""),
    ("engineering/monitoring.md", {"title": "Monitoring", "tags": ["ops"]}, """
# Monitoring

Grafana fronts Prometheus. Alerts page on an error rate above 2% for five
minutes; anything shorter is a deploy, not an incident.

| Signal | Threshold | Pages |
|---|---|---|
| error rate | > 2% for 5m | yes |
| p95 latency | > 800ms for 10m | yes |
| disk | > 85% | no |

The last time this paged for real: [[Incident: checkout 502s]].

The VPN tunnel MTU is pinned to 1280 — see [[Networking Notes]] for why a
working handshake can still black-hole every packet.
"""),
    ("engineering/networking-notes.md", {"title": "Networking Notes", "tags": ["ops", "networking"]}, """
# Networking Notes

Path MTU discovery fails silently behind the tunnel: the handshake completes,
small packets flow, and anything larger disappears. Pinning the MTU is the fix.

```bash
ip link set dev wg0 mtu 1280
ping -M do -s 1252 10.0.0.1   # must succeed before you believe it
```

Related: [[Deployment Runbook]], [[Monitoring]].
"""),
    ("engineering/postgres.md", {"title": "Postgres", "tags": ["database"]}, """
# Postgres

An index is not free on write. Check `pg_stat_user_indexes` before adding
another — an unused index costs every insert and buys nothing.

```sql
SELECT relname, indexrelname, idx_scan
FROM pg_stat_user_indexes WHERE idx_scan = 0 ORDER BY relname;
```

Write amplification shows up first as p95 latency, so watch [[Monitoring]]
after adding one.
"""),
    ("engineering/incident-2026-06-14.md", {"title": "Incident: checkout 502s", "tags": ["incident"]}, """
# Incident: checkout 502s

**Impact** — 11 minutes of failed checkouts, 3% of sessions.

**Cause** — a deploy recreated the VPN sidecar; dependent containers stayed
pinned to the old network namespace, so published ports answered nothing.

**Fix** — force-recreate the dependents, per [[Deployment Runbook]], now
automated by a guard service. The MTU pinning in [[Networking Notes]] is
unrelated and was ruled out early.

**Follow-ups** own a name each, per [[Team Onboarding]]:

- [x] Guard service shipped
- [ ] Alert on namespace mismatch directly
"""),
    ("product/roadmap.md", {"title": "Roadmap", "tags": ["product"], "pinned": True}, """
# Roadmap

## This quarter
- Incremental indexing — no full rebuild on restart
- Per-write cache maintenance, so a write does not stall the next query
- Request limits before anything is exposed beyond the tunnel

## Later
- Approximate nearest neighbours, but only once queries are measurably slow

Scored against the fixed question set in [[Retrieval Notes]]; the reasoning
behind each item is in [[Decisions]].
"""),
    ("product/decisions.md", {"title": "Decisions", "tags": ["product"]}, """
# Decisions

**Plain markdown on disk, always.** The index is a cache and can be deleted; the
notes cannot. Anything that makes the files unreadable without the app is out.

**Retrieval is measured, not asserted.** Every change to ranking is scored on a
fixed question set before it ships — the numbers live in [[Retrieval Notes]],
the sequencing in [[Roadmap]].
"""),
    ("team/team-onboarding.md", {"title": "Team Onboarding", "tags": ["onboarding"]}, """
# Team Onboarding

Copy `.env.example` to `.env`, add the CA certificate, then run the stack.

- Read [[Deployment Runbook]] before your first on-call week.
- Every incident action item has exactly one owner. An action without an owner
  is a wish.
- Ask the notes rather than a person first — that is what they are for.
"""),
    ("team/oncall.md", {"title": "On-call", "tags": ["onboarding", "ops"]}, """
# On-call

One-week rotations starting Thursday morning. The pager escalates after ten
minutes to the secondary and after twenty to the engineering manager.

Handover happens in writing, not in a meeting. Start from [[Deployment Runbook]]
and the most recent [[Incident: checkout 502s]] write-up.
"""),
    ("research/retrieval-notes.md", {"title": "Retrieval Notes", "tags": ["research"]}, """
# Retrieval Notes

Hybrid retrieval beats either leg alone when the corpus does not fit the
reader's window — and loses to simply reading everything when it does.

| method | LongMemEval | context |
|---|---|---|
| dense only | 69.0% | 7.2k |
| lexical only | 70.0% | 6.4k |
| hybrid | **77.5%** | 7.6k |
| everything | 69.0% | 118k |

Which is why the size check exists at all — see [[Decisions]].
"""),
]

# Memories a working agent would actually have written, so the provenance panel
# and the 🤖 badges have something to show.
MEMORIES = [
    ("claude-code", "ship the release pipeline",
     "The release archive must contain web/ as a whole directory. A `web/**/*` glob "
     "matches only paths with a directory component, so index.html was left out and "
     "the published binary served 404 at /."),
    ("claude-code", "speed up indexing",
     "fts.path is UNINDEXED, so deleting a note's full-text row by path scans the "
     "entire FTS index. Deleting by rowid took a 50k-note rebuild from 897s to 24s."),
    ("research-agent", "compare retrieval strategies",
     "Hybrid retrieval beats dense-only by 8.5 points on LongMemEval at a fifteenth "
     "of the context tokens, but loses to full context on LoCoMo, whose conversations "
     "fit the window. Retrieval helps when the corpus does not."),
    ("ops-agent", "watch the vault broker",
     "The credential vault must unlock at startup from the passphrase file, not from "
     "a boot-only unit: a restart otherwise leaves every use_credential failing 423."),
]

SECRETS = [
    ("github-token", "ghp_demo_value_never_displayed"),
    ("notion-api-key", "secret_demo_value_never_displayed"),
    ("weather-api-key", "demo_value_never_displayed"),
]


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def api(base: str, path: str, body=None, method=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(base + path, data=data,
                                 method=method or ("POST" if data else "GET"),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=60) as r:
        raw = r.read()
    return json.loads(raw) if raw else {}


def seed(base: str, vault: Path):
    for rel, meta, body in NOTES:
        payload = {"path": rel, "body": body.strip() + "\n",
                   "title": meta["title"], "tags": meta.get("tags", [])}
        if meta.get("pinned"):
            payload["frontmatter"] = {"pinned": True}
        api(base, "/api/notes", payload)
    for agent, task, text in MEMORIES:
        api(base, "/api/memory", {"agent": agent, "task": task, "text": text})
    api(base, "/api/vault/init", {"passphrase": PASSPHRASE})
    for name, value in SECRETS:
        api(base, "/api/secrets", {"name": name, "value": value,
                                   "meta": {"source": "demo"}})
    # one live grant, so the console shows what an agent currently holds
    api(base, "/api/secrets/github-token/grant",
        {"grantee": "claude-code", "scope": "https://api.github.com", "ttl": 3600})


def shot(page, name: str, full=False):
    OUT.mkdir(parents=True, exist_ok=True)
    page.wait_for_timeout(450)
    page.screenshot(path=str(OUT / name), full_page=full)
    print("wrote", OUT / name)


def open_note(page, fragment: str):
    page.click(f"#note-list .note-row:has-text('{fragment}')")
    page.wait_for_timeout(600)


def capture(base: str):
    with sync_playwright() as p:
        browser = p.chromium.launch()
        ctx = browser.new_context(viewport={"width": 1440, "height": 900},
                                  device_scale_factor=2)
        page = ctx.new_page()
        page.goto(base)
        page.wait_for_selector("body[data-ready]", timeout=15000)
        page.wait_for_timeout(800)

        open_note(page, "Deployment Runbook")
        shot(page, "hero.png")

        page.click("#preview-toggle")
        shot(page, "preview.png")
        page.click("#preview-toggle")

        page.click("#graph-open")
        page.wait_for_timeout(1200)
        shot(page, "graph.png")
        page.click("#graph-close")

        # agent memory: the 🤖 badges in the list plus the provenance banner
        page.click("#palette-open")
        page.fill("#palette-input", "Agent memories")
        page.wait_for_timeout(400)
        page.keyboard.press("Enter")
        page.wait_for_timeout(900)
        shot(page, "agent-memory.png")

        # Retrieval inspection asks for the query through a browser prompt(),
        # so the dialog has to be answered rather than typed into.
        page.once("dialog", lambda d: d.accept("why did the deploy still 502"))
        page.click("#palette-open")
        page.fill("#palette-input", "retrieval inspection")
        page.wait_for_timeout(400)
        page.keyboard.press("Enter")
        page.wait_for_selector("#inspect-modal:not(.hidden)", timeout=10000)
        page.wait_for_timeout(1500)
        shot(page, "retrieval-inspection.png")
        page.click("#inspect-close")

        page.click("#vault-open")
        page.wait_for_timeout(900)
        shot(page, "credential-console.png")
        page.click("#vault-close")

        page.click("#palette-open")
        page.fill("#palette-input", "")
        page.wait_for_timeout(500)
        shot(page, "palette.png")
        page.keyboard.press("Escape")

        page.click("#theme-toggle")
        page.wait_for_timeout(700)
        open_note(page, "Retrieval Notes")
        page.click("#preview-toggle")
        shot(page, "dark.png")
        page.click("#preview-toggle")
        page.click("#theme-toggle")
        page.wait_for_timeout(500)

        ctx.close()
        mobile = browser.new_context(viewport={"width": 414, "height": 896},
                                     device_scale_factor=3, is_mobile=True,
                                     has_touch=True)
        mpage = mobile.new_page()
        mpage.goto(base)
        mpage.wait_for_selector("body[data-ready]", timeout=15000)
        mpage.wait_for_timeout(1000)
        shot(mpage, "mobile.png")
        browser.close()


def main() -> int:
    if not Path(BINARY).exists():
        print(f"{BINARY} not built — run: cd go && go build -o grimoire ./cmd/grimoire")
        return 1
    vault = Path(tempfile.mkdtemp(prefix="grimoire-shots-"))
    port = free_port()
    env = {**os.environ, "GRIMOIRE_VAULT": str(vault), "GRIMOIRE_PORT": str(port),
           "GRIMOIRE_WEB_DIR": str(ROOT / "web"),
           "GRIMOIRE_PLUGIN_DIR": str(ROOT / "plugins"),
           "GRIMOIRE_NO_WATCHER": "1"}
    env.pop("GRIMOIRE_OLLAMA_URL", None)  # the demo must not depend on a local LLM
    proc = subprocess.Popen([BINARY], env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = f"http://127.0.0.1:{port}"
    try:
        for _ in range(120):
            try:
                api(base, "/api/health"); break
            except Exception:  # noqa: BLE001 — not up yet
                if proc.poll() is not None:
                    raise RuntimeError("server exited before answering") from None
                time.sleep(0.25)
        seed(base, vault)
        capture(base)
    finally:
        proc.terminate()
        proc.wait(timeout=30)
        shutil.rmtree(vault, ignore_errors=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
