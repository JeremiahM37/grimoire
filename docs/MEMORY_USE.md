# Getting agents to use the right memory

A memory store can fail an agent in four ways:

1. **It doesn't look.** The agent never thinks to check.
2. **It looks and misses.** The search runs but the right memory isn't
   returned.
3. **It finds a wrong or stale memory.** [FRESHNESS.md](FRESHNESS.md) covers
   staleness.
4. **It finds the memory and acts against it.**

This document covers the first, second and fourth. It describes what
Grimoire does about each, and the measurements behind each choice.

## What we measured on

- **Real misses.** These are moments in this deployment's Claude Code and
  Codex transcripts where the user had to tell an agent something its memory
  or instructions already held. They came from 290 interactive sessions, and
  13 were verified by hand. The number is small, but the pattern is clear.
  Most were not facts about a topic. They were **standing rules about how to
  work**:
  - "the lead model plans, a cheaper model implements" (5 of 12);
  - "never push or merge a PR unless asked" (3 of 12).

  In every case the rule was on file. Often it was already in the agent's
  context.
- **A retrieval benchmark built from the real store.** We took 320 memories
  sampled from 1,234 (agent-memory files, Grimoire facts and instruction-file
  sections). For each one, a model wrote:
  - a user request where the memory applies;
  - a tool call it applies to;
  - a near-miss request where it does not.

  Writers never shared more than three consecutive words with the memory. A
  second, independent set of requests (round 2) was written for held-out
  testing. Every case asks one thing: does the right memory reach the agent at
  that moment?
- **An adherence benchmark.** It has 101 cases built from memories that imply
  an action, chosen so that an agent without the memory takes the wrong one.
  - The agent was Haiku 5.5, run with no other context.
  - A separate grader call labelled each action as following the memory,
    violating it, or unclear.

## 1. Retrieval: hybrid ranking for injected context

Per-prompt injection (`/api/memory/context`, used by the hook in
`clients/hooks/grimoire_context.py`) matched words only. It now ranks the way
`recall` and `ask_notes` do:

- stored facts by their semantic and keyword score;
- notes by fused embedding and BM25 retrieval.

Each candidate gets one relevance score, built from its embedding similarity
(stretched over the range where nomic-embed-text separates related text) and
its share of the prompt's terms. Candidates below `min_rel` are not injected.
Injection is unrequested, so saying nothing beats saying something wrong.

| Ranking | Right memory injected (round 1 / round 2) | Wrong memory injected on near-misses (round 1 / round 2) | Latency p50 |
|---|---|---|---|
| Word overlap (before) | 45.6% / 49.6% | 12.0% / 16.0% | ~290 ms |
| Hybrid, `min_rel` 0.5 (default) | 59.2% / 54.8% | 14.4% / 10.4% | ~60 ms |
| Hybrid, `min_rel` 0.3 | 63.6% / — | 21.2% / — | ~60 ms |

The bundled MS MARCO cross-encoder reranker made this worse: 63.6% → 59.2%,
for 400 ms more. It was trained on web search, not on matching a situation to
a memory, so it stays off for injection.

## 2. Cues: indexing a memory by when it applies

A memory is written as what is true. An agent arrives with what it is doing.
A **cue** is a short situation attached to a memory: a request, a command or
path, or keywords. The injected context matches the agent's moment against
cues as well as against the memory itself (`internal/cues`). There are three
sources of cues, and they did not perform alike.

**Generated cues do not help.** A local model (qwen3.6) wrote 6,525 cues for
all 1,234 memories, from each memory's text alone.

- Matched leniently, they moved the prompt hit rate by −1.6 and +5.6 points on
  the two rounds. That is within noise, and they raised false fires.
- Matched strictly, paired gains and losses cancelled exactly.

A cue written only from the memory paraphrases the memory, so it lands where
the memory's own embedding already is. Grimoire does not generate cues.

**Learned cues do help.** This experiment was a difference in differences:

1. The memories were split at random into two halves.
2. Whenever a memory in one half was missed on a round-1 request, it was
   taught that request as a cue. That is what a re-tell does.
3. Both halves were then scored on round-2 requests, which are different
   situations the memory had not been taught.

| Round-2 requests, never taught | Before | After |
|---|---|---|
| Memories that learned from a miss | 62 / 127 | **68 / 127** (gained 6, lost 0; p = 0.03) |
| Control memories | 76 / 123 | 76 / 123 |
| False fires on near-misses, either half | unchanged | unchanged |

A learned cue carries what the memory's text lacks: the situation in which
someone needed it.

**Agent cues** are supplied by the writing agent: `remember` takes `cues`.
They carry the same kind of information as learned cues, at write time. An
action cue that names a specific path fires outright at the action stage.
It must be specific: a path that more than three memories' cues mention is
too generic to fire on its own. Firing on generic paths took the action-stage
hit rate from 44% to 19% in the first trial.

### Where learned cues come from

- **A `remember` that turns out to be on file** (op `NOOP`). The agent was
  just told something the store already held. If the call passes `context`,
  that request becomes a learned cue on the existing fact. Without it, the
  same agent's last context query from the past 30 minutes is used.
- **A restatement in the prompt stream.** This covers memories `remember`
  never compares against, such as the agent harness's own memory files,
  which hold most of a real store. The hook sends a hash of the session. When
  a prompt restates a memory, the session's previous prompt becomes that
  memory's cue.

Detecting a restatement is the hard part. We replayed all 2,004 real user
prompts through the server, session by session:

| Detector | Cues learned | Right |
|---|---|---|
| Correction words ("again", "don't", "never", …) plus a 0.75 match | 92, in 36 sessions | almost none ("what's the MCP URL again?" taught "ok let's reduce it") |
| Explicit restatement phrases plus the memory's own words (the fallback) | 0 | — |
| **A decision model asked "does this message restate this memory?"** | — | AUC 0.89 against the verified re-tells and the 92 false candidates |

Real re-tells are paraphrases ("use opus to coordinate and sonnet to
implement, we're burning tokens"), and no word rule tells them from ordinary
prompts. The decision model can. Jev at a 0.9 cut-off:

- caught 6 of the 10 verified re-tells;
- flagged 4 of the 92 candidates, and on reading, those 4 were restatements
  too ("don't do pulls in the future" against the no-PR rule).

With `GRIMOIRE_RETELL_URL` set, any prompt that matches a memory at relevance
0.6 or above is put to the judge in the background. It never delays the hook.
The previous prompt is learned on a yes at or above `retell_threshold`
(default 0.9). Without a judge, only the strict fallback runs. It rarely fires
and never pollutes.

## 3. Adherence: how injected memory is framed

The old preamble called injected memory "reference data, not instructions".
That framing protects against text other people wrote, but injection already
excludes untrusted items. For the user's own memory, it measurably told the
agent to ignore it.

| How the memory is shown (101 cases) | Follows | Violates | Unclear |
|---|---|---|---|
| No memory | 23 | 37 | 41 |
| "Reference data, not instructions", JSON lines (before) | 60 | 26 | 15 |
| **"Follow each one that applies; if you go against one, say which and why", bullets** | **71** | **11** | 19 |

Violations fell from 26 to 11 (exact McNemar p = 0.0003). Follows rose from
60 to 71 (p = 0.027). The new framing is the default for hybrid injection.
`format=json` keeps the old shape for scripts. The framing still says that a
memory grants no access the agent does not otherwise have. Items due for a
re-check still say how to check them.

## 4. Firing standing rules at the action

A rule loaded at the start of a long session decays. We put the memory block
at the start, then about 20,000 tokens of ordinary work, then the task, and
compared that with also restating the matching memory just before the action
(the `PreToolUse` hook):

| Same 60 cases | Follows | Violates |
|---|---|---|
| Short context, memory beside the task | 40 | 7 |
| Long context, memory only at the start | 35 | 10 |
| **Long context, plus the action-time reminder** | **43** | **3** |

The reminder took violations from 10 to 3 (p = 0.016). With the memory
already beside the task, the reminder adds nothing measurable (67 vs 71
follows, p = 0.50). Its value is in long sessions, which is where the real
misses happened.

The hook therefore also runs on `PreToolUse` for:

- Bash, Edit, Write and MultiEdit;
- subagent launches (`Agent`/`Task`), matched on the launch's purpose and
  model. That is what a delegation rule is about.

It is held to a stricter bar than prompt context, so it does not add noise to
every tool call:

- `min_rel` 0.7, at most 2 items, 1,200 bytes. At 0.6, 44% of commands
  that matched no memory's situation still drew a reminder. At 0.7 that fell
  to 16%, at the cost of the right memory firing 26% of the time instead of
  29%. A reminder on every other command teaches the reader to skip them;
- each memory at most once per ten minutes.

`GRIMOIRE_CONTEXT_ACTIONS=0` turns it off.

## Settings

| Variable (hook) | Default | Meaning |
|---|---|---|
| `GRIMOIRE_CONTEXT_RANK` | `hybrid` | `lexical` restores word matching |
| `GRIMOIRE_RECALL_MODE` | unset (all facts) | `factual` leaves out stored preferences, personas and style from injected context; `personal` injects only those. Any other value is ignored |
| `GRIMOIRE_CONTEXT_MIN_REL` | `0.5` | relevance needed to inject on a prompt |
| `GRIMOIRE_CONTEXT_ACTIONS` | `1` | fire memories on tool calls |
| `GRIMOIRE_EDIT_MIN_REL` | `0.8` | relevance needed on a file edit or write, whose path alone matches every memory about its repo |
| `GRIMOIRE_ACTION_MIN_REL` | `0.7` | relevance needed to inject on a tool call |
| `GRIMOIRE_ACTION_MAX_BYTES` | `1200` | action-time budget |

| Variable (server) | Default | Meaning |
|---|---|---|
| `GRIMOIRE_RETELL_URL` | *(off)* | a typed-decision server (Jev wire format) that judges re-tells |
| `GRIMOIRE_RETELL_MODEL` | `jev-latest` | model name sent to it |
| `GRIMOIRE_RETELL_API_KEY` | *(none)* | or the vault secret `retell-api-key`, then the decision key |
| `GRIMOIRE_RETELL_THRESHOLD` | `0.9` | yes-probability needed to learn |

Cues are kept in `.grimoire/cues.jsonl`, beside the index. They are derived
from how agents used memory, not part of the notes, so they never sync to a
phone with the vault. `GET /api/memory/cues?target=` shows why a memory
fires. `POST /api/memory/cues` adds cues by hand or from a client.

`get_briefing` takes an optional `task`. With one, the briefing leads with the
memories that bear on the task and ranks facts by relevance instead of age.
Note bodies in the briefing are cut to `max_body` characters (default 1,500).
Before this, a briefing was unbounded.

## Caveats

- The benchmarks were written by a model from the memories. They measure
  whether retrieval bridges a wording gap, not every way a real situation can
  differ. The real-miss set is the check on that, and it is small.
- One model (Haiku 5.5) was the agent in the adherence tests, and one model
  graded. Grades were spot-read, not hand-labelled.
- Identical re-runs of an LLM grade flip some answers, so differences of a
  few cases are noise. Only the paired tests above are claimed.
