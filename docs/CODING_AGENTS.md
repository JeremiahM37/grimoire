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

Everything below is opt-in. Nothing here changes an agent's configuration for
you; the snippets are examples to merge into your own.

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

Merge these entries with your existing hooks; do not replace unrelated ones:

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
