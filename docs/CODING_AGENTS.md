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

The bank tools the agent gets:

| tool | what it does |
|---|---|
| `retain` | hand over content (text, or a JSON array of `{speaker, text, timestamp}` turns) to extract facts from |
| `bank_recall` | recall facts for a question, within a token budget |
| `list_banks`, `create_bank`, `bank_profile` | banks and their missions |
| `list_bank_memories`, `delete_bank_memory` | browse and remove facts (a fact a person wrote needs `force`) |
| `list_entities` | people, components and tools the bank knows about |
| `list_bank_documents`, `get_bank_document`, `delete_bank_document` | the sources facts came from |

Add a line to the repository's `CLAUDE.md` / `AGENTS.md` so the agent uses them:

```markdown
This repository has a Grimoire memory bank (`coding-agent:myrepo`, via the
`grimoire` MCP server). Before starting a task, `bank_recall` the goal. After a
decision, a dead end or a user preference, `retain` it in a sentence or two,
with the reason. Memory is a record of the past: verify it against the code.
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
send. With `GRIMOIRE_BANK_RECALL=1` it also handles `UserPromptSubmit`: it
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

The **Banks** panel in the web app (command palette: "Memory banks") lists
each bank's facts, documents and entities and has a recall playground.
`grimoire bank recall coding-agent:myrepo "why is the cache keyed by commit?"`
does the same from a shell. Correct a fact in its file and it is yours from
then on: re-retains, document replacement and model re-extraction never
overwrite it.
