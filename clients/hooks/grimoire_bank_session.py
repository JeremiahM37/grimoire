#!/usr/bin/env python3
"""Retain coding-agent session transcripts into a Grimoire memory bank.

A Claude Code / Codex command hook, Python 3 standard library only.

- ``Stop`` and ``SessionEnd``: read the session transcript, keep the user and
  assistant text turns, and retain them into the repository's bank as one
  conversation document, ``session:<session_id>``. The whole transcript is sent
  each time, replacing the stored document, so repeating it is idempotent and
  the server re-extracts only the chunks that changed.
- ``UserPromptSubmit`` (only with ``GRIMOIRE_BANK_RECALL=1``): recall from the
  bank with the prompt and add what comes back as context.

Opt-in (``GRIMOIRE_BANK_SESSIONS=1`` for retain). Loopback HTTP or HTTPS only,
short timeouts, bounded sizes, and fail-open: any problem means the agent
carries on with nothing retained or injected. Nothing secret is printed.
See docs/CODING_AGENTS.md.
"""

import hashlib
import json
import os
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path

MAX_TRANSCRIPT_BYTES = 8_000_000   # read at most the newest 8 MB of a transcript
MAX_TURN_CHARS = 8_000
MAX_CONTENT_BYTES = 1_500_000      # oldest turns are dropped beyond this
MAX_CONTEXT_BYTES = 4_000
BANK_ID = re.compile(r"[a-z0-9][a-z0-9._:-]{0,63}")
INJECTED = re.compile(
    r"<(system-reminder|grimoire[\w-]*|hook_prompt|task-notification|relevant_memories|"
    r"user_feedback)\b[^>]*>.*?</\1>",
    re.DOTALL,
)
CONTEXT = "coding-agent session transcript"


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def debug(environment, message):
    if environment.get("GRIMOIRE_BANK_DEBUG") == "1":
        print("grimoire bank hook: " + message, file=sys.stderr)


def safe_base(environment):
    base = environment.get("GRIMOIRE_URL", "http://127.0.0.1:9111").rstrip("/")
    parsed = urllib.parse.urlsplit(base)
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        return None
    if parsed.scheme == "https" or (
        parsed.scheme == "http" and parsed.hostname in {"localhost", "127.0.0.1", "::1"}
    ):
        return base
    return None


def repo_root(cwd):
    """The nearest directory holding .git (a folder, or a worktree's file)."""
    try:
        path = Path(cwd).resolve()
    except (OSError, RuntimeError):
        return None
    for candidate in (path, *path.parents):
        if (candidate / ".git").exists():
            return candidate
    return None


def derive_bank(environment, cwd):
    explicit = environment.get("GRIMOIRE_BANK", "").strip()
    if explicit:
        return explicit if BANK_ID.fullmatch(explicit) else None
    root = repo_root(cwd) if cwd else None
    if root is None:
        return None
    name = re.sub(r"[^a-z0-9._-]+", "-", root.name.lower()).strip("-._")
    if not name:
        return None
    bank = "coding-agent:" + name[: 64 - len("coding-agent:")]
    return bank if BANK_ID.fullmatch(bank) else None


def clean(text):
    text = INJECTED.sub("", text)
    return text.strip()[:MAX_TURN_CHARS]


def message_text(content, kinds):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(
            part.get("text", "") for part in content
            if isinstance(part, dict) and part.get("type") in kinds
            and isinstance(part.get("text"), str)
        )
    return ""


def turn_of(entry):
    """(speaker, text, timestamp) for one transcript line, or None.

    Claude Code lines are ``{type: user|assistant, message: {role, content}}``;
    Codex lines are ``{type: response_item, payload: {type: message, role,
    content: [{type: input_text|output_text, text}]}}``. Tool calls, tool
    results and thinking are dropped: what is kept is what was said.
    """
    if not isinstance(entry, dict):
        return None
    if entry.get("isMeta") or entry.get("isSidechain") or entry.get("isCompactSummary"):
        return None
    stamp = entry.get("timestamp") if isinstance(entry.get("timestamp"), str) else None
    if entry.get("type") in {"user", "assistant"} and isinstance(entry.get("message"), dict):
        message = entry["message"]
        role = message.get("role") or entry["type"]
        text = message_text(message.get("content"), {"text"})
    elif entry.get("type") == "response_item" and isinstance(entry.get("payload"), dict):
        payload = entry["payload"]
        if payload.get("type") != "message":
            return None
        role = payload.get("role")
        text = message_text(payload.get("content"), {"input_text", "output_text", "text"})
    else:
        return None
    if role not in {"user", "assistant"}:
        return None
    text = clean(text)
    if not text:
        return None
    return role, text, stamp


def read_turns(transcript_path):
    path = Path(transcript_path)
    if not path.is_file():
        return []
    size = path.stat().st_size
    with path.open("rb") as handle:
        if size > MAX_TRANSCRIPT_BYTES:
            handle.seek(size - MAX_TRANSCRIPT_BYTES)
            handle.readline()  # drop the partial first line
        raw = handle.read()
    turns = []
    for line in raw.splitlines():
        try:
            turn = turn_of(json.loads(line))
        except ValueError:
            continue
        if turn:
            speaker, text, stamp = turn
            item = {"speaker": speaker, "text": text}
            if stamp:
                item["timestamp"] = stamp
            turns.append(item)
    # Bound the request by dropping the oldest turns, never the newest.
    while turns and len(json.dumps(turns).encode()) > MAX_CONTENT_BYTES:
        turns.pop(0)
    return turns


def request_json(base, token, path, body, timeout):
    headers = {"Accept": "application/json", "Content-Type": "application/json",
               "X-Grimoire-Agent": "coding-agent-hook"}
    if token:
        headers["Authorization"] = "Bearer " + token
    request = urllib.request.Request(base + path, data=json.dumps(body).encode(),
                                     headers=headers, method="POST")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=timeout) as response:
        raw = response.read(256001)
    if len(raw) > 256000:
        raise ValueError("oversized response")
    return json.loads(raw) if raw else {}


def state_dir(environment):
    directory = Path(environment.get(
        "GRIMOIRE_BANK_STATE_DIR", str(Path.home() / ".cache" / "grimoire" / "bank-sessions")))
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    return directory


def retain(event, environment, base, bank, send):
    session = event.get("session_id", "")
    if not isinstance(session, str) or not re.fullmatch(r"[A-Za-z0-9._:-]{1,200}", session):
        return None
    transcript = event.get("transcript_path", "")
    if not isinstance(transcript, str) or not transcript:
        return None
    turns = read_turns(transcript)
    if not any(t["speaker"] == "user" for t in turns):
        return None
    digest = hashlib.sha256((bank + "\0" + json.dumps(turns)).encode()).hexdigest()
    marker = state_dir(environment) / (hashlib.sha256(
        (base + "\0" + bank + "\0" + session).encode()).hexdigest() + ".sent")
    try:
        if marker.read_text() == digest:
            return None  # nothing new since the last retain of this session
    except OSError:
        pass
    item = {
        "content": turns,
        "document_id": "session:" + session,
        "context": CONTEXT,
        "tags": ["source:session"],
        "metadata": {"session_id": session, "source": "session-hook",
                     "harness": environment.get("GRIMOIRE_BANK_HARNESS", "")},
        "update_mode": "replace",
    }
    first = next((t["timestamp"] for t in turns if t.get("timestamp")), None)
    if first:
        item["timestamp"] = first
    timeout = max(1.0, min(60.0, float(environment.get("GRIMOIRE_BANK_TIMEOUT", "10"))))
    send(base, environment.get("GRIMOIRE_AUTH_TOKEN", ""),
         "/api/banks/" + urllib.parse.quote(bank, safe="") + "/memories", {"items": [item]}, timeout)
    marker.write_text(digest)
    return None


def recall(event, environment, base, bank, send):
    prompt = event.get("prompt", "")
    if not isinstance(prompt, str):
        return None
    prompt = prompt.strip()
    if len(prompt) < 5 or len(prompt.encode()) > 8000 or re.fullmatch(
        r"(?:thanks?|thank you|ok(?:ay)?|yes|no|continue|proceed|hi|hello)[.!\s]*",
        prompt, flags=re.IGNORECASE,
    ):
        return None
    tokens = max(64, min(4096, int(environment.get("GRIMOIRE_BANK_RECALL_TOKENS", "1024"))))
    out = send(base, environment.get("GRIMOIRE_AUTH_TOKEN", ""),
               "/api/banks/" + urllib.parse.quote(bank, safe="") + "/memories/recall",
               {"query": prompt[:2000], "budget": "low", "max_tokens": tokens,
                "include": {"entities": None}}, 2.0)
    results = out.get("results") if isinstance(out, dict) else None
    if not isinstance(results, list):
        return None
    lines = []
    for fact in results:
        if not isinstance(fact, dict) or not isinstance(fact.get("text"), str):
            continue
        mark = " [written by a person]" if fact.get("authority") == "human" else ""
        if fact.get("disputed_by"):
            mark += " [disputed]"
        when = fact.get("occurred_start") or fact.get("mentioned_at") or ""
        lines.append(f"- {fact['text'].strip()}{mark}" + (f" ({when[:10]})" if when else ""))
    if not lines:
        return None
    head = (f'<grimoire_bank_memories bank="{bank}">\n'
            "Recalled from this repository's memory bank; a record of the past, which may or "
            "may not bear on the task. Verify against the code.\n")
    tail = "\n</grimoire_bank_memories>"
    body = ""
    for line in lines:
        if len((head + body + line + "\n" + tail).encode()) > MAX_CONTEXT_BYTES:
            break
        body += line + "\n"
    if not body:
        return None
    return {"hookSpecificOutput": {"hookEventName": "UserPromptSubmit",
                                   "additionalContext": head + body.rstrip("\n") + tail}}


def run(event, environment=None, send=request_json):
    environment = os.environ if environment is None else environment
    name = event.get("hook_event_name", "")
    if name in {"Stop", "SessionEnd"}:
        if environment.get("GRIMOIRE_BANK_SESSIONS") != "1":
            return None
    elif name == "UserPromptSubmit":
        if environment.get("GRIMOIRE_BANK_RECALL") != "1":
            return None
    else:
        return None
    base = safe_base(environment)
    if base is None:
        debug(environment, "GRIMOIRE_URL must be https or loopback http")
        return None
    bank = derive_bank(environment, str(event.get("cwd", "")))
    if bank is None:
        debug(environment, "no bank: set GRIMOIRE_BANK or run inside a git repository")
        return None
    if name == "UserPromptSubmit":
        return recall(event, environment, base, bank, send)
    return retain(event, environment, base, bank, send)


def main():
    try:
        raw = sys.stdin.buffer.read(64001)
        if len(raw) > 64000:
            return
        event = json.loads(raw)
        if not isinstance(event, dict):
            return
        result = run(event)
        if result:
            print(json.dumps(result))
    except (OSError, ValueError, TypeError, OverflowError) as error:
        debug(os.environ, "unavailable (" + type(error).__name__ + "); the session continues")


if __name__ == "__main__":
    main()
