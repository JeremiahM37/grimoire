# Did the agent use the memory it was shown?

[MEMORY_USE.md](MEMORY_USE.md) is about getting the right memory in front of
an agent. This document is about what happens next: whether the agent acted
on it, how that is measured without a grader model, and what Grimoire does
with the answer.

## Markers

Every item injected in directive form carries a short tag, `m:3e99`: a prefix
of the item's key (at least four hex characters, longer only as far as needed
to be unique within the response). The preamble asks the agent to cite a tag
once when an item changes what it does, for example `(m:3e99)`. Tags are
deterministic, so a grader needs no model to resolve one. `format=json` is
unchanged: no markers, no `tags` field.

## Injection log

Every hybrid injection that carries a `session` is written to a bounded
SQLite table (`.grimoire/adherence.db`, beside the index; derived telemetry,
never part of the notes): session hash, tag, key, target, stage, relevance,
time. Rows older than 30 days are dropped, and the table is capped at 200,000
rows.

## Outcomes

`POST /api/memory/outcome` takes what the agent did. Each injected item ends
as exactly one of:

| Outcome | Meaning |
|---|---|
| `violated` | its check forbade what the agent then did (wins over all others) |
| `contradicted` | a later re-tell of the same memory arrived: the user had to say it again |
| `cited` | the agent cited its tag |
| `followed` | its check passed (the forbidden pattern never appeared across a turn with tool calls, or the required one did) |
| `ignored` | none of the above by the end of the turn |

Contradiction hooks into the existing re-tell paths (`learnFromRetell`,
`learnFromPrompt`), so it fires on a `remember` that turns out to be on file
and on a prompt the re-tell judge calls a restatement.

Request bodies: `{session, tool, target}` for a tool call, and
`{session, cited:[tags], stop:true}` at the end of a turn. `{..., pre:true}`
computes a permission decision and records nothing. The hook never sends
transcript text; only tags, tool names and targets leave the machine.

### The hook

`clients/hooks/grimoire_outcome.py` handles `PostToolUse` and `Stop` for
claude-json-style events (register it beside `grimoire_context.py`; the
session hash it sends is the same one the context hook sends). On `Stop` it
reads `transcript_path`, takes the assistant text since the last real user
prompt, and extracts only the `m:` tags. It fails silently.
`GRIMOIRE_OUTCOME=0` turns it off.

The outcomes also build the corpus for [memory replay](MEMORY_REPLAY.md): a
memory that was cited or followed in a situation is one a later change to the
store must keep firing there.

## Checks

A memory can carry a check, stored beside the adherence log (not in the
note's frontmatter, so it cannot collide with the memory store's own
metadata) and editable by hand:

```
POST /api/memory/check  {"target":"fact:7590362a9817",
                         "forbid":"\\bgit\\s+push\\b",
                         "enforce":"ask", "tools":["Bash"]}
GET  /api/memory/check            DELETE /api/memory/check?target=...
```

`forbid` and `require` are Go (RE2, linear-time) regexes over the tool call's
target: the command for Bash, the path for Edit and Write, the launch
description for subagents. `tools` limits which tools it applies to. Setting a
check takes write access to the memory.

### `enforce: ask`

For a hard rule, `enforce: "ask"` (with a `forbid`) makes the action-stage
context response carry

```json
"permission": {"decision": "ask", "reason": "Grimoire rule: ... "}
```

whenever the action matches the pattern. This is deterministic: no embedding,
no model, and it applies whatever the rule's relevance score, so a rule is
not missed because the query shared no words with it. The rule is also
injected, with its tag. Grimoire can only ask; the hook never emits allow or
deny from it.

Wiring, either one:

1. **Context hook (WS1).** In `action_context`, when `result.get("permission")`
   is a dict with `decision == "ask"`, add to the `hookSpecificOutput` it
   returns:
   `"permissionDecision": "ask", "permissionDecisionReason": permission["reason"]`.
   Return that output even when `context` is empty.
2. **Outcome hook.** Register `grimoire_outcome.py` on `PreToolUse` and set
   `GRIMOIRE_OUTCOME_ENFORCE=1`. It asks the server (`pre:true`) and prints the
   permission output. This costs one extra local request per Bash/Edit/Write
   call, so prefer option 1.

### Proposing checks

`POST /api/memory/check/propose` (admin) is an offline pass over rule
memories (category `rule`, or text with never/always/must) that have no
check. The decision wire format answers yes/no, choice and score questions; it
cannot write a pattern. So candidate patterns are derived deterministically
(code spans, and the command after "never", "don't", "avoid") and the
configured decision model (`decision_url`) is asked whether a command
matching each one would be breaking the rule. Patterns at probability >= 0.7
are stored as **suggestions** (`GET /api/memory/check?suggestions=1`), never
applied. `POST /api/memory/check/accept {"target", "enforce"}` is a person's
act. Rules with no command in them ("never use mocks") get no proposal; a
regex over a tool call cannot see them.

## Usefulness

Outcomes feed the existing feedback counters of a fact: cited or followed
counts helpful; a contradiction counts unhelpful once. Violated and ignored
move no counter (a violation says the agent erred, not that the memory is
wrong). A memory ignored five or more times with under 20% of its finished
injections acted on is down-ranked for **injection only**, by a factor
falling from 0.9 to 0.6 at twenty ignores. Recall and search are untouched.

`GET /api/memory/adherence?days=30` reports per memory and overall:
injected, cited, followed, violated, ignored, contradicted, pending, the
acted-on rate and any injection penalty, plus the gate's latency.

## Gate: when not to inject

Off by default. Settings (env `GRIMOIRE_CONTEXT_GATE_*`):

| Setting | Default | Meaning |
|---|---|---|
| `context_gate_url` | *(off)* | a decision server (Jev wire format; local Laya at `http://127.0.0.1:8765` speaks it) |
| `context_gate_model` | `jev-latest` | model name sent to it |
| `context_gate_band` | `0.5,0.75` | relevance range that is asked |
| `context_gate_max` | `3` | most candidates asked per request, highest first |
| `context_gate_threshold` | `0.16` | yes-probability needed to keep a memory |

Above the band injects, below never does. Inside it, each candidate that
would otherwise be injected is put to the model ("would this memory change
what the agent does for this request?"), in parallel, under a hard 400 ms
total budget. A candidate with no answer in time keeps its score-rule fate.
Every pass is recorded (latency, kept, dropped, timed out).

### Round-2 benchmark and the defaults

Scripts: `benchmarks/memory_use/round2/gate_*.py` (results: `gate_report.json`) (collect candidates, score
labelled pairs, latency, report). 180 labelled (memory, request) pairs
(138 right, 42 wrong) in the relevance band, scored by Jev with two question
wordings:

| Question | AUC | at threshold 0.5: right kept / wrong removed | calibrated threshold: right kept / wrong removed |
|---|---|---|---|
| one line, "would this change what the agent does" | 0.745 | 82.6% / 54.8% | 0.37: 91.3% / 28.6% |
| **strict**: "directly about the task ... sharing a topic is not enough", with true/false criteria | **0.798** | 56.5% / 88.1% | **0.16: 86.2% / 42.9%** |

The strict wording separates better but answers with low probabilities, so
its threshold is **0.16**, not 0.5. The shipped gate uses the strict wording,
`context_gate_threshold=0.16`, `context_gate_max=3` in parallel and the 400 ms
budget (Jev p50 277 ms, p95 374 ms). It stays **off by default**: even
calibrated it removes about 40% of the wrong memories at a cost of 10-14% of
the right ones, a trade for each operator to choose. Laya's local checkpoint
does not suit the gate (below).

### Measured on local Laya

50 context requests against a test server on a copy of this vault, Laya
(`volatility` checkpoint) on `127.0.0.1:8765`, machine under ordinary load:

| `context_gate_max` | gate calls | timed out | p50 | p95 |
|---|---|---|---|---|
| 1 | 38 | 4 | 361 ms | 400 ms (the cap) |
| 2 | 38 | 38 | 400 ms | 401 ms |
| 6 (first try, long question) | 41 | 31 | 400 ms | 401 ms |

Laya answers one question at a time and its latency grows with the tokens it
reads (about 3 ms each; ~170 ms for a 58-token state, ~300 ms for 106). Two
candidates in parallel do not fit in 400 ms, so a Laya gate is effectively
one candidate per request. That checkpoint was fine-tuned for volatility, not
for "does this apply": its probabilities for relevant and irrelevant pairs
were low and close together (0.24 vs 0.04 on a right and a wrong memory), so
the threshold needs calibrating before the gate does anything useful. Do not
turn it on until the retrieval benchmark shows it wins.
