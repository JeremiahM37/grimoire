#!/usr/bin/env python3
"""Bounded, model-free context for any agent with command hooks.

Claude Code is the default shape. `--agent NAME` (or GRIMOIRE_AGENT_PROFILE)
loads a profile written by `grimoire agent install` (~/.grimoire/agents/NAME.json)
that says what the agent calls its events, where its payload fields are, and how
it wants context handed back; with no profile this behaves as it always has.
`--event prompt|action|session_start` names the event when the agent does not
put it in the payload."""

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


FP_SPEC = 1
FP_MAX_ITEMS = 24


def store_fingerprints(directory, sid, result):
    """Keep the salted fingerprint hashes the server sent with an injection, so
    grimoire_outcome.py can tell whether the agent used each memory without a
    tag. Only hashes are stored, never memory text. The file also carries the
    turn counter that hook advances at every Stop."""
    fp = result.get("fp")
    if not isinstance(fp, dict) or fp.get("v") != FP_SPEC or not isinstance(fp.get("salt"), str):
        return
    items = fp.get("items")
    if not isinstance(items, dict) or not items or len(items) > 10:
        return
    path = directory / ("fp-" + sid + ".json")
    state = read_state(path)
    if state.get("v") != FP_SPEC or not isinstance(state.get("items"), list):
        state = {"v": FP_SPEC, "turn": 0, "items": []}
    keep = [item for item in state["items"] if isinstance(item, dict) and item.get("tag") not in items]
    for tag, hashes in items.items():
        if (not isinstance(tag, str) or not re.fullmatch(r"[0-9a-f]{4,8}", tag) or not isinstance(hashes, list)
                or not hashes or len(hashes) > 8
                or any(not isinstance(h, str) or not re.fullmatch(r"[0-9a-f]{10}", h) for h in hashes)):
            continue
        keep.append({"tag": tag, "salt": fp["salt"][:32], "hashes": hashes,
                     "turn": state.get("turn", 0), "tools": 0, "m": []})
    state["items"] = keep[-FP_MAX_ITEMS:]
    try:
        write_state(path, state)
    except OSError:
        pass


def write_state(path, state):
    temporary = path.with_suffix(f".{os.getpid()}.tmp")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "w") as output:
            json.dump(state, output)
        os.replace(temporary, path)
    finally:
        temporary.unlink(missing_ok=True)


DEFAULT_PROFILE = {
    "name": "claude-code",
    "hooks": {"events": {"session_start": "SessionStart", "prompt": "UserPromptSubmit",
                         "pre_action": "PreToolUse"}},
    "event_fields": {"event": "hook_event_name", "prompt": "prompt", "tool": "tool_name",
                     "tool_input": "tool_input", "session": "session_id", "cwd": "cwd",
                     "source": "source"},
    "delegation_tools": ["Agent", "Task"],
    "output": "claude-json",
}
KINDS = ("session_start", "prompt", "pre_action")
KIND_ALIASES = {"action": "pre_action", "pre-action": "pre_action", "session-start": "session_start"}
ACTION_TOOLS = {"Bash": "command", "Edit": "file_path", "MultiEdit": "file_path",
                "Write": "file_path", "NotebookEdit": "notebook_path",
                "Agent": "prompt", "Task": "prompt"}
DEFAULT_PROFILE["actions"] = ACTION_TOOLS


def load_profile(name, environment):
    """The resolved profile `grimoire agent install` wrote for NAME, or the
    Claude Code shape when there is none (never an error: a hook that fails
    loudly stops the agent's turn)."""
    if not name or not re.fullmatch(r"[a-z][a-z0-9_-]{0,31}", name):
        return DEFAULT_PROFILE
    directory = environment.get("GRIMOIRE_AGENT_DIR") or str(Path.home() / ".grimoire" / "agents")
    try:
        raw = (Path(directory) / (name + ".json")).read_bytes()
        if len(raw) > 64000:
            return DEFAULT_PROFILE
        value = json.loads(raw)
    except (OSError, ValueError):
        return DEFAULT_PROFILE
    if not isinstance(value, dict):
        return DEFAULT_PROFILE
    merged = dict(DEFAULT_PROFILE)
    merged.update({key: item for key, item in value.items() if item not in (None, {}, "")})
    return merged


def lookup(source, path):
    """Value at a dotted path ("tool_input.command"); alternatives separated by |."""
    for option in str(path).split("|"):
        value = source
        for part in option.split("."):
            value = value.get(part) if isinstance(value, dict) else None
        if isinstance(value, list) and all(isinstance(v, str) for v in value):
            value = " ".join(value)
        if value not in (None, "", {}):
            return value
    return None


def normalise(event, profile, forced=None):
    """The event in one shape whatever the agent: kind, session, cwd, prompt,
    tool, tool_input, source, native (the agent's own event name)."""
    fields = profile.get("event_fields") or DEFAULT_PROFILE["event_fields"]
    events = (profile.get("hooks") or {}).get("events") or DEFAULT_PROFILE["hooks"]["events"]
    native = lookup(event, fields.get("event", "hook_event_name"))
    kind = KIND_ALIASES.get(forced, forced)
    if kind not in KINDS:
        kind = next((k for k in KINDS if events.get(k) and events.get(k) == native), None)
    if kind is None:
        return None
    session = lookup(event, fields.get("session", "session_id"))
    cwd = lookup(event, fields.get("cwd", "cwd")) or ""
    return {"kind": kind, "native": native or events.get(kind, ""), "session": session,
            "cwd": str(cwd), "source": lookup(event, fields.get("source", "source")),
            "prompt": lookup(event, fields.get("prompt", "prompt")) if kind == "prompt" else "",
            "tool": lookup(event, fields.get("tool", "tool_name")) or "",
            "tool_input": lookup(event, fields.get("tool_input", "tool_input")),
            "tool_use_id": tool_use_id(event, fields)}


TU_FIELDS = ("tool_use_id", "toolUseId", "tool_call_id", "call_id", "tool_use.id")


def tool_use_id(event, fields):
    """The harness's id for a tool call, when its payload has one. It pairs the
    memory shown for a pending call with that call's outcome (docs/MEMORY_TRACE.md)."""
    for path in ([fields["tool_use_id"]] if fields.get("tool_use_id") else []) + list(TU_FIELDS):
        value = lookup(event, path)
        if isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_\-:.]{1,128}", value):
            return value
    return None


def edit_content_hash(tool, tool_input, delegation=()):
    """A short hash of what an edit call changes, or None for a call that is not
    an edit. The pending-action experiment hashes the tool and target; for Edit,
    MultiEdit and Write the target is only the path, so without this a different
    edit of the same file would read as unchanged. The context hook (before the
    call) and the outcome hook (after it) must compute the same value, so keep
    the two copies identical. Only the hash leaves the machine."""
    if not isinstance(tool_input, dict) or tool in delegation or tool.lower() in {"bash", "shell"}:
        return None

    def text(value):
        return value if isinstance(value, str) else ""

    if isinstance(tool_input.get("edits"), list):
        material = ["edits", [[text(e.get("old_string")), text(e.get("new_string"))]
                              for e in tool_input["edits"] if isinstance(e, dict)]]
    elif "old_string" in tool_input or "new_string" in tool_input:
        material = ["edit", text(tool_input.get("old_string")), text(tool_input.get("new_string")),
                    bool(tool_input.get("replace_all"))]
    else:
        material = None
        for key in ("content", "new_source", "file_text"):
            if isinstance(tool_input.get(key), str):
                material = ["write", tool_input[key]]
                break
        if material is None:
            for key in ("input", "patch", "command"):
                value = tool_input.get(key)
                if isinstance(value, str) and re.search(r"^(?:\*\*\* (?:Update|Add|Delete) File: |\+\+\+ (?:b/)?)\S", value, re.M):
                    material = ["patch", value]
                    break
        if material is None:
            return None
    raw = json.dumps(material, ensure_ascii=True, separators=(",", ":")).encode("utf-8", "replace")
    return hashlib.sha256(b"edit\0" + raw).hexdigest()[:16]


def ask_reason(result):
    """The reason of a server `permission` of decision "ask", else None.
    Grimoire may only ask; allow and deny are never honoured."""
    permission = result.get("permission") if isinstance(result, dict) else None
    if not isinstance(permission, dict) or permission.get("decision") != "ask":
        return None
    reason = permission.get("reason")
    if not isinstance(reason, str) or not reason.strip():
        return None
    return reason.strip()[:1000]


def render(profile, norm, context, ask=None):
    """The hook's output for context: a JSON object or plain text, per profile.
    `ask` is a permission reason for an `enforce: ask` rule: structured agents
    get permissionDecision "ask" (Claude Code and Codex field names); plain
    agents get the reason printed ahead of the context."""
    if profile.get("output") == "plain-stdout":
        return "\n".join(part for part in (ask, context) if part)
    name = {"prompt": "UserPromptSubmit", "pre_action": "PreToolUse"}[norm["kind"]]
    events = (profile.get("hooks") or {}).get("events") or {}
    out = {"hookEventName": events.get(norm["kind"]) or name}
    if context:
        out["additionalContext"] = context
    if ask:
        out["permissionDecision"] = "ask"
        out["permissionDecisionReason"] = ask
    return {"hookSpecificOutput": out}


def fetch_context(base, token, query, excluded, budget, mode, paths, extra=None):
    values = {
        "q": query, "exclude": ",".join(excluded), "max_bytes": budget, "limit": 5,
        "scope": mode, "path": paths,
    }
    values.update(extra or {})
    parameters = urllib.parse.urlencode(values, doseq=True)
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


def run(event, environment=None, fetch=fetch_context, now=None, profile=None, kind=None):
    environment = os.environ if environment is None else environment
    profile = profile or DEFAULT_PROFILE
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
    norm = normalise(event, profile, kind)
    if norm is None:
        return None
    kind = norm["kind"]
    if kind == "pre_action" and environment.get("GRIMOIRE_CONTEXT_ACTIONS", "1") == "0":
        return None
    session = norm["session"]
    if (not session and profile.get("session_fallback")
            and kind in {"prompt", "pre_action"}):
        session = environment.get("GRIMOIRE_SESSION") or "cwd:" + norm["cwd"]
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
    cwd = norm["cwd"]
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
        if kind == "session_start":
            if norm["source"] in {"compact", "clear", "resume"}:
                write_state(state_path, {})
            return None
        if kind == "pre_action":
            return action_context(norm, environment, fetch, now, state, state_path,
                                  base, token, mode, paths, profile)
        prompt = norm["prompt"]
        if not isinstance(prompt, str) or len(prompt.encode()) > 8000:
            return None
        prompt = prompt.strip()
        # Automated turns (background-task notifications, system reminders)
        # arrive as prompts too; they are not requests and draw only noise.
        if AUTOMATED_PROMPT.match(prompt):
            return None
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
        # The session travels as a hash: the server pairs consecutive prompts
        # to learn from re-tells, and never needs to know which session it is.
        extra = rank_params(environment)
        extra["session"] = fingerprint("session\0" + session)[:32]
        result = fetch(base, token, query, excluded, budget, mode, paths, extra)
        context = result.get("context", "")
        keys = result.get("keys", [])
        if (not isinstance(context, str) or len(context.encode()) > budget
                or not isinstance(keys, list) or len(keys) > 10
                or any(not isinstance(key, str) or not re.fullmatch(r"[a-f0-9]{32}", key)
                       for key in keys)):
            return None
        for key in keys:
            seen[key] = now
        if environment.get("GRIMOIRE_FINGERPRINTS", "1") != "0":
            store_fingerprints(directory, extra["session"], result)
        seen = dict(list(seen.items())[-256:])
        state.update({"query": query_hash, "checked": now, "seen": seen})
        write_state(state_path, state)
        if not context:
            return None
        return render(profile, norm, context)
    finally:
        lock.rmdir()


AUTOMATED_PROMPT = re.compile(r"\s*(\[SYSTEM NOTIFICATION|<task-notification>|<system-reminder>)", re.IGNORECASE)


def rank_params(environment):
    """Ranking for the server: hybrid (embeddings + keywords + cues) unless the
    operator pins the old word-overlap ranking."""
    rank = environment.get("GRIMOIRE_CONTEXT_RANK", "hybrid")
    if rank not in {"hybrid", "lexical"}:
        rank = "hybrid"
    extra = {"rank": rank}
    if rank == "hybrid":
        extra["min_rel"] = environment.get("GRIMOIRE_CONTEXT_MIN_REL", "0.5")
    return extra


def action_text(tool, tool_input, profile=None):
    """The text an action is matched on. A subagent launch is described by
    what it is for and which model runs it, not by its whole brief: standing
    rules about delegation ("the lead plans, a cheaper model implements") are
    about exactly those two things."""
    profile = profile or DEFAULT_PROFILE
    if tool in profile.get("delegation_tools", ["Agent", "Task"]):
        parts = ["launch subagent", str(tool_input.get("subagent_type", "") or ""),
                 "model " + str(tool_input.get("model", "") or "default"),
                 str(tool_input.get("description", "") or ""),
                 str(tool_input.get("prompt", "") or "")[:300]]
        return " ".join(p for p in parts if p.strip())
    return str(lookup(tool_input, (profile.get("actions") or ACTION_TOOLS)[tool]) or "")


def action_context(norm, environment, fetch, now, state, state_path, base, token, mode, paths,
                   profile=None):
    """PreToolUse: the memories that bear on the command or file about to be
    touched, at the moment the agent acts. A rule read twenty turns ago is the
    one most likely to be ignored; restating it as the action happens is what
    makes it stick. Held to a stricter bar and a smaller budget than prompt
    context, because it runs on every tool call."""
    profile = profile or DEFAULT_PROFILE
    tool = norm["tool"]
    tool_input = norm["tool_input"]
    if tool not in (profile.get("actions") or ACTION_TOOLS) or not isinstance(tool_input, dict):
        return None
    target = action_text(tool, tool_input, profile)
    if not target.strip() or len(target.encode()) > 4000:
        return None
    query = (tool + " " + target.strip())[:2000]
    recent = state.get("actions", {})
    if not isinstance(recent, dict):
        recent = {}
    ttl = 600
    recent = {key: stamp for key, stamp in recent.items()
              if isinstance(key, str) and isinstance(stamp, (float, int)) and now - stamp < ttl}
    budget = max(128, min(4000, int(environment.get("GRIMOIRE_ACTION_MAX_BYTES", "1200"))))
    extra = rank_params(environment)
    if extra["rank"] == "hybrid":
        # 0.7, not 0.6: at 0.6, 44% of off-target commands drew a reminder;
        # at 0.7, 16%, for 3 points fewer right ones (docs/MEMORY_USE.md).
        extra["min_rel"] = environment.get("GRIMOIRE_ACTION_MIN_REL", "0.7")
        # A file path is topical: every file in a repo resembles every memory
        # about that repo. Edits need a near-exact match (4% off-target at 0.8).
        if tool.lower() not in {"bash", "shell"} and tool not in profile.get("delegation_tools", []):
            extra["min_rel"] = environment.get("GRIMOIRE_EDIT_MIN_REL", "0.8")
    extra.update({"limit": 2, "stage": "action"})
    # The session hash lets the server log action-stage injections, so their
    # fingerprints and outcomes count too; the server keeps tool calls out of
    # its prompt pairing.
    if norm.get("session"):
        extra["session"] = fingerprint("session\0" + norm["session"])[:32]
        if norm.get("tool_use_id") and environment.get("GRIMOIRE_TRACE", "1") != "0":
            extra["tu"] = norm["tool_use_id"]
        if environment.get("GRIMOIRE_TRACE", "1") != "0":
            ec = edit_content_hash(tool, tool_input, profile.get("delegation_tools") or [])
            if ec:
                extra["ec"] = ec
    result = fetch(base, token, query, list(recent)[-128:], budget, mode, paths, extra)
    context = result.get("context", "")
    keys = result.get("keys", [])
    if (not isinstance(context, str) or len(context.encode()) > budget
            or not isinstance(keys, list) or len(keys) > 2
            or any(not isinstance(key, str) or not re.fullmatch(r"[a-f0-9]{32}", key) for key in keys)):
        return None
    for key in keys:
        recent[key] = now
    state["actions"] = dict(list(recent.items())[-128:])
    write_state(state_path, state)
    if extra.get("session") and environment.get("GRIMOIRE_FINGERPRINTS", "1") != "0":
        store_fingerprints(state_path.parent, extra["session"], result)
    ask = ask_reason(result)
    if not context and not ask:
        return None
    return render(profile, norm, context, ask)


def main(argv=None, environment=None):
    argv = sys.argv[1:] if argv is None else argv
    environment = os.environ if environment is None else environment
    agent, kind = environment.get("GRIMOIRE_AGENT_PROFILE", ""), None
    for index, argument in enumerate(argv):
        if argument == "--agent" and index + 1 < len(argv):
            agent = argv[index + 1]
        elif argument.startswith("--agent="):
            agent = argument[8:]
        elif argument == "--event" and index + 1 < len(argv):
            kind = argv[index + 1]
        elif argument.startswith("--event="):
            kind = argument[8:]
    try:
        raw = sys.stdin.buffer.read(16001)
        if len(raw) > 16000:
            return
        event = json.loads(raw)
        if not isinstance(event, dict):
            return
        result = run(event, environment, profile=load_profile(agent, environment), kind=kind)
        if isinstance(result, str):
            print(result)
        elif result:
            print(json.dumps(result))
    except (OSError, ValueError, TypeError, OverflowError):
        if os.environ.get("GRIMOIRE_CONTEXT_DEBUG") == "1":
            print("Grimoire automatic context unavailable; explicit MCP lookup remains available.",
                  file=sys.stderr)


if __name__ == "__main__":
    main()
