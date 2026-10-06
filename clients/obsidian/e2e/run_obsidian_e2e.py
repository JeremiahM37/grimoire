#!/usr/bin/env python3
"""Drive the plugin inside a real Obsidian against a real Grimoire server.

The unit and integration tests prove the parser and the client. This proves
the part they cannot: that inside Obsidian the badges render on the right
lines, a hand edit made in the editor is what the server treats as the
person's, a dispute surfaces in the status bar and the panel, and settling it
from the panel rewrites the file.

Usage (from clients/obsidian, after `npm run build` and building the server):

    OBSIDIAN_BIN=/path/to/obsidian-1.x/obsidian \
    python e2e/run_obsidian_e2e.py --out /tmp/grimoire-obsidian-e2e

Needs Xvfb and Python Playwright. Everything runs in throwaway directories:
an isolated HOME for Obsidian's config and a scratch vault, never yours.
"""

import argparse
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

HERE = Path(__file__).resolve().parent
PLUGIN = HERE.parent
REPO = PLUGIN.parent.parent
PLUGIN_ID = "grimoire-memory"


def free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def http(method: str, url: str, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as r:
        raw = r.read()
        return json.loads(raw) if raw else None


def wait(what, fn, timeout=20.0, interval=0.25):
    end = time.time() + timeout
    last = None
    while time.time() < end:
        try:
            v = fn()
            if v:
                return v
        except Exception as e:  # noqa: BLE001 - surfaced on timeout
            last = e
        time.sleep(interval)
    raise AssertionError(f"timed out waiting for {what}" + (f": {last}" if last else ""))


class Run:
    def __init__(self, out: Path):
        self.out = out
        self.passed: list[str] = []

    def check(self, name: str, ok: bool, detail: str = ""):
        if not ok:
            raise AssertionError(f"FAIL {name} {detail}")
        self.passed.append(name)
        print(f"  ok  {name}")

    def shot(self, page, name: str):
        # Toasts are asserted on separately; keep them out of the pictures.
        page.evaluate("() => document.querySelectorAll('.notice-container .notice').forEach(n => n.remove())")
        page.screenshot(path=str(self.out / f"{name}.png"))


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default=tempfile.mkdtemp(prefix="grimoire-obsidian-e2e-"))
    ap.add_argument("--theme", choices=["obsidian", "moonstone"], default="obsidian",
                    help="obsidian = dark, moonstone = light")
    args = ap.parse_args()

    obsidian = os.environ.get("OBSIDIAN_BIN")
    server_bin = os.environ.get("GRIMOIRE_BIN", str(REPO / "go" / "grimoire"))
    if not obsidian or not Path(obsidian).exists():
        sys.exit("set OBSIDIAN_BIN to an unpacked Obsidian binary")
    if not (PLUGIN / "main.js").exists():
        sys.exit("build the plugin first: npm run build")

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix="grimoire-obsidian-vault-"))
    home, vault = work / "home", work / "vault"
    home.mkdir()
    vault.mkdir()
    run = Run(out)
    procs: list[subprocess.Popen] = []

    try:
        # ---- Grimoire, serving the vault Obsidian will open.
        port = free_port()
        base = f"http://127.0.0.1:{port}"
        env = {k: v for k, v in os.environ.items()
               if k not in ("GRIMOIRE_SESSION", "GRIMOIRE_URL")}
        env.update(GRIMOIRE_VAULT=str(vault), GRIMOIRE_PORT=str(port))
        procs.append(subprocess.Popen([server_bin], env=env, stdout=subprocess.DEVNULL,
                                      stderr=open(out / "grimoire.log", "w")))
        wait("grimoire", lambda: http("GET", f"{base}/api/health"))

        def remember(agent, text, topic, **extra):
            return http("POST", f"{base}/api/memory", {"text": text, "topic": topic, "agent": agent, **extra})

        remember("codex", "Billing Postgres runs on port 5432", "ops")
        remember("claude-code", "Nightly backups run at 03:00 UTC", "ops")
        remember("codex", "The on-call rota lives in PagerDuty", "ops",
                 origin="web:wiki.example.com")
        http("POST", f"{base}/api/notes", {
            "path": "runbooks/kestrel.md",
            "body": "# Kestrel\n\nThe kestrel gateway uses copper certificates, rotated every 30 days.",
        })

        # ---- The plugin, installed into the vault and pointed at that server.
        obs = vault / ".obsidian"
        dest = obs / "plugins" / PLUGIN_ID
        dest.mkdir(parents=True)
        for f in ("main.js", "manifest.json", "styles.css"):
            shutil.copy(PLUGIN / f, dest / f)
        (dest / "data.json").write_text(json.dumps({"serverUrl": base, "you": "jeremiah", "pollSeconds": 10}))
        (obs / "community-plugins.json").write_text(json.dumps([PLUGIN_ID]))
        (obs / "appearance.json").write_text(json.dumps({"theme": args.theme}))
        (obs / "app.json").write_text(json.dumps({"livePreview": True, "promptDelete": False}))

        # ---- Obsidian, with an isolated config that already knows the vault.
        cfg = home / ".config" / "obsidian"
        cfg.mkdir(parents=True)
        (cfg / "obsidian.json").write_text(json.dumps({
            "vaults": {"e2e0000000000001": {"path": str(vault), "ts": int(time.time() * 1000), "open": True}},
        }))
        cdp = free_port()
        display = f":{free_port() % 400 + 100}"
        procs.append(subprocess.Popen(["Xvfb", display, "-screen", "0", "1440x900x24", "-nolisten", "tcp"],
                                      stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        time.sleep(0.5)
        oenv = {**os.environ, "HOME": str(home), "XDG_CONFIG_HOME": str(home / ".config"),
                "DISPLAY": display}
        oenv.pop("WAYLAND_DISPLAY", None)
        procs.append(subprocess.Popen(
            [obsidian, "--no-sandbox", f"--remote-debugging-port={cdp}", "--disable-gpu"],
            env=oenv, stdout=open(out / "obsidian.log", "w"), stderr=subprocess.STDOUT))
        wait("obsidian devtools", lambda: http("GET", f"http://127.0.0.1:{cdp}/json/version"), timeout=40)

        with sync_playwright() as p:
            browser = p.chromium.connect_over_cdp(f"http://127.0.0.1:{cdp}")

            def vault_page():
                for ctx in browser.contexts:
                    for pg in ctx.pages:
                        if pg.evaluate("() => !!(window.app && window.app.vault)"):
                            return pg
                return None

            page = wait("the vault window", vault_page, timeout=40)
            errors: list[str] = []
            page.on("console", lambda m: errors.append(m.text) if m.type == "error" else None)
            page.on("pageerror", lambda e: errors.append(str(e)))
            page.set_viewport_size({"width": 1440, "height": 900})

            # A vault with community plugins asks once whether to trust it.
            def trusted():
                btn = page.get_by_text("Trust author and enable plugins")
                if btn.count():
                    btn.first.click()
                return page.evaluate(f"() => !!app.plugins.plugins['{PLUGIN_ID}']")
            wait("plugin to load", trusted, timeout=30)
            run.check("plugin loads inside Obsidian", True)
            page.keyboard.press("Escape")

            def status():
                return page.locator(".grimoire-status").inner_text()
            wait("status bar to connect", lambda: "✓" in status())
            run.check("status bar shows a live connection", "Grimoire ✓" in status(), status())

            # ---- Badges in Live Preview.
            page.evaluate("() => app.workspace.openLinkText('memory/ops.md', '', false)")
            badges = page.locator(".markdown-source-view .grimoire-badge")
            wait("badges to render", lambda: badges.count() >= 3)
            labels = badges.all_inner_texts()
            run.check("each agent's line is badged with its name",
                      "codex" in labels and "claude-code" in labels, str(labels))
            run.check("a fact copied from the web is flagged",
                      any("from web:wiki.example.com" in b for b in labels), str(labels))
            run.check("the raw <!--m trailer is folded away",
                      "<!--m" not in page.locator(".markdown-source-view .cm-content").inner_text())
            page.mouse.click(1300, 860)  # move focus off the lines
            run.shot(page, "01-badges-live-preview")

            # ---- The person corrects the agent in the editor.
            idx = page.evaluate("""() => {
                const ed = app.workspace.activeEditor.editor
                for (let i = 0; i < ed.lineCount(); i++) {
                    const l = ed.getLine(i)
                    if (l.includes('port 5432')) { ed.setLine(i, l.replace('port 5432', 'port 6432')); return i }
                }
                return -1
            }""")
            run.check("found the agent's line in the editor", idx >= 0)
            # Park the cursor on the blank line under the heading, so no memory
            # line is in raw-edit mode.
            page.evaluate("""() => {
                const ed = app.workspace.activeEditor.editor
                for (let i = 0; i < ed.lineCount(); i++) if (ed.getLine(i).startsWith('# ')) { ed.setCursor({line: i + 1, ch: 0}); return }
            }""")
            wait("badge to flip to the person's",
                 lambda: "✎ you edited codex's" in badges.all_inner_texts())
            run.check("the edited line is badged as yours immediately", True)
            page.evaluate("() => app.commands.executeCommandById('editor:save-file')")
            wait("server to see the edit as human", lambda: any(
                h["authority"] == "human" and "6432" in h["text"]
                for h in http("GET", f"{base}/api/memory?q=billing+postgres+port&limit=5")))
            run.check("the server agrees the edit is the person's", True)

            # ---- The agent repeats its old belief.
            res = remember("codex", "Billing Postgres runs on port 5432", "ops")
            run.check("the agent may not overwrite the person",
                      res["results"][0]["op"] == "ADD" and res["results"][0].get("challenges"), str(res))
            page.evaluate(f"() => app.plugins.plugins['{PLUGIN_ID}'].refresh()")
            wait("status bar to show the dispute", lambda: "1 to settle" in status())
            run.check("status bar shows the dispute", True)
            wait("a notice announcing it", lambda: page.locator(".grimoire-notice").count() >= 1, timeout=10)
            run.check("a notice announces it", True)
            wait("dispute badges in the note", lambda: any(
                "disagrees" in b for b in badges.all_inner_texts()), timeout=15)
            labels = badges.all_inner_texts()
            run.check("the person's line shows who disagrees",
                      "✎ you edited codex's · ⚑ codex disagrees" in labels, str(labels))
            run.check("the agent's line shows it is disputing",
                      "⚑ codex disputes yours" in labels, str(labels))
            run.shot(page, "02-dispute-in-note")

            # ---- Settle it from the panel.
            page.locator(".grimoire-status").click()
            panel = page.locator(".grimoire-panel")
            wait("panel", lambda: panel.is_visible())
            page.evaluate("() => { app.workspace.rightSplit.expand(); app.workspace.rightSplit.setSize(380) }")
            card = panel.locator(".grimoire-challenge")
            wait("challenge card", lambda: card.count() == 1)
            run.check("the panel shows both sides",
                      "port 5432" in card.inner_text() and "port 6432" in card.inner_text(), card.inner_text())
            run.shot(page, "03-panel-dispute")
            card.get_by_text("Keep mine").click()
            wait("challenge to clear", lambda: http("GET", f"{base}/api/memory/challenges") == [])
            run.check("Keep mine settles it on the server", True)
            wait("status to clear", lambda: "Grimoire ✓" in status())
            body = (vault / "memory" / "ops.md").read_text()
            agent_line = next(line for line in body.splitlines() if "port 5432" in line)
            run.check("the agent's claim is struck through in the file", agent_line.startswith("- ~~"), agent_line)
            wait("note to show the settled state", lambda: "replaced" in badges.all_inner_texts())
            run.check("the note shows the agent's claim as replaced", True)
            run.shot(page, "04-settled")

            # ---- Make an agent's line yours. The panel took focus; give it back.
            page.evaluate("""() => {
                const leaf = app.workspace.getLeavesOfType('markdown')[0]
                app.workspace.setActiveLeaf(leaf, {focus: true})
                const ed = leaf.view.editor
                for (let i = 0; i < ed.lineCount(); i++) {
                    if (ed.getLine(i).includes('03:00 UTC')) { ed.setCursor({line: i, ch: 2}); return }
                }
            }""")
            page.evaluate(f"() => app.commands.executeCommandById('{PLUGIN_ID}:vouch-line')")
            page.evaluate("() => app.commands.executeCommandById('editor:save-file')")
            wait("file to carry by=human", lambda: any(
                "03:00 UTC" in line and "by=human" in line
                for line in (vault / "memory" / "ops.md").read_text().splitlines()))
            wait("server to treat it as the person's", lambda: any(
                h["authority"] == "human" and "03:00" in h["text"]
                for h in http("GET", f"{base}/api/memory?q=nightly+backups&limit=5")))
            run.check("'Make this memory line mine' gives the line human authority", True)

            # ---- Tell my agents.
            page.evaluate(f"() => app.commands.executeCommandById('{PLUGIN_ID}:tell-agents')")
            modal = page.locator(".modal.grimoire-modal")
            wait("tell modal", lambda: modal.is_visible())
            modal.locator("textarea").fill("Never deploy billing on Fridays")
            topic = modal.locator("input[type=text]")
            run.check("topic defaults to the open memory note", topic.input_value() == "ops", topic.input_value())
            run.shot(page, "05-tell-agents")
            modal.get_by_role("button", name="Save", exact=True).click()
            wait("fact saved as human", lambda: any(
                h["authority"] == "human" and "Fridays" in h["text"] and h.get("agent") == "jeremiah"
                for h in http("GET", f"{base}/api/memory?q=deploy+billing+fridays&limit=5")))
            run.check("'Tell my agents' saves a human fact under your name", True)

            # ---- What would my agent see?
            page.evaluate(f"() => app.commands.executeCommandById('{PLUGIN_ID}:what-would-agent-see-anywhere')")
            wait("retrieve modal", lambda: modal.is_visible())
            q = modal.locator("input.grimoire-query")
            q.fill("how often are kestrel certificates rotated")
            q.press("Enter")
            chunk = modal.locator(".grimoire-chunk")
            wait("retrieved chunks", lambda: chunk.count() >= 1)
            run.check("retrieval shows the runbook the agent would get",
                      "runbooks/kestrel.md" in modal.inner_text() and "copper certificates" in modal.inner_text())
            run.shot(page, "06-what-would-agent-see")
            page.keyboard.press("Escape")

            # ---- Reading view.
            page.evaluate("""() => {
                const leaf = app.workspace.getMostRecentLeaf()
                return leaf.setViewState({...leaf.getViewState(), state: {...leaf.getViewState().state, mode: 'preview'}})
            }""")
            reading = page.locator(".markdown-reading-view .grimoire-badge")
            wait("reading-view badges", lambda: reading.count() >= 3)
            rlabels = reading.all_inner_texts()
            run.check("reading view badges the same lines", "✓ yours" in rlabels and "replaced" in rlabels, str(rlabels))
            run.check("Obsidian accepts the memory note's properties",
                      "Invalid properties" not in page.locator(".markdown-reading-view").inner_text())
            run.shot(page, "07-reading-view")

            # ---- Notes outside memory are untouched.
            page.evaluate("() => app.workspace.openLinkText('runbooks/kestrel.md', '', false)")
            time.sleep(1)
            run.check("ordinary notes get no badges",
                      page.locator(".workspace-leaf.mod-active .grimoire-badge").count() == 0)

            plugin_errors = [e for e in errors if "grimoire" in e.lower()]
            run.check("no plugin errors in the console", not plugin_errors, str(plugin_errors))
            browser.close()

        print(f"\nPASS: {len(run.passed)} checks in real Obsidian. Screenshots: {out}")
        return 0
    except AssertionError as e:
        print(f"\nFAIL: {e}\n({len(run.passed)} checks passed before it). Logs and screenshots: {out}")
        return 1
    finally:
        for proc in reversed(procs):
            proc.terminate()
        for proc in procs:
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
        shutil.rmtree(work, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
