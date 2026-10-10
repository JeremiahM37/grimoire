# Memory for any agent

Grimoire injects what an agent has learned into its sessions. Which agent does
not matter: there are four ways in, and adding an agent that has none of them
yet is writing one JSON file.

| Tier | Works for | How | Install |
|---|---|---|---|
| 0 MCP | any MCP client | the server's `instructions` carry the memory core (standing rules and pointers) when a session starts; tools recall the rest | mount `grimoire-mcp` |
| 1 files | any agent that reads an instruction or memory file | the shared memory store is linked or projected into the agent's file | `grimoire memory link` (see MEMORY_STORE.md) |
| 2 hooks | any agent with command hooks (Claude Code, Codex, ...) | per-prompt and per-action injection, driven by an agent profile | `grimoire agent install --agent NAME --memory` |
| 3 HTTP / CLI | agents you run yourself, wrappers | `GET /api/memory/context`, or `grimoire context` | nothing to install |

Tiers stack. An agent with hooks and MCP gets both: the core at session start,
the relevant memories at each prompt and each command.

## Tier 0: the core in the MCP instructions

On `initialize`, `grimoire-mcp` asks `GET /api/memory/core?budget=6000` and
appends the answer to its instructions, under a heading that says it is stored
notes, not permissions. The core is bounded to about 6,000 bytes (cut at a line
break). A server without that endpoint, or one that is slow (1.5 s limit) or
answers oddly, leaves the instructions exactly as before. Set
`GRIMOIRE_MCP_CORE=0` to turn it off.

## Tier 2: add an agent in five minutes

```bash
grimoire agent new myagent            # writes ~/.config/grimoire/agents/myagent.json
$EDITOR ~/.config/grimoire/agents/myagent.json
grimoire agent install --agent myagent --memory --dry-run
grimoire agent install --agent myagent --memory
```

Find three things about the agent and put them in the profile:

1. **Where its hooks are configured** (`hooks.file`) and **what it calls its
   events** (`hooks.events`). Most agents copied Claude Code's format
   (`{"hooks": {"EventName": [{"matcher": ..., "hooks": [{"type": "command", ...}]}]}}`)
   and need nothing more.
2. **What the hook receives on stdin** (`event_fields`): the JSON path of the
   prompt, the tool name, the tool's input, the session id and the working
   directory. Log one real payload with a throwaway hook (`cat >> /tmp/payload.jsonl`)
   and copy the names.
3. **How it takes context back** (`output`): `claude-json` if it understands
   `{"hookSpecificOutput": {"additionalContext": ...}}`, `plain-stdout` if it
   appends whatever the hook prints.

Then check it without the agent:

```bash
echo '{"type":"user_message","conversation":"c1","message":{"text":"how do I deploy kestrel"}}' \
  | python3 ~/.grimoire/hooks/grimoire_context.py --agent myagent
```

`install` is merge-never-replace like the rest of `grimoire agent`: your other
hooks are untouched, a changed file is copied to `FILE.grimoire-backup-TIME`
first, a second run changes nothing, `--dry-run` writes nothing, and `uninstall`
removes only what `install` added. `install` also writes the resolved profile to
`~/.grimoire/agents/NAME.json`, which is the only thing the hook reads, so it
needs no Go. An agent that cannot run a hook at all uses tier 3.

### Profile reference

Built-ins (`grimoire agent profiles`): `claude-code`, `codex`, `generic`. A file
in `~/.config/grimoire/agents/` with a built-in's name overrides it; any other
name adds an agent (starting from `generic`). A field you name replaces that
field whole (inside `hooks`, `mcp` and `memory`, per sub-field); unknown fields
are errors, so a typo is not ignored.

| Field | Meaning |
|---|---|
| `name`, `description` | the file name is the name |
| `detect` | paths whose presence means the agent is installed; `install` with no agent named uses the detected ones |
| `hooks.file` | the hooks file; `~/` is expanded. Empty means "not installable yet" |
| `hooks.format` | `claude-json` (the only one so far) |
| `hooks.events` | logical event to the agent's own name: `session_start`, `prompt`, `pre_action`, `post_action`, `post_action_failure` (agents that report a failed call on its own event, as Claude Code's `PostToolUseFailure`), `stop`, `session_end`, `file_read` |
| `hooks.matchers` | logical event to the tool matcher; `pre_action` defaults to the tools in `actions` |
| `hooks.timeouts` | seconds per logical event (bank hooks only) |
| `hooks.trust` | `codex`: record the agent's own trust hash in `config.toml` |
| `event_fields` | JSON paths in the payload: `event`, `prompt`, `tool`, `tool_input`, `session`, `cwd`, `source`, `transcript`, and for the utilization trace optionally `tool_response` and `tool_use_id` (defaults cover Claude Code and Codex; see [MEMORY_TRACE.md](MEMORY_TRACE.md)). Dotted paths reach into objects (`message.text`); `a\|b` tries `a` then `b`; a list of strings is joined with spaces |
| `actions` | tool name to the field of its input that holds the command or path. Only these tools draw action context |
| `delegation_tools` | tools that launch a sub-agent; matched on purpose and model, not the whole brief |
| `session_fallback` | the agent sends no session id: use `GRIMOIRE_SESSION`, else the working directory |
| `output` | `claude-json` or `plain-stdout` |
| `mcp` | `file` and `format` (`json`: `mcpServers`, `toml`: `mcp_servers`) for the MCP entry; empty skips it |
| `memory` | where the agent keeps instructions or memory (tier 1): `files` (instruction files, get a managed block), `dir` and `glob` (directory memory, becomes a symlink to the store; Claude Code uses `~/.claude/projects/*/memory`). `grimoire agent install --agent NAME --link-memory [--merge]` links all of them; `agent status` shows the state |
| `transcripts` | `glob` + `format` (`claude-jsonl`, `codex-rollout`, `pi`, `opencode`, `cursor`, `generic-jsonl`); for `generic-jsonl` a `map` of field paths (`session`, `cwd`, `time`, `model`, `role`, `text`, `tool`, `tool_target`, `tool_error`, optional `role_user` / `role_assistant`). Lets session retention, `skills mine` and `memory impact` read that agent's sessions. Text is redacted with the vault's secret patterns before it leaves the reader |
| `skills_dir` | where the agent loads native skills from; `grimoire skills export --agent NAME` writes procedure memories there (see MEMORY_SKILLS.md) |

The hook takes `--agent NAME` (or `GRIMOIRE_AGENT_PROFILE`) and `--event
prompt|action|session_start` for agents that do not put the event name in the
payload. With neither it behaves as it always did, for Claude Code. All the
existing `GRIMOIRE_CONTEXT_*` settings apply.

## Tier 3: no hooks

```bash
grimoire context --event prompt --text "$PROMPT"
grimoire context --event action --tool Bash --text "$COMMAND"
```

prints the block the hook would inject (nothing when no memory applies). Use
`--agent NAME` to print in that profile's output format, `--json` for the
server's reply, `--session ID` so the server can pair re-tells. It keeps no
dedupe state, so a wrapper that calls it every turn should skip repeats itself.
Programs can call `GET /api/memory/context` directly.

## What Codex does (codex-cli 0.161, measured 2026-10-09)

Tested with a throwaway hook logging stdin, in a temporary `CODEX_HOME`:

- **Events delivered:** `SessionStart`, `UserPromptSubmit`, `PreToolUse`,
  `PostToolUse`, `Stop`. Every payload carries `session_id`, `transcript_path`,
  `cwd`, `hook_event_name`, `model` and `permission_mode`; turn events add
  `turn_id`. `SessionStart` has `source` (`startup`).
- **`UserPromptSubmit`** has `prompt`. **`PreToolUse`/`PostToolUse`** have
  `tool_name` and `tool_input`; the shell tool arrives as `Bash` with
  `{"command": "echo hello"}`. `PostToolUse` adds `tool_response`.
- **`additionalContext` reaches the model on all of `SessionStart`,
  `UserPromptSubmit` and `PreToolUse`**, in the same `hookSpecificOutput` JSON
  Claude Code uses. A model asked afterwards listed one canary word from each of
  the three hooks. So the `codex` profile uses `claude-json` output and the same
  hook, with the matcher `Bash|apply_patch`.
- **Hooks must be trusted** or Codex never runs them; `install` records Codex's
  own hash in `config.toml` (managed block). Re-run `install` after changing a
  hook command.
- **Not verified:** the `apply_patch` payload (the profile reads `command`, then
  `input`; a patch that matches neither draws no action context, which fails
  safe). The sandboxed `workspace-write` run needed to produce one timed out in
  the test budget.
