#!/usr/bin/env python3
"""Merge the Grimoire context hook into a Claude Code or Codex hooks file.

    install.py ~/.claude/settings.json            # show the diff, change nothing
    install.py ~/.codex/hooks.json --apply        # merge, after a timestamped backup

Both hosts use the same shape: ``{"hooks": {Event: [{"hooks": [{...}]}]}}``.
Existing entries are never reordered, edited or removed; this only appends a
matcher group for ``SessionStart`` and ``UserPromptSubmit`` when no command
there already runs ``grimoire_context.py``, so running it twice is a no-op.
The file is re-serialised as 2-space JSON, which the diff shows up front.

The hook stays inert until ``GRIMOIRE_CONTEXT_MODE`` is ``all`` or ``scoped``;
``--mode`` / ``--env`` bake it into the command so no launcher change is needed.
Standard library only.
"""

import argparse
import difflib
import json
import os
import shlex
import sys
import tempfile
import time
from pathlib import Path

EVENTS = ("SessionStart", "UserPromptSubmit")
SCRIPT = "grimoire_context.py"
DEFAULT_HOOK = Path(__file__).resolve().parent / SCRIPT


def build_command(hook, python, environment):
    prefix = [f"{name}={shlex.quote(value)}" for name, value in environment]
    return " ".join(prefix + [shlex.quote(python), shlex.quote(str(hook))])


def already_installed(groups):
    return any(
        isinstance(group, dict) and any(
            isinstance(handler, dict) and SCRIPT in str(handler.get("command", ""))
            for handler in group.get("hooks", []) if isinstance(group.get("hooks"), list)
        )
        for group in groups
    )


def merge(config, command, timeout):
    """A new config with the entries added, plus the events that changed."""
    if not isinstance(config, dict):
        raise ValueError("top level of the file is not a JSON object")
    merged = json.loads(json.dumps(config))
    hooks = merged.setdefault("hooks", {})
    if not isinstance(hooks, dict):
        raise ValueError('"hooks" is not an object')
    changed = []
    for event in EVENTS:
        groups = hooks.setdefault(event, [])
        if not isinstance(groups, list):
            raise ValueError(f'"hooks.{event}" is not a list')
        if already_installed(groups):
            continue
        groups.append({"hooks": [{"type": "command", "command": command, "timeout": timeout}]})
        changed.append(event)
    return merged, changed


def render(config):
    return json.dumps(config, indent=2, ensure_ascii=False) + "\n"


def load(path):
    if not path.exists():
        return {}
    text = path.read_text()
    return json.loads(text) if text.strip() else {}


def write_atomic(path, text):
    mode = path.stat().st_mode & 0o777 if path.exists() else 0o600
    descriptor, temporary = tempfile.mkstemp(dir=path.parent, prefix=path.name + ".")
    try:
        with os.fdopen(descriptor, "w") as output:
            output.write(text)
        os.chmod(temporary, mode)
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("target", type=Path, help=".claude/settings.json or .codex/hooks.json")
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--dry-run", action="store_true", help="print the diff only (default)")
    mode.add_argument("--apply", action="store_true", help="back up and write the merged file")
    parser.add_argument("--hook", type=Path, default=DEFAULT_HOOK, help="path to grimoire_context.py")
    parser.add_argument("--python", default="python3", help="interpreter named in the command")
    parser.add_argument("--timeout", type=int, default=3, help="hook timeout, seconds")
    parser.add_argument("--mode", choices=["all", "scoped"], help="sets GRIMOIRE_CONTEXT_MODE")
    parser.add_argument("--env", action="append", default=[], metavar="NAME=VALUE",
                        help="environment baked into the command (repeatable); no secrets")
    args = parser.parse_args(argv)

    environment = []
    if args.mode:
        environment.append(("GRIMOIRE_CONTEXT_MODE", args.mode))
    for item in args.env:
        name, separator, value = item.partition("=")
        if not separator or not name.isidentifier():
            parser.error(f"--env expects NAME=VALUE, got {item!r}")
        if "TOKEN" in name.upper() or "SECRET" in name.upper():
            parser.error("do not write credentials into a hooks file; set them in the launcher")
        environment.append((name, value))

    target = args.target.expanduser()
    hook = args.hook.expanduser().resolve()
    if not hook.is_file():
        print(f"error: hook script not found: {hook}", file=sys.stderr)
        return 2
    try:
        config = load(target)
        merged, changed = merge(config, build_command(hook, args.python, environment), args.timeout)
    except (OSError, ValueError) as error:
        print(f"error: cannot merge into {target}: {error}", file=sys.stderr)
        return 2

    if not changed:
        print(f"{target}: Grimoire hook already present for {', '.join(EVENTS)}; nothing to do")
        return 0
    before = render(config) if target.exists() else ""
    diff = "".join(difflib.unified_diff(
        before.splitlines(True), render(merged).splitlines(True),
        fromfile=str(target), tofile=str(target) + " (merged)"))
    print(diff, end="")
    if not args.apply:
        print(f"\ndry run: would add {', '.join(changed)}. Re-run with --apply to write.")
        return 0
    if target.exists():
        backup = target.with_name(f"{target.name}.grimoire-backup-{time.strftime('%Y%m%dT%H%M%S')}")
        backup.write_bytes(target.read_bytes())
        print(f"backup: {backup}")
    else:
        target.parent.mkdir(parents=True, exist_ok=True)
    write_atomic(target, render(merged))
    print(f"wrote {target}: added {', '.join(changed)}. Start a new session; the host may ask "
          "you to trust the hook.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
