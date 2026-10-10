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

Utilization trace (docs/MEMORY_TRACE.md): beside the tool and its target, each
PostToolUse also sends OUTCOME CODES for the call, read locally from the tool
response and never sent as text: failed / exit code, test pass and fail counts
for go test, pytest, npm test and cargo test, whether the call re-edited or
reverted an earlier edit of the same lines (ids of hashed line sets), whether
the same command has now run three times, whether the user or harness denied it,
and which memories' fingerprints or tags THIS call carries. The prompt event
sends one bit, whether the prompt corrects the agent, classified locally by a
regex. PostToolUseFailure (Claude Code) is handled like a failed PostToolUse.
Events, tool names and payload fields come from the agent profile
(`--agent NAME`), so any agent with a profile works. GRIMOIRE_TRACE=0 turns the
trace fields off.

Fails silently: a missing server must never disturb the agent.
"""

import hashlib
import json
import os
import re
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

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

# ---- agent profile (the same shape grimoire_context.py reads) --------------

DEFAULT_EVENTS = {"prompt": "UserPromptSubmit", "pre_action": "PreToolUse", "post_action": "PostToolUse",
                  "post_action_failure": "PostToolUseFailure", "stop": "Stop"}
DEFAULT_FIELDS = {"event": "hook_event_name", "prompt": "prompt", "tool": "tool_name",
                  "tool_input": "tool_input", "session": "session_id", "cwd": "cwd",
                  "transcript": "transcript_path"}
DEFAULT_PROFILE = {"name": "claude-code", "hooks": {"events": DEFAULT_EVENTS}, "event_fields": DEFAULT_FIELDS,
                   "actions": ACTION_TOOLS, "delegation_tools": ["Agent", "Task"]}
TU_FIELDS = ("tool_use_id", "toolUseId", "tool_call_id", "call_id", "tool_use.id")
TU_RE = re.compile(r"[A-Za-z0-9_\-:.]{1,128}")


def load_profile(name, environment):
    """The resolved profile `grimoire agent install` wrote, or Claude Code's shape.
    Never an error: a hook that fails loudly stops the agent's turn."""
    if not name or not re.fullmatch(r"[a-z][a-z0-9_-]{0,31}", name):
        return DEFAULT_PROFILE
    directory = environment.get("GRIMOIRE_AGENT_DIR") or str(Path.home() / ".grimoire" / "agents")
    try:
        raw = (Path(directory) / (name + ".json")).read_bytes()
        value = json.loads(raw) if len(raw) <= 64000 else None
    except (OSError, ValueError):
        return DEFAULT_PROFILE
    if not isinstance(value, dict):
        return DEFAULT_PROFILE
    merged = dict(DEFAULT_PROFILE)
    merged.update({key: item for key, item in value.items() if item not in (None, {}, "")})
    return merged


def lookup(source, path):
    """A JSON path ("a.b", or "a|b" to try a then b) into a payload."""
    for option in str(path).split("|"):
        value = source
        for part in option.split("."):
            value = value.get(part) if isinstance(value, dict) else None
        if isinstance(value, list) and all(isinstance(v, str) for v in value):
            value = " ".join(value)
        if value not in (None, "", {}):
            return value
    return None


def event_kind(event, profile, forced=None):
    """post_action, post_action_failure, stop, pre_action or prompt - the
    logical event, from --event or the profile's own event names."""
    forced = {"post-action": "post_action", "pre-action": "pre_action", "action": "pre_action"}.get(forced, forced)
    if forced in DEFAULT_EVENTS:
        return forced
    fields = profile.get("event_fields") or DEFAULT_FIELDS
    native = lookup(event, fields.get("event", "hook_event_name"))
    events = dict(DEFAULT_EVENTS)
    events.update((profile.get("hooks") or {}).get("events") or {})
    for logical in DEFAULT_EVENTS:
        if events.get(logical) and events[logical] == native:
            return logical
    return None


def profile_target(profile, tool, tool_input):
    """The command or path of a call, per the profile's `actions`; None for a
    tool that draws no memory."""
    actions = profile.get("actions") or ACTION_TOOLS
    if tool not in actions or not isinstance(tool_input, dict):
        return None
    if tool in (profile.get("delegation_tools") or []):
        parts = [str(tool_input.get("subagent_type", "") or ""),
                 "model " + str(tool_input.get("model", "") or "default"),
                 str(tool_input.get("description", "") or ""),
                 str(tool_input.get("prompt", "") or "")[:300]]
        return " ".join(p for p in parts if p.strip())
    value = lookup(tool_input, actions[tool])
    return value if isinstance(value, str) else ""


# ---- outcome codes from a tool response (all local; only codes are sent) ----

EXIT_RE = re.compile(r"(?:\"exit_code\"\s*:\s*|Process exited with code |Exit code:? |exit code |exit status )(-?\d+)")
DENIED_RE = re.compile(
    r"(?i)(user (doesn'?t|does not|did not) want to (proceed|take this action)|the user rejected|"
    r"tool use was rejected|rejected by (the )?user|denied by (the )?(user|policy|sandbox|hook)|"
    r"blocked by (a |the )?hook|operation (was )?(cancelled|canceled) by (the )?user|request was denied)")
FAIL_KEYS = ("is_error", "isError", "error")
EXIT_KEYS = ("exit_code", "exitCode", "returncode", "return_code")
TEXT_KEYS = ("stdout", "stderr", "output", "content", "message", "text", "error", "formatted_output")


def _walk_response(value, depth=0):
    """(failed, exit, text) found in a response of any agent's shape."""
    failed, exit_code, text = None, None, []
    if isinstance(value, str):
        text.append(value[:20000])
    elif isinstance(value, dict) and depth < 3:
        for key in FAIL_KEYS:
            item = value.get(key)
            if isinstance(item, bool):
                failed = item or failed
            elif key == "error" and item not in (None, "", False):
                failed = True
        if value.get("success") is False or value.get("interrupted") is True:
            failed = True
        for key in EXIT_KEYS:
            item = value.get(key)
            if isinstance(item, int) and not isinstance(item, bool):
                exit_code = item
        for key in TEXT_KEYS:
            item = value.get(key)
            if isinstance(item, str):
                text.append(item[:20000])
            elif isinstance(item, list):
                for block in item[:20]:
                    if isinstance(block, dict) and isinstance(block.get("text"), str):
                        text.append(block["text"][:20000])
        for key in ("metadata", "result", "response", "state"):
            if isinstance(value.get(key), dict):
                f2, e2, t2 = _walk_response(value[key], depth + 1)
                failed = failed or f2 or None
                exit_code = e2 if e2 is not None else exit_code
                text.append(t2)
    return failed, exit_code, "\n".join(t for t in text if t)


def parse_response(response, failure_event=False):
    """-> dict(err 0/1/None, exit int/None, text, denied bool). `err` is None
    when nothing was returned to read."""
    if isinstance(response, str):
        stripped = response.lstrip()
        if stripped.startswith("{"):
            try:
                loaded = json.loads(stripped)
                if isinstance(loaded, dict):
                    response = loaded
            except ValueError:
                pass
    failed, exit_code, text = _walk_response(response)
    if exit_code is None:
        found = EXIT_RE.search(text[:20000])
        if found:
            exit_code = int(found.group(1))
    err = None
    if failure_event or failed:
        err = 1
    elif exit_code is not None:
        err = 1 if exit_code != 0 else 0
    elif response not in (None, "", {}):
        err = 0
    return {"err": err, "exit": exit_code, "text": text,
            "denied": bool(DENIED_RE.search(text[:20000]))}


RUNNER = re.compile(r"(?:^|[\s;&|(])(?:go\s+test|pytest|py\.test|python3?\s+-m\s+(?:pytest|unittest)|npm\s+(?:run\s+)?test|"
                    r"npm\s+t\b|yarn\s+test|pnpm\s+(?:run\s+)?test|npx\s+(?:jest|vitest|mocha)|jest|vitest|mocha|cargo\s+test)")


def parse_tests(command, text):
    """(passed, failed) counts from a test runner's output, or (None, None)."""
    if not command or not text or not RUNNER.search(command):
        return None, None
    text = text[-20000:]
    passed = failed = None
    # cargo: one "test result:" line per test binary
    results = re.findall(r"test result: (?:ok|FAILED)\. (\d+) passed; (\d+) failed", text)
    if results:
        return sum(int(p) for p, _ in results), sum(int(f) for _, f in results)
    # jest / vitest
    found = re.search(r"Tests:?\s+(.*?)(?:\n|$)", text)
    if found and re.search(r"\d+ (?:passed|failed)", found.group(1)):
        line = found.group(1)
        p, f = re.search(r"(\d+) passed", line), re.search(r"(\d+) failed", line)
        return int(p.group(1)) if p else 0, int(f.group(1)) if f else 0
    # mocha
    p, f = re.search(r"(\d+) passing", text), re.search(r"(\d+) failing", text)
    if p or f:
        return int(p.group(1)) if p else 0, int(f.group(1)) if f else 0
    # pytest summary: "=== 2 failed, 5 passed, 1 skipped in 0.3s ===" (also "-q" form)
    summary = [ln for ln in text.splitlines() if re.search(r"\b\d+ (?:passed|failed|errors?)\b", ln)
               and re.search(r"\bin [\d.]+s\b", ln)]
    if summary:
        line = summary[-1]
        p = re.search(r"(\d+) passed", line)
        f = sum(int(n) for n in re.findall(r"(\d+) (?:failed|errors?)\b", line))
        return int(p.group(1)) if p else 0, f
    # go test: per-test lines with -v, else per-package lines
    ok_t, fail_t = len(re.findall(r"^\s*--- PASS:", text, re.M)), len(re.findall(r"^\s*--- FAIL:", text, re.M))
    ok_p, fail_p = len(re.findall(r"^ok\s", text, re.M)), len(re.findall(r"^FAIL\s", text, re.M))
    if ok_t or fail_t:
        return ok_t, fail_t
    if ok_p or fail_p:
        return ok_p, fail_p
    return passed, failed


# ---- edit regions: re-edit and revert of the same lines ----------------------

PATCH_FILE = re.compile(r"^(?:\*\*\* (?:Update|Add|Delete) File: |\+\+\+ (?:b/)?)(\S.*)$", re.M)


def _lines(text):
    """Hashes of the non-trivial lines of a text (at most 64): the region."""
    out = []
    for line in str(text).splitlines():
        line = line.strip()
        if len(line) >= 6:
            digest = hashlib.sha1(line.encode("utf-8", "replace")).hexdigest()[:8]
            if digest not in out:
                out.append(digest)
        if len(out) >= 64:
            break
    return out


def edit_parts(tool_input):
    """(path, removed text, added text) of an edit-shaped tool call, or None.
    Understands Edit/MultiEdit-style fields, whole-file writes and patch text."""
    if not isinstance(tool_input, dict):
        return None
    path = lookup(tool_input, "file_path|path|notebook_path|filename") or ""
    if isinstance(tool_input.get("edits"), list):
        old = "\n".join(str(e.get("old_string", "")) for e in tool_input["edits"] if isinstance(e, dict))
        new = "\n".join(str(e.get("new_string", "")) for e in tool_input["edits"] if isinstance(e, dict))
        return str(path), old, new
    if "new_string" in tool_input or "old_string" in tool_input:
        return str(path), str(tool_input.get("old_string", "")), str(tool_input.get("new_string", ""))
    for key in ("content", "new_source", "file_text"):
        if isinstance(tool_input.get(key), str):
            return str(path), "", tool_input[key]
    patch = lookup(tool_input, "input|patch|command")
    if isinstance(patch, str) and PATCH_FILE.search(patch):
        files = PATCH_FILE.findall(patch)
        old = "\n".join(ln[1:] for ln in patch.splitlines() if ln.startswith("-") and not ln.startswith("---"))
        new = "\n".join(ln[1:] for ln in patch.splitlines() if ln.startswith("+") and not ln.startswith("+++"))
        return ",".join(sorted(f.strip() for f in files)), old, new
    return None


def _path_id(path):
    return hashlib.sha256(("path\0" + path).encode("utf-8", "replace")).hexdigest()[:12]


def region_effects(state, parts):
    """Compare this edit with the session's earlier ones. Returns (region id,
    ids of earlier regions this edit edited again, ids it undid). The state
    keeps only hashes of lines."""
    path, old, new = parts
    pid = _path_id(path)
    old_l, new_l = set(_lines(old)), set(_lines(new))
    region = hashlib.sha256((pid + "\0" + ",".join(sorted(new_l)) + "\0" + hashlib.sha256(new.encode("utf-8", "replace")).hexdigest()[:12]).encode()).hexdigest()[:16]
    reedit, revert = [], []
    for earlier in state.get("edits", []):
        if earlier.get("path") != pid or earlier.get("region") == region:
            continue
        e_new, e_old = set(earlier.get("new", [])), set(earlier.get("old", []))
        # Revert: this edit restores what the earlier one removed.
        if e_old and len(new_l & e_old) >= max(1, int(0.8 * len(e_old))) and e_old != e_new:
            revert.append(earlier["region"])
            continue
        # Re-edit: this edit changes lines the earlier one wrote (a whole-file
        # write replaces everything the file held).
        whole_file = not old
        if whole_file or (old_l and len(old_l & e_new) >= max(1, min(len(old_l), len(e_new)) // 2)):
            reedit.append(earlier["region"])
    return region, reedit[:8], revert[:8]


def remember_edit(state, parts, region):
    path, old, new = parts
    edits = [e for e in state.get("edits", []) if e.get("region") != region]
    edits.append({"path": _path_id(path), "region": region, "old": _lines(old)[:32], "new": _lines(new)[:32]})
    state["edits"] = edits[-48:]


# ---- trace state: per session, hashes and counters only ------------------------

TRACE_TTL = 6 * 3600


def trace_path(environment, sid):
    directory = environment.get("GRIMOIRE_CONTEXT_STATE_DIR") or os.path.join(
        os.path.expanduser("~"), ".cache", "grimoire", "context")
    return os.path.join(directory, "tr-" + sid + ".json")


def trace_read(path):
    try:
        with open(path, "rb") as handle:
            raw = handle.read(65537)
        state = json.loads(raw) if len(raw) <= 65536 else None
    except (OSError, ValueError):
        state = None
    if not isinstance(state, dict) or time.time() - state.get("ts", 0) > TRACE_TTL:
        return {"ts": time.time(), "cmds": {}, "edits": []}
    return state


def trace_write(path, state):
    state["ts"] = time.time()
    state["cmds"] = dict(list(state.get("cmds", {}).items())[-128:])
    fp_write(path, state)


# ---- the local correction classifier -------------------------------------------

CORRECTION = re.compile(
    r"(?i)^\W*(?:no+\b(?!\s+(?:problem|worries|rush|need|issue|hurry))|nope\b|stop\b|wait\b|wrong\b|incorrect\b|undo\b|revert\b|actually\b|don'?t\b|do not\b|"
    r"not (?:that|what|quite|like)\b|that'?s (?:not|wrong|incorrect)\b|why did you\b|"
    r"i (?:said|told|asked|meant|wanted|didn'?t (?:ask|want))\b|"
    r"you (?:didn'?t|forgot|broke|missed|should(?:n'?t| have)|shouldn'?t have|were supposed)\b|"
    r"that (?:broke|doesn'?t work|didn'?t work|isn'?t (?:right|what))\b|still (?:broken|failing|wrong|not)\b|try again\b)"
    r"|\b(?:i (?:already|just) (?:said|told|asked|mentioned)|like i said|as i (?:said|mentioned)|how many times|"
    r"i told you|that'?s not what i|not what i (?:asked|wanted|said)|you keep (?:forgetting|doing))\b")


def is_correction(prompt):
    """A cheap local guess that a prompt corrects the agent. The text never
    leaves this function; the server's own re-tell detector is a second vote."""
    if not isinstance(prompt, str) or not prompt.strip() or len(prompt) > 4000:
        return False
    return bool(CORRECTION.search(prompt))


def fp_event(state, text):
    """{tag: fingerprints matched in THIS text} for the live items."""
    out = {}
    if not state or not text:
        return out
    cache = {}
    for item in state.get("items", []):
        if not isinstance(item, dict) or not isinstance(item.get("hashes"), list) or not isinstance(item.get("tag"), str):
            continue
        salt = item.get("salt", "")
        if salt not in cache:
            cache[salt] = {fp_hash(salt, token) for token in candidates(text)}
        n = len(set(item["hashes"]) & cache[salt])
        if n:
            out[item["tag"]] = n
    return out


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


AUTOMATED_PROMPT = re.compile(r"\s*(\[SYSTEM NOTIFICATION|<task-notification>|<system-reminder>)", re.IGNORECASE)


def tool_use_id(event, profile):
    """The harness's id for this call, when its payload has one."""
    fields = profile.get("event_fields") or {}
    for path in ([fields["tool_use_id"]] if fields.get("tool_use_id") else []) + list(TU_FIELDS):
        value = lookup(event, path)
        if isinstance(value, str) and TU_RE.fullmatch(value):
            return value
    return None


def trace_body(event, profile, environment, name, tool, tool_input, target, fp_state, state):
    """The outcome codes of one executed call. Codes and counts only."""
    fields = profile.get("event_fields") or {}
    body = {}
    response = lookup(event, fields.get("tool_response") or "tool_response|tool_result|error")
    info = parse_response(response, failure_event=(name == "post_action_failure"))
    if event.get("is_interrupt") is True:
        info["denied"] = True
        info["err"] = 1
    if info["err"] is not None:
        body["err"] = info["err"]
    if info["exit"] is not None and -1000 <= info["exit"] <= 1000000:
        body["exit"] = info["exit"]
    passed, failed = parse_tests(target, info["text"])
    if passed is not None:
        body["tp"], body["tf"] = min(passed, 1000000), min(failed, 1000000)
    if info["denied"]:
        body["denied"] = True
    key = hashlib.sha256((tool + "\0" + target.strip()).encode("utf-8", "replace")).hexdigest()[:16]
    parts = edit_parts(tool_input)
    if parts is None:  # editing one file three times is work, running one command three times is thrash
        count = state["cmds"].get(key, 0) + 1
        state["cmds"][key] = count
        if count >= 3:
            body["thrash"] = min(count, 1000)
    if parts is not None and info["err"] != 1:
        region, reedit, revert = region_effects(state, parts)
        body["region"] = region
        if reedit:
            body["reedit"] = reedit
        if revert:
            body["revert"] = revert
        remember_edit(state, parts, region)
    tu = tool_use_id(event, profile)
    if tu:
        body["tu"] = tu
    if fp_state is not None:
        ev = fp_event({"items": fp_live(fp_state, environment)}, target)
        if ev:
            body["ev"] = ev
    return body


def run(event, environment=None, send=post, kind=None, profile=None):
    environment = os.environ if environment is None else environment
    if environment.get("GRIMOIRE_AUTO_CONTEXT", "1") == "0" or environment.get("GRIMOIRE_OUTCOME", "1") == "0":
        return None
    profile = profile or DEFAULT_PROFILE
    fields = profile.get("event_fields") or DEFAULT_FIELDS
    # `--event` names the event for agents whose own event names differ.
    name = event_kind(event, profile, kind)
    if name is None:
        return None
    session = lookup(event, fields.get("session", "session_id"))
    if not session and profile.get("session_fallback"):
        session = environment.get("GRIMOIRE_SESSION") or "cwd:" + str(lookup(event, fields.get("cwd", "cwd")) or "")
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
    trace_on = environment.get("GRIMOIRE_TRACE", "1") != "0"
    if name == "prompt":
        prompt = lookup(event, fields.get("prompt", "prompt"))
        if (trace_on and isinstance(prompt, str) and prompt.strip() and len(prompt.encode()) <= 8000
                and not AUTOMATED_PROMPT.match(prompt)):
            send(base, token, {"session": sid, "prompt": True, "corr": is_correction(prompt)})
        return None
    fp_file = fp_path(environment, sid)
    fp_state = fp_read(fp_file) if environment.get("GRIMOIRE_FINGERPRINTS", "1") != "0" else None
    if name == "stop":
        tags = []
        path = lookup(event, fields.get("transcript", "transcript_path"))
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
    tool = lookup(event, fields.get("tool", "tool_name")) or ""
    target = profile_target(profile, tool, lookup(event, fields.get("tool_input", "tool_input")))
    if target is None or not target.strip() or len(target.encode()) > 4000:
        return None
    if name in {"post_action", "post_action_failure"}:
        body = {"session": sid, "tool": tool, "target": target}
        if fp_state is not None and fp_state["items"]:
            ev_state = {"turn": fp_state.get("turn", 0), "items": [dict(item) for item in fp_state["items"]]}
            fp_match(fp_state, environment, target)
            for item in fp_live(fp_state, environment):
                item["tools"] = item.get("tools", 0) + 1
            matched = fp_counts(fp_state, environment)
            if matched:
                body["fp"] = matched
            fp_write(fp_file, fp_state)
        else:
            ev_state = None
        if trace_on:
            tr_file = trace_path(environment, sid)
            state = trace_read(tr_file)
            body.update(trace_body(event, profile, environment, name, tool,
                                   lookup(event, fields.get("tool_input", "tool_input")), target, ev_state, state))
            trace_write(tr_file, state)
            tags = TAG.findall(target)
            if tags:
                body["cited"] = list(dict.fromkeys(tags))[:8]
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
    kind, agent = None, os.environ.get("GRIMOIRE_AGENT_PROFILE", "")
    for index, argument in enumerate(argv):
        if argument == "--event" and index + 1 < len(argv):
            kind = argv[index + 1]
        elif argument.startswith("--event="):
            kind = argument[8:]
        elif argument == "--agent" and index + 1 < len(argv):
            agent = argv[index + 1]
        elif argument.startswith("--agent="):
            agent = argument[8:]
    try:
        raw = sys.stdin.buffer.read(65001)
        if len(raw) > 65000:
            return
        event = json.loads(raw)
        if not isinstance(event, dict):
            return
        result = run(event, kind=kind, profile=load_profile(agent, os.environ))
        if result:
            print(json.dumps(result))
    except (OSError, ValueError, TypeError, OverflowError):
        if os.environ.get("GRIMOIRE_CONTEXT_DEBUG") == "1":
            print("Grimoire outcome reporting unavailable.", file=sys.stderr)


if __name__ == "__main__":
    main()
