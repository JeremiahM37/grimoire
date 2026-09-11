#!/usr/bin/env python3
"""Bounded, model-free context for Claude Code and Codex command hooks."""

import hashlib
import json
import os
import re
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def fingerprint(value):
    return hashlib.sha256(value.encode()).hexdigest()


def read_state(path):
    try:
        raw = path.read_bytes()
        if len(raw) > 32000:
            return {}
        value = json.loads(raw)
        return value if isinstance(value, dict) else {}
    except (OSError, ValueError):
        return {}


def write_state(path, state):
    temporary = path.with_suffix(f".{os.getpid()}.tmp")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "w") as output:
            json.dump(state, output)
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


def fetch_context(base, token, query, excluded, budget, mode, paths):
    parameters = urllib.parse.urlencode({
        "q": query, "exclude": ",".join(excluded), "max_bytes": budget, "limit": 5,
        "scope": mode, "path": paths,
    }, doseq=True)
    headers = {"Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    request = urllib.request.Request(base + "/api/memory/context?" + parameters, headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=1.5) as response:
        raw = response.read(64001)
    if len(raw) > 64000:
        raise ValueError("oversized context response")
    result = json.loads(raw)
    if not isinstance(result, dict):
        raise ValueError("invalid context response")
    return result


def run(event, environment=None, fetch=fetch_context, now=None):
    environment = os.environ if environment is None else environment
    now = time.time() if now is None else now
    if environment.get("GRIMOIRE_AUTO_CONTEXT", "1") == "0":
        return None
    mode = environment.get("GRIMOIRE_CONTEXT_MODE", "manual")
    if mode not in {"all", "scoped"}:
        return None
    paths = json.loads(environment.get("GRIMOIRE_CONTEXT_PATHS", "[]"))
    if (not isinstance(paths, list) or len(paths) > 32
            or any(not isinstance(path, str) or len(path) > 512 for path in paths)
            or (mode == "scoped" and not paths) or (mode == "all" and paths)):
        return None
    event_name = event.get("hook_event_name", "")
    if event_name not in {"SessionStart", "UserPromptSubmit"}:
        return None
    session = event.get("session_id", "")
    if not isinstance(session, str) or not session or len(session) > 256:
        return None
    base = environment.get("GRIMOIRE_URL", "http://127.0.0.1:9111").rstrip("/")
    parsed = urllib.parse.urlsplit(base)
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        return None
    if parsed.scheme != "https" and not (
        parsed.scheme == "http" and parsed.hostname in {"localhost", "127.0.0.1", "::1"}
    ):
        return None
    token = environment.get("GRIMOIRE_AUTH_TOKEN", "")
    cwd = str(event.get("cwd", ""))
    identity = fingerprint(base + "\0" + fingerprint(token) + "\0" + cwd + "\0" + session
                           + "\0" + mode + json.dumps(paths))
    directory = Path(environment.get(
        "GRIMOIRE_CONTEXT_STATE_DIR", str(Path.home() / ".cache" / "grimoire" / "context")
    ))
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    state_path = directory / (identity + ".json")
    lock = directory / (identity + ".lock")
    try:
        lock.mkdir(mode=0o700)
    except FileExistsError:
        if now - lock.stat().st_mtime > 60:
            try:
                lock.rmdir()
            except OSError:
                pass
        return None
    try:
        state = read_state(state_path)
        if event_name == "SessionStart":
            if event.get("source") in {"compact", "clear", "resume"}:
                write_state(state_path, {})
            return None
        prompt = event.get("prompt", "")
        if not isinstance(prompt, str) or len(prompt.encode()) > 8000:
            return None
        prompt = prompt.strip()
        if not prompt or re.fullmatch(
            r"(?:thanks?|thank you|ok(?:ay)?|yes|no|continue|proceed|hi|hello)[.!\s]*",
            prompt, flags=re.IGNORECASE,
        ):
            return None
        query = environment.get("GRIMOIRE_CONTEXT_QUERY_PREFIX", "") + " " + prompt
        query = query.strip()
        query_hash = fingerprint(query)
        if state.get("query") == query_hash and now - state.get("checked", 0) < 30:
            return None
        budget = max(128, min(8000, int(environment.get("GRIMOIRE_CONTEXT_MAX_BYTES", "2400"))))
        seen = state.get("seen", {})
        if not isinstance(seen, dict):
            seen = {}
        seen = {key: stamp for key, stamp in seen.items()
                if isinstance(key, str) and isinstance(stamp, (float, int))
                and now - stamp < 1800}
        excluded = list(seen)[-256:]
        result = fetch(base, token, query, excluded, budget, mode, paths)
        context = result.get("context", "")
        keys = result.get("keys", [])
        if (not isinstance(context, str) or len(context.encode()) > budget
                or not isinstance(keys, list) or len(keys) > 10
                or any(not isinstance(key, str) or not re.fullmatch(r"[a-f0-9]{32}", key)
                       for key in keys)):
            return None
        for key in keys:
            seen[key] = now
        seen = dict(list(seen.items())[-256:])
        write_state(state_path, {"query": query_hash, "checked": now, "seen": seen})
        if not context:
            return None
        return {"hookSpecificOutput": {
            "hookEventName": "UserPromptSubmit", "additionalContext": context,
        }}
    finally:
        lock.rmdir()


def main():
    try:
        raw = sys.stdin.buffer.read(16001)
        if len(raw) > 16000:
            return
        event = json.loads(raw)
        if not isinstance(event, dict):
            return
        result = run(event)
        if result:
            print(json.dumps(result))
    except (OSError, ValueError, TypeError, OverflowError):
        if os.environ.get("GRIMOIRE_CONTEXT_DEBUG") == "1":
            print("Grimoire automatic context unavailable; explicit MCP lookup remains available.",
                  file=sys.stderr)


if __name__ == "__main__":
    main()
