# Coding agents and per-repository memory banks

A coding agent forgets a repository between sessions: why a flag exists, which
approach was tried and abandoned, what the user prefers. A [memory
bank](MEMORY_BANKS.md) per repository fixes that. The agent retains what it
learns, recalls it at the start of the next task, and anything you correct in
the bank's files outranks what a model extracted.

The convention is one bank per repository, shared by every agent that works in
it:

```
coding-agent:<repo>        e.g. coding-agent:grimoire
```

`:` names a family of banks; on disk it is `banks/coding-agent__<repo>/`, an
ordinary folder of notes you can read, edit and put under the same spaces and
reader lists as anything else.

Everything below is opt-in. The quickest way in is one command:

```bash
grimoire agent install --claude-code --codex --bank coding-agent:myrepo   # see section 0
```

or merge the snippets in sections 1 and 3 by hand.

## 0. One-command install

```bash
grimoire agent install [--claude-code] [--codex] [--bank NAME] [--url URL]
                       [--recall] [--no-mcp] [--no-tools] [--dry-run]
grimoire agent status
grimoire agent uninstall
```

With neither agent flag, the agents whose folder exists (`~/.claude`, `~/.codex`)
are used. `install` copies the hook script to `~/.grimoire/hooks/` and merges:

| agent | hooks into | MCP server into |
|---|---|---|
| Claude Code | `~/.claude/settings.json` | `~/.claude.json` (`mcpServers.grimoire`) |
| Codex | `~/.codex/hooks.json` | `~/.codex/config.toml` (a marked `[mcp_servers.grimoire]` block) |

Hooks installed: `SessionStart`, `PostToolUse` (omit with `--no-tools`), `Stop`,
`SessionEnd` (Claude Code only), and `UserPromptSubmit` with `--recall`. Each
carries its settings as environment variables on the command line, so what is
on is visible in the agent's own settings. Without `--bank` the hook picks
`coding-agent:<repo>` from the enclosing git repository; the MCP server then
needs a `bank` argument or a project-level `GRIMOIRE_BANK`.

It merges and never replaces. Your other hooks, servers and settings are left as
they are; only entries it wrote (recognised by the hook script's name, or by
`GRIMOIRE_INSTALLED_BY` in the MCP entry) are replaced in place. A file that
changes is first copied to `FILE.grimoire-backup-<time>`; running the command
again changes nothing and makes no backup. A settings file that is not valid JSON
is refused rather than rewritten. An existing `grimoire` MCP entry that it did
not write is kept. JSON files are rewritten with sorted keys and two-space
indent. `--dry-run` prints what would change and writes nothing. No token is
written: set `GRIMOIRE_AUTH_TOKEN` in the agent's environment. `uninstall`
removes exactly what `install` added. The installer is for Unix-like systems
(the hook command is `python3 …`). If your Codex version gates hooks behind a
feature flag, enable it in `config.toml`.

## 1. Point the agent at the bank over MCP

The MCP server is `grimoire-mcp` (stdio). `GRIMOIRE_BANK` selects the bank its
bank tools use when a call names none, so the agent never has to know or type
the bank id.

**Claude Code** — `.mcp.json` in the repository (or `claude mcp add`):

```json
{
  "mcpServers": {
    "grimoire": {
      "command": "/path/to/grimoire-mcp",
      "env": {
        "GRIMOIRE_URL": "http://127.0.0.1:9111",
        "GRIMOIRE_AGENT_NAME": "claude-code",
        "GRIMOIRE_BANK": "coding-agent:myrepo"
      }
    }
  }
}
```

```bash
claude mcp add grimoire --scope project \
  -e GRIMOIRE_URL=http://127.0.0.1:9111 -e GRIMOIRE_AGENT_NAME=claude-code \
  -e GRIMOIRE_BANK=coding-agent:myrepo -- /path/to/grimoire-mcp
```

**Codex** — `.codex/config.toml` in the repository, or `~/.codex/config.toml`:

```toml
[mcp_servers.grimoire]
command = "/path/to/grimoire-mcp"
env = { GRIMOIRE_URL = "http://127.0.0.1:9111", GRIMOIRE_AGENT_NAME = "codex", GRIMOIRE_BANK = "coding-agent:myrepo" }
```

On a gated server add `GRIMOIRE_AUTH_TOKEN`. Put it in the launcher's
environment rather than a file you commit.

**One bank per endpoint, over HTTP.** For a client that speaks MCP over HTTP,
run `grimoire-mcp` with its HTTP transport. Per-bank endpoints live there — on
the MCP process's own port (default `127.0.0.1:9112`), **not** on the Grimoire
server's port, which only serves the REST API the MCP process calls:

```bash
GRIMOIRE_MCP_TRANSPORT=http GRIMOIRE_MCP_PORT=9112 \
GRIMOIRE_URL=http://127.0.0.1:9111 GRIMOIRE_AGENT_NAME=claude-code \
  /path/to/grimoire-mcp
```

```bash
claude mcp add --transport http grimoire-myrepo \
  http://127.0.0.1:9112/mcp/coding-agent:myrepo
```

`/mcp/<bank>` hands the agent exactly that bank: the tools take no `bank`
argument and `list_banks` / `create_bank` are not offered. `/mcp` with an
`X-Bank-Id: <bank>` header makes that bank the default instead. It binds
loopback and has no authentication of its own: set `GRIMOIRE_MCP_TOKEN` (sent
as `Authorization: Bearer …`) or put it behind a proxy before exposing it. A
bank's `mcp_tools` setting (e.g. `grimoire bank update coding-agent:myrepo
--config mcp_tools=bank_recall,reflect`) narrows what an agent may call on it.

The bank tools the agent gets:

| tool | what it does |
|---|---|
| `retain` | hand over content (text, or a JSON array of `{speaker, text, timestamp}` turns) to extract facts from; queued, returns an `operation_id` |
| `bank_recall` | recall facts (and observations) for a question, within a token budget |
| `reflect` | answer a question by reasoning over mental models, observations and facts, citing what it used |
| `list_banks`, `create_bank`, `bank_profile` | banks and their missions |
| `bank_index`, `bank_timeline`, `bank_get` | skim one line per entry, look around an entry or a day, then read only the `#id`s you need |
| `list_bank_memories`, `get_bank_memory`, `delete_bank_memory` | browse and remove facts (a fact a person wrote needs `force`) |
| `list_entities` | people, components and tools the bank knows about |
| `list_bank_documents`, `get_bank_document`, `delete_bank_document` | the sources facts came from |
| `consolidate`, `list_observations` | distil facts into observations, and read them |
| `list_mental_models`, `get_mental_model`, `create_mental_model`, `update_mental_model`, `delete_mental_model`, `refresh_mental_model` | standing questions whose answers the bank keeps written down (the `coding-agent` template creates three) |
| `list_directives`, `create_directive`, `delete_directive` | standing rules every reflect answer follows |
| `list_operations`, `get_operation`, `cancel_operation` | background work: retains, consolidations, refreshes |
| `list_bank_templates`, `import_bank_template` | bank templates |

Consolidation and mental-model refreshes need a language model on the server
(`model_required` otherwise); reflect then answers by quoting what recall
finds.

Add a line to the repository's `CLAUDE.md` / `AGENTS.md` so the agent uses them:

```markdown
This repository has a Grimoire memory bank (`coding-agent:myrepo`, via the
`grimoire` MCP server). Before starting a task, `bank_recall` the goal (or
`reflect` on a question about the project). After a decision, a dead end or a
user preference, `retain` it in a sentence or two, with the reason. Memory is a
record of the past: verify it against the code.
```

A good bank profile helps extraction keep the right things:

```bash
grimoire bank create coding-agent:myrepo --template coding-agent
# or set the missions directly
grimoire bank update coding-agent:myrepo \
  --mission "What an agent working in this repository needs to know" \
  --retain-mission "Technical decisions and their reasons, conventions, gotchas, the user's preferences. Not routine chatter."
```

## 2. Seed the bank from git history

```bash
grimoire bank import-git /path/to/myrepo              # last 300 commits
grimoire bank import-git . --limit 1000 --diffs --max-diff-bytes 20000
```

Each commit becomes one document, `git:<sha>`: its message, author, changed
paths and (with `--diffs`, capped) the diff, timestamped with the author date.
The bank defaults to `coding-agent:<repository directory name>`; `--bank`
chooses another. Re-running it is idempotent: a commit already retained is the
same document with the same content, so nothing is extracted again, and new
commits are added. Run it from a cron job or a post-merge hook to keep up.

## 3. Optional: retain session transcripts automatically

`clients/hooks/grimoire_bank_session.py` is a standalone Python 3 standard
library hook for Claude Code and Codex. On `Stop` and `SessionEnd` it reads the
session transcript, keeps the user and assistant text turns (tool calls, tool
results, thinking, meta and side-chain lines are dropped, and injected blocks
such as `<system-reminder>` are stripped), and retains them as one conversation
document, `session:<session_id>`, tagged `source:session`.

The whole transcript is sent each time and replaces the stored document, so
the hook is idempotent: the server re-extracts only the chunks that changed,
and the hook skips the request entirely when nothing changed since its last
send. The retain is queued (`async`): the server answers at once with an
operation id and extracts in the background, so a long transcript never holds
the agent up (`grimoire bank ops ls coding-agent:myrepo` shows the work). A
server without the operations queue is retained into in the foreground. With `GRIMOIRE_BANK_RECALL=1` it also handles `UserPromptSubmit`: it
recalls from the bank with the prompt and adds up to 4 KB of facts as context,
marking a person's facts and disputed ones.

`grimoire agent install` does this merge for you, including the `SessionStart`
and `PostToolUse` entries described below. To do it by hand, merge these entries
with your existing hooks; do not replace unrelated ones:

```json
{
  "hooks": {
    "Stop": [{"hooks": [{
      "type": "command",
      "command": "python3 /absolute/path/to/grimoire/clients/hooks/grimoire_bank_session.py",
      "timeout": 15
    }]}],
    "SessionEnd": [{"hooks": [{
      "type": "command",
      "command": "python3 /absolute/path/to/grimoire/clients/hooks/grimoire_bank_session.py",
      "timeout": 15
    }]}],
    "UserPromptSubmit": [{"hooks": [{
      "type": "command",
      "command": "python3 /absolute/path/to/grimoire/clients/hooks/grimoire_bank_session.py",
      "timeout": 3
    }]}]
  }
}
```

Claude Code uses `.claude/settings.json`; Codex uses `.codex/hooks.json` (or
the user-level files). Codex has no `SessionEnd`; `Stop` is enough. Supply the
environment in the agent launcher:

| variable | meaning |
|---|---|
| `GRIMOIRE_BANK_SESSIONS=1` | required to retain anything |
| `GRIMOIRE_BANK_RECALL=1` | also inject recalled facts on each prompt |
| `GRIMOIRE_BANK_CONTEXT=1` | inject the bank's rules, knowledge and last-session digest at `SessionStart` |
| `GRIMOIRE_BANK_TOOLS=1` | record file edits and commands at `PostToolUse` (see below) |
| `GRIMOIRE_BANK_DIGEST=0` | turn the session digest off (on by default with `GRIMOIRE_BANK_SESSIONS=1`) |
| `GRIMOIRE_BANK_DIGEST_MODEL=0` | never ask for a model-written digest; rules only |
| `GRIMOIRE_HOOK_MAX_CHARS` | the most characters any injection may render (default 9000; clamped to 300..9800) |
| `GRIMOIRE_BANK` | the bank; default `coding-agent:<name of the enclosing git repository>` |
| `GRIMOIRE_URL` | default `http://127.0.0.1:9111`; must be loopback HTTP or HTTPS |
| `GRIMOIRE_AUTH_TOKEN` | bearer token for a gated server; never printed |
| `GRIMOIRE_BANK_TIMEOUT` | retain timeout in seconds (default 10, at most 60) |
| `GRIMOIRE_BANK_RECALL_TOKENS` | recall budget (default 1024) |
| `GRIMOIRE_BANK_HARNESS` | recorded in the document's metadata, e.g. `claude-code` |
| `GRIMOIRE_BANK_STATE_DIR` | where "already sent" markers live (hashes only) |
| `GRIMOIRE_BANK_DEBUG=1` | a content-free line on stderr when something fails |

Limits and failure behaviour, in the style of the [context
hook](AUTOMATIC_MEMORY.md):

- Reads at most the newest 8 MB of a transcript; each turn is capped at 8,000
  characters, and the request at 1.5 MB by dropping the oldest turns.
- No redirects and no ambient HTTP proxy. A failure is fail-open: the session
  carries on, nothing is retained, and the next `Stop` tries again.
- Local state holds a hash of what was sent per session, never transcript text.
- What the extraction keeps depends on the bank's retain mission and on whether
  the server has a model configured; without one, sentences become facts.
- A transcript can contain things you would not want in a bank shared with
  other people. Keep the bank's folder private, or leave the hook off.

### Privacy

The hook never sends text you marked `<private>…</private>` (an unclosed tag
hides the rest of that turn), and it redacts credentials (the same shapes
`grimoire secret scan` finds, plus high-entropy values assigned to
secret-named variables) from transcript turns, tool records and digests before
anything is written to disk or sent. The server strips private spans again on
every retain, over the API, MCP and CLI alike, so a client that forgets still
cannot store them. Redaction is pattern-based: it cannot know that an ordinary
sentence is sensitive. Wrap that in `<private>`.

### Session start and digests

With `GRIMOIRE_BANK_CONTEXT=1` the `SessionStart` hook asks the bank for its
context (`startup`, `resume`, `clear` and `compact` alike) and adds it to the
session: the previous session's digest first, then the bank's directives, mental
models, observations and facts, each cut to fit `GRIMOIRE_HOOK_MAX_CHARS`
(Claude Code truncates hook output over 10,000 characters, so the default is
9,000). The lowest-value items are dropped first and a line says how many.

Each `Stop` writes the session's digest, `banks/<bank>/sessions/<id>.md`, with a
request, learned, done and next. No model is involved at `Stop`; `SessionEnd`
(or `Stop` on Codex, which has no `SessionEnd`) asks the server for one
model-written digest if a model is configured, falling back to the rule-based
one. Edit the digest freely: text outside the markers is never touched, and
editing inside them pins the note against regeneration.

### Tool activity, without a model call per tool

With `GRIMOIRE_BANK_TOOLS=1`, `PostToolUse` appends one line per file edit
(path) or command (text and exit status, never its output) to a local buffer
under `GRIMOIRE_BANK_STATE_DIR`, mode 600, capped at 400 KB. Nothing is sent and
no model runs while the agent works. At `Stop` the buffer is retained once as
the document `activity:<session>` in `chunks` mode (no extraction model) and
passed to the digest, so a session costs at most one model call for the digest
plus whatever the transcript extraction costs. `SessionEnd` deletes the buffer;
buffers of sessions that never ended are swept after seven days. Commands and
paths pass the privacy rules above first; reads (`Read`, `grep`) are not
recorded.

### Skimming the bank instead of dumping it

The agent can triage cheaply: `bank_index` returns one line per entry,
`bank_timeline` shows what surrounds one, and `bank_get` fetches the few it
needs by `#id`. See [MEMORY_BANKS.md](MEMORY_BANKS.md).

## Reading what the bank learned

The **Memory banks** panel in the web app (command palette: "Memory banks")
shows each bank's facts, documents, entities, observations, mental models (as
a folder tree, with refresh and any pending proposal to accept or reject),
directives and background operations, and has a playground for recall (with
each arm's ranks) and reflect (with the memories it cited).
`grimoire bank recall coding-agent:myrepo "why is the cache keyed by commit?"`
and `grimoire bank reflect coding-agent:myrepo "…"` do the same from a shell.

Correct a fact in its file and it is yours from then on: re-retains, document
replacement and model re-extraction never overwrite it. The same holds one
level up: an observation you edit is never revised by consolidation (the
model's disagreement is filed beside it as a challenge), and a mental model
whose text you edited is never overwritten — the next refresh waits as a
proposal for you to accept or reject.
