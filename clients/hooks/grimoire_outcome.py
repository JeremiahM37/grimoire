#!/usr/bin/env python3
"""Reports what an agent did after Grimoire injected memories (adherence).

Handles claude-json-style hook events:

* PostToolUse - sends the tool name and its target (the command or path), so
  the server can test the checks stored on the memories shown to this session.
* Stop        - reads the transcript, extracts ONLY the memory tags (m:3e99)
  the agent cited in its last turn, and sends those. Transcript text is never
  sent anywhere.

Tag-free use detection (docs/MEMORY_ADHERENCE.md, "Fingerprints"): the context
hook stores, per session, salted hashes of each injected memory's distinctive
tokens (paths, hosts, ports, flags, env vars, versions, rare identifiers). Here
the tool target (PostToolUse) and the turn's assistant text (Stop) are
normalised by the spec the server publishes, every candidate token is hashed
the same way, and ONLY {tag: number of fingerprints matched} is sent. Neither
the preimages nor any text leave the machine.
* PreToolUse  - optional (GRIMOIRE_OUTCOME_ENFORCE=1): asks the server whether
  a rule marked `enforce: ask` forbids the action and, if so, answers with a
  permission decision so the agent harness asks the user first. Normally the
  context hook's action response already carries this; see
  docs/MEMORY_ADHERENCE.md for the two ways to wire it.

Fails silently: a missing server must never disturb the agent.
"""

import hashlib
import json
import os
import re
import sys
import urllib.parse
import urllib.request

FP_RUN = re.compile(r"[a-z0-9_./:~@%+#$=\-]+")
FP_SPEC = 1
FP_LOWER = {c: c + 32 for c in range(65, 91)}
FP_MAX_ITEMS = 24
# Use window: an injected memory is watched through the turn it arrived in and
# this many turns after it. A tool-call bound is optional (0 = none).
FP_WINDOW_TURNS = 1
FP_WINDOW_TOOLS = 0
TAG = re.compile(r"\bm:([0-9a-f]{4,8})\b")
ACTION_TOOLS = {"Bash": "command", "Edit": "file_path", "MultiEdit": "file_path",
                "Write": "file_path", "NotebookEdit": "notebook_path",
                "Agent": "prompt", "Task": "prompt"}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def session_hash(session):
    """The same opaque session id the context hook sends."""
    return hashlib.sha256(("session\0" + session).encode()).hexdigest()[:32]


def action_target(tool, tool_input):
    if not isinstance(tool_input, dict):
        return ""
    if tool in {"Agent", "Task"}:
        parts = [str(tool_input.get("subagent_type", "") or ""),
                 "model " + str(tool_input.get("model", "") or "default"),
                 str(tool_input.get("description", "") or ""),
                 str(tool_input.get("prompt", "") or "")[:300]]
        return " ".join(p for p in parts if p.strip())
    return str(tool_input.get(ACTION_TOOLS[tool], "") or "")


def _texts(content):
    if isinstance(content, str):
        return [content]
    out = []
    if isinstance(content, list):
        for block in content:
            if isinstance(block, dict) and block.get("type") == "text" and isinstance(block.get("text"), str):
                out.append(block["text"])
    return out


def _is_prompt(entry):
    """A user entry that is a person's prompt, not a tool result."""
    content = (entry.get("message") or {}).get("content")
    if isinstance(content, str):
        return bool(content.strip())
    if isinstance(content, list):
        return any(isinstance(b, dict) and b.get("type") == "text" for b in content)
    return False


def cited_tags(transcript_path, limit=524288):
    """Memory tags the agent cited since the last user prompt. Only tags leave
    this function; the text it read does not."""
    try:
        size = os.path.getsize(transcript_path)
        with open(transcript_path, "rb") as handle:
            if size > limit:
                handle.seek(size - limit)
                handle.readline()  # drop the partial first line
            lines = handle.read().decode("utf-8", "replace").splitlines()
    except OSError:
        return []
    found = []
    for line in reversed(lines):
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        if not isinstance(entry, dict):
            continue
        kind = entry.get("type")
        if kind == "user" and _is_prompt(entry):
            break
        if kind == "assistant":
            for text in _texts((entry.get("message") or {}).get("content")):
                for tag in TAG.findall(text):
                    if tag not in found:
                        found.append(tag)
    return found[:32]


# ---- fingerprints ----------------------------------------------------------
# candidates() is the normalisation spec (go/internal/fingerprint, SpecVersion
# 1). clients/hooks/tests/fingerprint_vectors.json is run by both the Go and
# the Python suites, so the two cannot drift.


def _fp_clean(run):
    return run.lstrip(":=+#%@").rstrip(".:=-/+#%@")


def _fp_expand(run):
    if len(run) < 3 or len(run) > 160:
        return []
    out = [run]

    def add(piece):
        piece = _fp_clean(piece)
        if len(piece) >= 3:
            out.append(piece)

    if run.startswith("$"):
        add(run[1:])
    for piece in run.split("="):
        if piece != run:
            add(piece)
    if "://" in run:
        rest = run.split("://", 1)[1]
        auth, _, path = rest.partition("/")
        if "@" in auth:
            auth = auth.rsplit("@", 1)[1]
        add(auth)
        if ":" in auth:
            add(auth.split(":", 1)[0])
        for piece in path.split("/"):
            add(piece)
    elif "/" in run:
        for piece in run.split("/"):
            add(piece)
    elif ":" in run:
        for piece in run.split(":"):
            add(piece)
    return list(dict.fromkeys(out))


def candidates(text, limit=4000):
    """Every token in text that could be a fingerprint: lowercase, distinct."""
    seen = {}
    for run in FP_RUN.findall(text.translate(FP_LOWER)):
        for token in _fp_expand(_fp_clean(run)):
            seen.setdefault(token, None)
            if len(seen) >= limit:
                return list(seen)
    return list(seen)


def fp_hash(salt, token):
    return hashlib.sha256((salt + "\0" + token).encode()).hexdigest()[:10]


def fp_path(environment, sid):
    directory = environment.get("GRIMOIRE_CONTEXT_STATE_DIR") or os.path.join(
        os.path.expanduser("~"), ".cache", "grimoire", "context")
    return os.path.join(directory, "fp-" + sid + ".json")


def fp_read(path):
    try:
        with open(path, "rb") as handle:
            raw = handle.read(65537)
        if len(raw) > 65536:
            return None
        state = json.loads(raw)
    except (OSError, ValueError):
        return None
    if not isinstance(state, dict) or state.get("v") != FP_SPEC or not isinstance(state.get("items"), list):
        return None
    return state


def fp_write(path, state):
    temporary = "%s.%d.tmp" % (path, os.getpid())
    try:
        descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(descriptor, "w") as output:
            json.dump(state, output)
        os.replace(temporary, path)
    except OSError:
        try:
            os.unlink(temporary)
        except OSError:
            pass


def _int_env(environment, name, default):
    try:
        return max(0, int(environment.get(name, default)))
    except (TypeError, ValueError):
        return default


def fp_live(state, environment):
    """The injected items still inside their use window."""
    turns = _int_env(environment, "GRIMOIRE_FP_WINDOW_TURNS", FP_WINDOW_TURNS)
    tools = _int_env(environment, "GRIMOIRE_FP_WINDOW_TOOLS", FP_WINDOW_TOOLS)
    live = []
    for item in state["items"]:
        if (not isinstance(item, dict) or not isinstance(item.get("hashes"), list)
                or not isinstance(item.get("tag"), str)):
            continue
        if state.get("turn", 0) - item.get("turn", 0) > turns:
            continue
        if tools and item.get("tools", 0) > tools:
            continue
        live.append(item)
    return live


def fp_match(state, environment, text):
    """Hash the candidates of text and record which fingerprints of the live
    items appear. Only hashes are compared."""
    if not text:
        return
    cache = {}
    for item in fp_live(state, environment):
        salt = item.get("salt", "")
        if salt not in cache:
            cache[salt] = {fp_hash(salt, token) for token in candidates(text)}
        found = set(item["hashes"]) & cache[salt]
        if found:
            item["m"] = sorted(set(item.get("m", [])) | found)


def fp_counts(state, environment):
    """{tag: distinct fingerprints matched so far} - all that is ever sent."""
    return {item["tag"]: len(item["m"]) for item in fp_live(state, environment) if item.get("m")}


def turn_text(transcript_path, limit=524288):
    """The assistant's text since the last user prompt. Stays local."""
    try:
        size = os.path.getsize(transcript_path)
        with open(transcript_path, "rb") as handle:
            if size > limit:
                handle.seek(size - limit)
                handle.readline()
            lines = handle.read().decode("utf-8", "replace").splitlines()
    except OSError:
        return ""
    parts = []
    for line in reversed(lines):
        try:
            entry = json.loads(line)
        except ValueError:
            continue
        if not isinstance(entry, dict):
            continue
        kind = entry.get("type")
        if kind == "user" and _is_prompt(entry):
            break
        if kind == "assistant":
            parts.extend(reversed(_texts((entry.get("message") or {}).get("content"))))
    return "\n".join(reversed(parts))


def post(base, token, body):
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    if token:
        headers["Authorization"] = "Bearer " + token
    request = urllib.request.Request(base + "/api/memory/outcome", data=json.dumps(body).encode(),
                                     headers=headers, method="POST")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=1.0) as response:
        raw = response.read(65001)
    result = json.loads(raw)
    return result if isinstance(result, dict) else {}


KIND_NAMES = {"post_action": "PostToolUse", "post-action": "PostToolUse", "stop": "Stop",
              "pre_action": "PreToolUse", "pre-action": "PreToolUse"}


def run(event, environment=None, send=post, kind=None):
    environment = os.environ if environment is None else environment
    if environment.get("GRIMOIRE_AUTO_CONTEXT", "1") == "0" or environment.get("GRIMOIRE_OUTCOME", "1") == "0":
        return None
    # `--event` names the event for agents whose own event names differ.
    name = KIND_NAMES.get(kind) or event.get("hook_event_name", "")
    if name not in {"PostToolUse", "Stop", "PreToolUse"}:
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
    sid = session_hash(session)
    fp_file = fp_path(environment, sid)
    fp_state = fp_read(fp_file) if environment.get("GRIMOIRE_FINGERPRINTS", "1") != "0" else None
    if name == "Stop":
        tags = []
        path = event.get("transcript_path")
        if isinstance(path, str) and path:
            tags = cited_tags(path)
            if fp_state is not None:
                fp_match(fp_state, environment, turn_text(path))
        body = {"session": sid, "stop": True}
        if tags:
            body["cited"] = tags
        if fp_state is not None:
            matched = fp_counts(fp_state, environment)
            if matched:
                body["fp"] = matched
            # The turn is over: age the items, drop those out of window.
            fp_state["turn"] = fp_state.get("turn", 0) + 1
            fp_state["items"] = fp_live(fp_state, environment)[-FP_MAX_ITEMS:]
            fp_write(fp_file, fp_state)
        send(base, token, body)
        return None
    tool = event.get("tool_name", "")
    if tool not in ACTION_TOOLS:
        return None
    target = action_target(tool, event.get("tool_input", {}))
    if not target.strip() or len(target.encode()) > 4000:
        return None
    if name == "PostToolUse":
        body = {"session": sid, "tool": tool, "target": target}
        if fp_state is not None and fp_state["items"]:
            fp_match(fp_state, environment, target)
            for item in fp_live(fp_state, environment):
                item["tools"] = item.get("tools", 0) + 1
            matched = fp_counts(fp_state, environment)
            if matched:
                body["fp"] = matched
            fp_write(fp_file, fp_state)
        send(base, token, body)
        return None
    if environment.get("GRIMOIRE_OUTCOME_ENFORCE", "0") != "1":
        return None
    permission = send(base, token, {"session": sid, "tool": tool, "target": target, "pre": True}).get("permission")
    return permission_output(permission)


def permission_output(permission):
    """The hook output for a server permission decision. Only 'ask' is ever
    honoured: Grimoire may ask the user first, never allow or deny."""
    if not isinstance(permission, dict) or permission.get("decision") != "ask":
        return None
    reason = permission.get("reason")
    if not isinstance(reason, str) or not reason:
        return None
    return {"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "ask",
                                   "permissionDecisionReason": reason[:1000]}}


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    kind = None
    for index, argument in enumerate(argv):
        if argument == "--event" and index + 1 < len(argv):
            kind = argv[index + 1]
        elif argument.startswith("--event="):
            kind = argument[8:]
    try:
        raw = sys.stdin.buffer.read(65001)
        if len(raw) > 65000:
            return
        event = json.loads(raw)
        if not isinstance(event, dict):
            return
        result = run(event, kind=kind)
        if result:
            print(json.dumps(result))
    except (OSError, ValueError, TypeError, OverflowError):
        if os.environ.get("GRIMOIRE_CONTEXT_DEBUG") == "1":
            print("Grimoire outcome reporting unavailable.", file=sys.stderr)


if __name__ == "__main__":
    main()
