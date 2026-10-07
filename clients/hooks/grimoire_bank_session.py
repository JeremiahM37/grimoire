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
- ``SessionStart`` (only with ``GRIMOIRE_BANK_CONTEXT=1``): add the bank's
  standing rules and knowledge as context. Every injection is measured and its
  lowest-value items dropped until it fits ``GRIMOIRE_HOOK_MAX_CHARS`` (default
  9000; Claude Code truncates hook output over 10,000).

Opt-in (``GRIMOIRE_BANK_SESSIONS=1`` for retain). Loopback HTTP or HTTPS only,
short timeouts, bounded sizes, and fail-open: any problem means the agent
carries on with nothing retained or injected. Nothing secret is printed.
See docs/CODING_AGENTS.md.
"""

import hashlib
import json
import math
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

MAX_TRANSCRIPT_BYTES = 8_000_000   # read at most the newest 8 MB of a transcript
MAX_TURN_CHARS = 8_000
MAX_CONTENT_BYTES = 1_500_000      # oldest turns are dropped beyond this
MAX_CONTEXT_BYTES = 4_000          # default budget for a per-prompt recall injection
DEFAULT_HOOK_CHARS = 9_000         # Claude Code truncates hook output over 10,000 characters
MIN_HOOK_CHARS, MAX_HOOK_CHARS = 300, 9_800
BANK_ID = re.compile(r"[a-z0-9][a-z0-9._:-]{0,63}")
INJECTED = re.compile(
    r"<(system-reminder|grimoire[\w-]*|hook_prompt|task-notification|relevant_memories|"
    r"user_feedback)\b[^>]*>.*?</\1>",
    re.DOTALL,
)
CONTEXT = "coding-agent session transcript"

# Text a person wrapped in <private>...</private> is never retained. An
# opening tag with no closing one hides the rest: failing closed.
PRIVATE_SPAN = re.compile(r"<private\b[^>]*>.*?</private\s*>", re.DOTALL | re.IGNORECASE)
PRIVATE_OPEN = re.compile(r"<private\b[^>]*>.*$", re.DOTALL | re.IGNORECASE)

# Credential shapes, mirroring the server's secret scanner: issuer-defined
# prefixes, plus a secret-named assignment that must clear an entropy bar.
SECRET_SHAPES = [
    ("AWS access key", re.compile(r"\bAKIA[0-9A-Z]{16}\b")),
    ("GitHub token", re.compile(r"\bgh[pousr]_[A-Za-z0-9]{36,}\b")),
    ("GitHub fine-grained token", re.compile(r"\bgithub_pat_[A-Za-z0-9_]{50,}\b")),
    ("Slack token", re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{10,}\b")),
    ("Slack webhook", re.compile(r"https://hooks\.slack\.com/services/[A-Za-z0-9/+]{20,}")),
    ("Stripe key", re.compile(r"\b[sr]k_live_[A-Za-z0-9]{20,}\b")),
    ("Anthropic key", re.compile(r"\bsk-ant-[A-Za-z0-9_-]{32,}\b")),
    ("OpenAI key", re.compile(r"\bsk-(?:proj-)?[A-Za-z0-9_-]{32,}\b")),
    ("Google API key", re.compile(r"\bAIza[0-9A-Za-z_-]{35}\b")),
    ("SendGrid key", re.compile(r"\bSG\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\b")),
    ("npm token", re.compile(r"\bnpm_[A-Za-z0-9]{36}\b")),
    ("PyPI token", re.compile(r"\bpypi-[A-Za-z0-9_-]{50,}\b")),
    ("private key", re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH |PGP )?PRIVATE KEY-----")),
    ("JSON Web Token", re.compile(r"\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b")),
]
SECRET_ASSIGNMENT = re.compile(
    r"\b(api[_-]?key|secret|token|password|passwd|access[_-]?key|auth)\b\s*[:=]\s*[\"']?"
    r"([A-Za-z0-9_\-./+=]{16,})[\"']?", re.IGNORECASE)
PLACEHOLDERS = {"changeme", "your_api_key_here", "todo", "xxx", "placeholder", "example",
                "redacted", "none", "null", "undefined", "test", "secret", "password"}


def entropy(value):
    counts = {c: value.count(c) for c in set(value)}
    return -sum(n / len(value) * math.log2(n / len(value)) for n in counts.values())


def looks_random(value):
    return len(value) >= 16 and entropy(value) >= 3.2


def strip_private(text):
    if "<private" not in text.lower():
        return text
    return PRIVATE_OPEN.sub("", PRIVATE_SPAN.sub("", text))


def redact_secrets(text):
    """Replace credentials with a marker naming their kind."""
    for kind, shape in SECRET_SHAPES:
        text = shape.sub("[REDACTED:" + kind + "]", text)

    def assigned(match):
        value = match.group(2)
        if value.lower() in PLACEHOLDERS or not looks_random(value):
            return match.group(0)
        return match.group(0).replace(value, "[REDACTED:possible " + match.group(1).lower() + "]")

    return SECRET_ASSIGNMENT.sub(assigned, text)


def sanitize(text):
    """What may be stored: no private spans, no credentials."""
    return redact_secrets(strip_private(text))


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
    text = sanitize(INJECTED.sub("", text))
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


def hook_limit(environment, default=DEFAULT_HOOK_CHARS):
    """The most characters an injection may render (GRIMOIRE_HOOK_MAX_CHARS)."""
    try:
        value = int(environment.get("GRIMOIRE_HOOK_MAX_CHARS", default))
    except ValueError:
        value = default
    return max(MIN_HOOK_CHARS, min(MAX_HOOK_CHARS, value))


def fit_items(head, items, tail, limit):
    """Render head + items + tail under ``limit`` characters.

    Items come in descending value. The rendered size is measured and the
    lowest-value (last) item is dropped until it fits, with a note saying how
    many were left out. Returns (text, kept, dropped); text is "" when not even
    the first item fits whole.
    """
    kept = list(items)
    while kept:
        dropped = len(items) - len(kept)
        note = ("\n(%d lower-value items left out.)" % dropped) if dropped else ""
        text = head + "\n".join(kept) + note + tail
        if len(text) <= limit:
            return text, len(kept), dropped
        kept.pop()
    return "", 0, len(items)


def request_get(base, token, path, timeout):
    headers = {"Accept": "application/json", "X-Grimoire-Agent": "coding-agent-hook"}
    if token:
        headers["Authorization"] = "Bearer " + token
    request = urllib.request.Request(base + path, headers=headers, method="GET")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    with opener.open(request, timeout=timeout) as response:
        raw = response.read(256001)
    if len(raw) > 256000:
        raise ValueError("oversized response")
    return json.loads(raw) if raw else {}


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
        "scan_secrets": True,   # the server scans again: a second net
    }
    first = next((t["timestamp"] for t in turns if t.get("timestamp")), None)
    if first:
        item["timestamp"] = first
    timeout = max(1.0, min(60.0, float(environment.get("GRIMOIRE_BANK_TIMEOUT", "10"))))
    token = environment.get("GRIMOIRE_AUTH_TOKEN", "")
    path = "/api/banks/" + urllib.parse.quote(bank, safe="") + "/memories"
    # Queued: the server answers 202 with an operation id at once and extracts
    # in the background, so a long transcript never outlasts the timeout. A
    # server without the operations queue refuses async with a 400 that says
    # so; retain in the foreground there.
    try:
        send(base, token, path, {"items": [item], "async": True}, timeout)
    except urllib.error.HTTPError as error:
        if error.code != 400 or not async_refused(error):
            raise
        send(base, token, path, {"items": [item]}, timeout)
    marker.write_text(digest)
    return None


def async_refused(error):
    try:
        return "async" in error.read(4096).decode("utf-8", "replace").lower()
    except (OSError, ValueError):
        return False


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
    default = MAX_CONTEXT_BYTES if "GRIMOIRE_HOOK_MAX_CHARS" not in environment else DEFAULT_HOOK_CHARS
    body, kept, _ = fit_items(head, lines, tail, hook_limit(environment, default))
    if not kept:
        return None
    return {"hookSpecificOutput": {"hookEventName": "UserPromptSubmit",
                                   "additionalContext": body}}


def start_context(event, environment, base, bank, get):
    """SessionStart: the bank's digest, rules and knowledge, under the limit."""
    limit = hook_limit(environment)
    source = event.get("source", "startup")
    source = source if source in {"startup", "resume", "clear", "compact"} else "startup"
    query = urllib.parse.urlencode({"max_chars": limit, "source": source})
    out = get(base, environment.get("GRIMOIRE_AUTH_TOKEN", ""),
              "/api/banks/" + urllib.parse.quote(bank, safe="") + "/context?" + query, 2.5)
    text = out.get("context") if isinstance(out, dict) else None
    if not isinstance(text, str) or not text.strip():
        return None
    if len(text) > limit:  # the server should have fitted it; never trust that blindly
        text = text[:limit - 1].rstrip() + "…"
    return {"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext": text}}


def run(event, environment=None, send=request_json, get=request_get):
    environment = os.environ if environment is None else environment
    name = event.get("hook_event_name", "")
    if name in {"Stop", "SessionEnd"}:
        if environment.get("GRIMOIRE_BANK_SESSIONS") != "1":
            return None
    elif name == "UserPromptSubmit":
        if environment.get("GRIMOIRE_BANK_RECALL") != "1":
            return None
    elif name == "SessionStart":
        if environment.get("GRIMOIRE_BANK_CONTEXT") != "1":
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
    if name == "SessionStart":
        return start_context(event, environment, base, bank, get)
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
