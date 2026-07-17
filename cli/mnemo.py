#!/usr/bin/env python3
"""mnemo CLI — quick capture, daily, search, serve, reindex from the terminal.

Works directly against the vault (no server needed) for local ops, so it's fast
and scriptable. `mnemo serve` starts the web/API.

Usage:
  mnemo new "Title" [body...]      create a note (body from args or stdin)
  mnemo daily [text...]            append to today's daily note (or open it)
  mnemo capture [text...]          quick capture → inbox + daily link
  mnemo search QUERY               full-text search the vault
  mnemo ls [--tag TAG]             list notes
  mnemo open PATH                  print a note
  mnemo reindex                    rebuild the search index
  mnemo serve [--port N]           run the web app + API
Env: MNEMO_VAULT (default ~/mnemo-vault)
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from server import config, db, index, vault  # noqa: E402


def _ready():
    config.mnemo_dir().mkdir(parents=True, exist_ok=True)
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
        print("usage: mnemo new \"Title\" [body...]", file=sys.stderr); return 2
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
        print("usage: mnemo open PATH", file=sys.stderr); return 2
    print(vault.read(args[0])["raw"])


def cmd_reindex(args):
    print(f"indexed {index.reindex()} notes")


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
    out = Path(args[args.index("--out") + 1]) if "--out" in args else Path("mnemo-export")
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
    """Sync with a peer mnemo. Usage: mnemo sync PEER_URL [--watch] [--interval N] [--token T]"""
    import time as _t
    from server import syncclient
    if not args or args[0].startswith("--"):
        print("usage: mnemo sync PEER_URL [--watch] [--interval N] [--token T]", file=sys.stderr)
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


COMMANDS = {"new": cmd_new, "daily": cmd_daily, "capture": cmd_capture,
            "search": cmd_search, "ls": cmd_ls, "open": cmd_open,
            "reindex": cmd_reindex, "serve": cmd_serve, "export": cmd_export,
            "mcp": cmd_mcp, "sync": cmd_sync}


def main(argv=None):
    argv = argv if argv is not None else sys.argv[1:]
    if not argv or argv[0] in ("-h", "--help", "help"):
        print(__doc__); return 0
    cmd = argv[0]
    if cmd not in COMMANDS:
        print(f"unknown command {cmd!r}. Try: {', '.join(COMMANDS)}", file=sys.stderr); return 2
    if cmd != "serve":
        _ready()
    return COMMANDS[cmd](argv[1:]) or 0


if __name__ == "__main__":
    sys.exit(main())
