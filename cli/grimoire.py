#!/usr/bin/env python3
"""grimoire CLI — quick capture, daily, search, serve, reindex from the terminal.

Works directly against the vault (no server needed) for local ops, so it's fast
and scriptable. `grimoire serve` starts the web/API.

Usage:
  grimoire new "Title" [body...]      create a note (body from args or stdin)
  grimoire daily [text...]            append to today's daily note (or open it)
  grimoire capture [text...]          quick capture → inbox + daily link
  grimoire search QUERY               full-text search the vault
  grimoire ls [--tag TAG]             list notes
  grimoire open PATH                  print a note
  grimoire reindex                    rebuild the search index
  grimoire ingest PATH [--into DIR]   bulk-import a folder of markdown/text
  grimoire seed-demo                  write a small sample vault (first-run demo)
  grimoire serve [--port N]           run the web app + API
Env: GRIMOIRE_VAULT (default ~/grimoire-vault)
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from server import config, db, index, vault  # noqa: E402


def _ready():
    config.grimoire_dir().mkdir(parents=True, exist_ok=True)
    db.init()
    if not db.one("SELECT COUNT(*) c FROM notes")["c"]:
        index.reindex()


def _stdin_or_args(args):
    if args:
        return " ".join(args)
    if not sys.stdin.isatty():
        return sys.stdin.read()
    return ""


def cmd_new(args):
    if not args:
        print("usage: grimoire new \"Title\" [body...]", file=sys.stderr); return 2
    title = args[0]
    body = _stdin_or_args(args[1:]) or f"# {title}\n\n"
    rel = f"{vault.slugify(title)}.md"
    vault.write(rel, body, {"title": title})
    index.upsert(rel)
    print(rel)


def cmd_daily(args):
    import time
    d = time.strftime("%Y-%m-%d")
    rel = f"{config.DAILY_DIR}/{d}.md"
    if not vault.safe_path(rel).exists():
        vault.write(rel, f"# {d}\n\n", {"title": d, "tags": ["daily"]})
    text = _stdin_or_args(args)
    if text:
        n = vault.read(rel)
        vault.write(rel, n["body"].rstrip() + f"\n- {text}\n", n["frontmatter"])
        index.upsert(rel)
        print(f"appended to {rel}")
    else:
        print(vault.safe_path(rel))


def cmd_capture(args):
    import time
    text = _stdin_or_args(args)
    if not text:
        print("nothing to capture", file=sys.stderr); return 2
    stamp = time.strftime("%Y%m%d-%H%M%S")
    rel = f"{config.INBOX_DIR}/{stamp}.md"
    vault.write(rel, text, {"title": f"capture {stamp}", "tags": ["capture"]})
    index.upsert(rel)
    print(rel)


def cmd_search(args):
    from server.routers.search import search
    q = " ".join(args)
    for r in search(q=q):
        print(f"{r['path']:40}  {r['title']}")


def cmd_ls(args):
    tag = None
    if "--tag" in args:
        tag = args[args.index("--tag") + 1]
    if tag:
        rows = db.query("SELECT n.path,n.title FROM notes n JOIN tags t ON t.note=n.path "
                        "WHERE t.tag=? ORDER BY n.updated DESC", (tag,))
    else:
        rows = db.query("SELECT path,title FROM notes ORDER BY updated DESC")
    for r in rows:
        print(f"{r['path']:40}  {r['title']}")


def cmd_open(args):
    if not args:
        print("usage: grimoire open PATH", file=sys.stderr); return 2
    print(vault.read(args[0])["raw"])


def cmd_reindex(args):
    print(f"indexed {index.reindex()} notes")


def cmd_ingest(args):
    """Bulk-import an existing folder of markdown/text into the vault — the
    cold-start fix, so a new user's agent has something to retrieve on day one.
    usage: grimoire ingest PATH [--into SUBDIR]"""
    if not args:
        print("usage: grimoire ingest PATH [--into SUBDIR]", file=sys.stderr); return 2
    src = Path(args[0]).expanduser()
    into = args[args.index("--into") + 1].strip("/") if "--into" in args else ""
    if not src.exists():
        print(f"no such path: {src}", file=sys.stderr); return 2
    exts = {".md", ".markdown", ".mdown", ".txt"}
    files = [src] if src.is_file() else sorted(p for p in src.rglob("*") if p.is_file())
    n = 0
    for p in files:
        if p.suffix.lower() not in exts or any(seg.startswith(".") for seg in p.parts):
            continue
        try:
            text = p.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            continue
        base = Path(p.name if src.is_file() else str(p.relative_to(src)))
        rel = "/".join(vault.slugify(seg) for seg in base.with_suffix("").parts) + ".md"
        if into:
            rel = f"{into}/{rel}"
        note = vault.note_from_text(rel, text, 0.0)
        fm = dict(note["frontmatter"])
        fm.setdefault("title", base.stem.replace("-", " ").replace("_", " ").strip())
        vault.write(rel, note["body"], fm)
        index.upsert(rel)
        n += 1
    print(f"ingested {n} file(s) into {config.VAULT}")


_DEMO = {
    "deployment-runbook.md": ("Deployment Runbook",
        "# Deployment Runbook\n\nport:: 8443\nowner:: platform-team\n\n"
        "## Rolling back a bad deploy\n1. `docker compose rollback` pins the previous image.\n"
        "2. If the proxy still 502s, the namespaces are stale — do a full "
        "`--force-recreate`, not a plain restart.\n3. Confirm on [[Monitoring]].\n",
        {"pinned": True, "tags": ["ops"]}),
    "monitoring.md": ("Monitoring",
        "# Monitoring\n\nGrafana fronts Prometheus. Alerts page on error rate "
        "> 2% for 5 minutes. The VPN tunnel MTU is pinned to 1280.\n", {"tags": ["ops"]}),
    "team-onboarding.md": ("Team Onboarding",
        "# Team Onboarding\n\nCopy `.env.example` to `.env`, add the CA cert, and "
        "ask the knowledge base before pinging the team.\n", {"tags": ["onboarding"]}),
    "memory/deploy-quirks.md": ("Memory: deploy quirks",
        "The staging deploy needs `--force-recreate` after any VPN change — a "
        "plain restart leaves stale namespaces and outbound calls black-hole.\n",
        {"memory": True, "agent": "claude-code", "task": "debug-session", "tags": ["ops"]}),
}


def cmd_seed_demo(args):
    """Write a small sample vault so a fresh install shows the agent loop
    (retrieval, a fact, a provenance-stamped memory) with zero setup."""
    for rel, (title, body, extra) in _DEMO.items():
        vault.write(rel, body, {"title": title, **extra})
        index.upsert(rel)
    print(f"seeded {len(_DEMO)} demo notes into {config.VAULT} — try: grimoire search deploy")


def cmd_serve(args):
    import uvicorn

    from server.app import create_app
    port = config.PORT
    if "--port" in args:
        port = int(args[args.index("--port") + 1])
    uvicorn.run(create_app(), host=config.HOST, port=port)


def cmd_export(args):
    """Static HTML export of the whole vault (e-ink / offline archive)."""
    from pathlib import Path

    from server.routers import read as rd
    out = Path(args[args.index("--out") + 1]) if "--out" in args else Path("grimoire-export")
    out.mkdir(parents=True, exist_ok=True)
    (out / "index.html").write_text(rd.read_index())
    n = 0
    for row in db.query("SELECT path FROM notes WHERE private=0"):
        stem = rd._u(row["path"])
        try:
            html = rd.read_note(stem)
        except Exception:
            continue
        dest = out / (stem + ".html")
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text(html)
        n += 1
    print(f"exported {n} notes to {out}/ (open index.html)")


def cmd_mcp(args):
    from server import mcp_server
    mcp_server.mcp.run()


def cmd_sync(args):
    """Sync with a peer grimoire. Usage: grimoire sync PEER_URL [--watch] [--interval N] [--token T]"""
    import time as _t

    from server import syncclient
    if not args or args[0].startswith("--"):
        print("usage: grimoire sync PEER_URL [--watch] [--interval N] [--token T]", file=sys.stderr)
        return 2
    peer = args[0]
    token = args[args.index("--token") + 1] if "--token" in args else None
    interval = int(args[args.index("--interval") + 1]) if "--interval" in args else 60
    index.reindex()
    while True:
        try:
            s = syncclient.sync_with_peer(peer, "cli", token)
            print(f"synced {peer}: pulled {s['pulled']}, pushed {s['pushed']}, conflicts {s['conflicts']}")
        except Exception as e:  # noqa: BLE001
            print(f"sync error: {e}", file=sys.stderr)
        if "--watch" not in args:
            break
        _t.sleep(interval)


AGENT_SNIPPET = """\
## Team knowledge base (Grimoire)

This project has a Grimoire context server: runbooks, conventions, ticket
decisions, and agent memory, exposed through the `grimoire` MCP tools.

- Call `get_briefing` once before starting work (pinned notes, onboarding
  rules, recent agent memories).
- Before assuming any project-specific fact or picking an approach, check
  `search_notes` / `ask_notes` / `recall` — the team records accepted fixes
  that are not visible in the code.
- Persist anything future agents need with `remember`.
"""


REFLECT_HOOK = """\
#!/usr/bin/env python3
# Grimoire reflection hook (Claude Code Stop hook): before an agent session
# ends, ask ONCE whether anything durable was learned — and if so, persist it
# via the grimoire `remember` tool. Idempotent: allows the stop on the second
# pass so agents are never trapped.
import json, sys
data = json.load(sys.stdin)
if data.get("stop_hook_active"):
    sys.exit(0)   # already reflected once — allow the stop
print(json.dumps({
    "decision": "block",
    "reason": ("Before finishing: did this session teach you anything a future "
               "agent would need — root causes, gotchas, decisions, environment "
               "rules? If yes, record it with the grimoire `remember` tool "
               "(topic it well). If nothing is worth keeping, just finish.")}))
"""

HOOK_SETTINGS = """\
{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command",
                     "command": "python3 .claude/grimoire-reflect.py" } ] }
    ]
  }
}
"""


def cmd_agent_setup(args):
    """Print everything needed to make agents discover the knowledge base:
    an MCP config block and a snippet for the repo's agent context file
    (CLAUDE.md / AGENTS.md). Discoverability is a deployment concern — agents
    reliably read project context files; they only sometimes browse tool lists."""
    import json as _json
    api_url = args[0] if args else "http://localhost:9111"
    mcp_config = {"mcpServers": {"grimoire": {
        "command": sys.executable,
        "args": ["-m", "server.mcp_server"],
        "env": {"GRIMOIRE_API": api_url, "GRIMOIRE_AGENT_NAME": "my-agent"}}}}
    print("# 1. MCP config (e.g. .mcp.json), or register at user scope for headless runs:")
    print(_json.dumps(mcp_config, indent=2))
    print()
    print("# 2. Add to the repo's CLAUDE.md / AGENTS.md so agents consult the KB:")
    print(AGENT_SNIPPET)
    print("# 3. Optional but measured to matter: a reflection hook so agents")
    print("#    RECORD what they learn before finishing (benchmarked: without it,")
    print("#    agents solve tasks and write nothing). Save as")
    print("#    .claude/grimoire-reflect.py + merge into .claude/settings.json:")
    print(REFLECT_HOOK)
    print(HOOK_SETTINGS)


COMMANDS = {"new": cmd_new, "daily": cmd_daily, "capture": cmd_capture,
            "search": cmd_search, "ls": cmd_ls, "open": cmd_open,
            "reindex": cmd_reindex, "ingest": cmd_ingest, "seed-demo": cmd_seed_demo,
            "serve": cmd_serve, "export": cmd_export,
            "mcp": cmd_mcp, "sync": cmd_sync, "agent-setup": cmd_agent_setup}


def main(argv=None):
    argv = argv if argv is not None else sys.argv[1:]
    if not argv or argv[0] in ("-h", "--help", "help"):
        print(__doc__); return 0
    cmd = argv[0]
    if cmd not in COMMANDS:
        print(f"unknown command {cmd!r}. Try: {', '.join(COMMANDS)}", file=sys.stderr); return 2
    if cmd not in ("serve", "agent-setup"):
        _ready()
    return COMMANDS[cmd](argv[1:]) or 0


if __name__ == "__main__":
    sys.exit(main())
