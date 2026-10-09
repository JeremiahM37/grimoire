#!/usr/bin/env python3
"""Reports what an agent did after Grimoire injected memories (adherence).

Handles claude-json-style hook events:

* PostToolUse - sends the tool name and its target (the command or path), so
  the server can test the checks stored on the memories shown to this session.
* Stop        - reads the transcript, extracts ONLY the memory tags (m:3e99)
  the agent cited in its last turn, and sends those. Transcript text is never
  sent anywhere.
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


def run(event, environment=None, send=post):
    environment = os.environ if environment is None else environment
    if environment.get("GRIMOIRE_AUTO_CONTEXT", "1") == "0" or environment.get("GRIMOIRE_OUTCOME", "1") == "0":
        return None
    name = event.get("hook_event_name", "")
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
    if name == "Stop":
        tags = []
        path = event.get("transcript_path")
        if isinstance(path, str) and path:
            tags = cited_tags(path)
        body = {"session": sid, "stop": True}
        if tags:
            body["cited"] = tags
        send(base, token, body)
        return None
    tool = event.get("tool_name", "")
    if tool not in ACTION_TOOLS:
        return None
    target = action_target(tool, event.get("tool_input", {}))
    if not target.strip() or len(target.encode()) > 4000:
        return None
    if name == "PostToolUse":
        send(base, token, {"session": sid, "tool": tool, "target": target})
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


def main():
    try:
        raw = sys.stdin.buffer.read(65001)
        if len(raw) > 65000:
            return
        event = json.loads(raw)
        if not isinstance(event, dict):
            return
        result = run(event)
        if result:
            print(json.dumps(result))
    except (OSError, ValueError, TypeError, OverflowError):
        if os.environ.get("GRIMOIRE_CONTEXT_DEBUG") == "1":
            print("Grimoire outcome reporting unavailable.", file=sys.stderr)


if __name__ == "__main__":
    main()
