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
| `used` | one of the memory's fingerprints appeared in what the agent did or said, with no tag cited (see Fingerprints) |
| `ignored` | injected, fingerprintable, and none of the above by the end of the window |
| `unknown` | the memory has no fingerprint, so nothing could show use. Not `ignored` |

Contradiction hooks into the existing re-tell paths (`learnFromRetell`,
`learnFromPrompt`), so it fires on a `remember` that turns out to be on file
and on a prompt the re-tell judge calls a restatement.

Request bodies: `{session, tool, target}` for a tool call, and
`{session, cited:[tags], fp:{tag:n}, stop:true}` at the end of a turn (`fp`
may also ride on a tool call). `{..., pre:true}`
computes a permission decision and records nothing. The hook never sends
transcript text; only tags, tool names, targets and fingerprint match counts
leave the machine.

### The hook

`clients/hooks/grimoire_outcome.py` handles `PostToolUse` and `Stop` for
claude-json-style events (register it beside `grimoire_context.py`; the
session hash it sends is the same one the context hook sends). On `Stop` it
reads `transcript_path`, takes the assistant text since the last real user
prompt, and extracts only the `m:` tags and fingerprint match counts. It fails
silently.
`GRIMOIRE_OUTCOME=0` turns it off.

The outcome hook also feeds the **utilization trace**: whether a memory that was
used led to a better outcome, with outcome codes per action, a natural experiment
at the action stage and an optional randomised holdout. See
[MEMORY_TRACE.md](MEMORY_TRACE.md).

## Fingerprints: use without a tag

Agents rarely write the `(m:3e99)` tag (qwen 14% of the time even when asked), so
a tag cannot be the main signal. The memory's own content is: if the memory says
to run `~/tailscale-helpers/snapshot.sh` and the agent's next command contains
`snapshot.sh`, it used the memory.

**Selection (server, at injection).** For each injected item
(`go/internal/fingerprint`): take its distinctive tokens, namely paths and their
components, URLs and hosts, IPs and ports, `--flags`, ENV_VARS, versions,
code-ish identifiers (`snake_case`, camelCase, letters+digits, `file.ext`),
hyphenated names, and plain words inside backticks. Score each by rarity across
the whole memory store (idf of its document frequency, times a weight per kind:
backticked plain words and hyphenated compounds weigh least, because `read-only`
is English and `tailscale-helpers` is a name). Drop a token that is too common
(in more than 2% of memories, between 2 and 12), generic (`home`, `config`,
`return`...), or **already present in the situation** (the prompt or command that
triggered the injection, or a distinctive piece of the token, such as the file
name of a path): an answer repeating the prompt proves nothing. Keep at most 6,
at most 2 from the same run of text. A memory left with none is
**unfingerprintable**; nothing is guessed. Document frequencies are cached for 10
minutes.

**Wire.** The hybrid directive response gains, beside `tags`:

```json
"fp": {"v": 1, "spec": "<normalisation text>", "salt": "<16 hex, fresh per response>",
       "items": {"3e99": ["2bba5360fd", "db783cf87a"]}, "none": ["a1b2"]}
```

`items` maps a tag to `sha256(salt NUL token)[:10]` for each fingerprint; `none`
lists the unfingerprintable tags. No token or memory text is in it. The context
hook stores it per session (`fp-<session hash>.json` beside its other state: at
most 24 items, 8 hashes each, hashes only).

**Normalisation spec (v1)**, shared by server and hook: lowercase ASCII letters
only; runs of `[a-z0-9_./:~@%+#$=-]`; strip leading `[:=+#%@]` and trailing
`[.:=-/+#%@]`; keep runs of 3 to 160 characters; also emit the run without a
leading `$`, pieces split on `=`, for a URL its host and `host:port` and path
components, for a path every component of 3+ characters, for `a:b` the pieces of
3+ characters. `clients/hooks/tests/fingerprint_vectors.json` is run by both the
Go and Python suites, so the two implementations cannot drift; bump
`SpecVersion` if it ever changes.

**Matching (hook).** `grimoire_outcome.py` normalises the tool target on
`PostToolUse` and the assistant text since the last prompt on `Stop` (read from
`transcript_path` locally), hashes every candidate with each item's salt, and
compares. It sends **only `fp: {tag: distinct fingerprints matched so far}`**,
never text, tokens or hashes. The server clamps a count to the number of
fingerprints it sent. A match is `used` (`adherence.UsedMin = 1`).

**Window.** A memory is watched through the turn it was injected in and one turn
after (`GRIMOIRE_FP_WINDOW_TURNS`, default 1; `GRIMOIRE_FP_WINDOW_TOOLS=N`
optionally also stops after N tool calls). The hook counts turns at `Stop`. A
match in the second turn upgrades an injection the first `Stop` closed as
`ignored` (counted helpful once). `GRIMOIRE_FINGERPRINTS=0` turns it off. Only
prompt-stage injections are logged today, so action-stage injections carry no
fingerprints until they are.

**Report.** `GET /api/memory/adherence` adds `used`, `unknown`, `used_rate` (share
of finished fingerprintable injections that matched) and `fingerprint_coverage`
(share of injections that were fingerprintable), overall and per memory.

### Measured offline (no model calls)

`benchmarks/memory_use/round2/fp_eval.py` runs the production Go selector
(`go/cmd/fpdump`) and the production hook matcher, with the salted hashes of the
wire, over the 101 adherence cases (corpus: 349 memory texts, situation = the
task text; memories as shown, first 900 characters):

* **Coverage: 78.2%** of memories are fingerprintable (79/101); 15 have the full
  6. The 22 that are not are advice in plain words ("prefer small commits").
* **False positives, answers produced without the memory** (should not match):
  **2.1%** of 240 qwen no-memory answers (0.0% at 2 matches), **7.5%** of 80 Haiku
  no-memory answers (1.2% at 2). Among no-memory answers that follow the rule anyway
  (they land on the same path or command), `used` fires on 33%, which is expected;
  on those that do not follow, 2.5%.
* **Agreement of `used` with the follow labels** (precision = P(follows | used),
  recall = P(used | follows), at one match):

| Answers | n | precision | recall | used rate vs follow rate |
|---|---|---|---|---|
| qwen, memory arms A-D (deterministic follow) | 348 | 57.1% | 68.2% | 50.9% vs 42.5% |
| Haiku marker runs A+B | 52 | 64.3% | 81.8% | 80.8% vs 63.5% |
| Haiku adherence: current + directive + recheck (LLM-graded) | 240 | 82.2% | 67.7% | 56.2% vs 68.3% |
| Haiku directive arm alone | 80 | 89.1% | 81.7% | 68.8% vs 75.0% |
| Haiku long-horizon (start + action) | 104 | 75.0% | 63.6% | 53.8% vs 63.5% |

* **Agreement with right-cites** (qwen arms B-D, 720 answers): recall 54.7% (of
  answers that cited the right tag, 55% also show a fingerprint), precision 24.2%,
  because qwen cites the tag in only 18% of answers while using the content in 40%.
  For Haiku arm B, 21 of 26 right-cites also matched (recall 80.8%), and no
  answer matched without having cited.
* **Two matches instead of one** trades recall for precision: qwen A-D precision
  54.7% recall 31.8%; Haiku current+directive+recheck 85.3% / 35.4%. One match is
  the default because a fingerprint is already rare in the store.

`used` is not `followed`: it says the memory's content showed up in the action
(the agent touched the path, ran the command, named the host), not that the
action was right. It is the evidence that an injected memory was read and acted
on, which is what the injection-usefulness counters need; `followed` and
`violated` remain the checks' verdicts.

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

Outcomes feed the existing feedback counters of a fact: cited, followed or
used counts helpful; a contradiction counts unhelpful once. Violated, ignored and unknown
move no counter (a violation says the agent erred, not that the memory is
wrong). A memory ignored five or more times with under 20% of its finished
injections acted on (cited, followed or used; unfingerprintable injections are
left out of the denominator) is down-ranked for **injection only**, by a factor
falling from 0.9 to 0.6 at twenty ignores. Recall and search are untouched.

`GET /api/memory/adherence?days=30` reports per memory and overall:
injected, cited, followed, used, violated, ignored, unknown, contradicted,
pending, the acted-on rate, the used rate and fingerprint coverage, and any
injection penalty, plus the gate's latency.

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

### Local gate: a distilled cross-encoder (no extra service)

`context_gate_local` (env `GRIMOIRE_CONTEXT_GATE_LOCAL`) points the gate at a
model run **inside the Go binary** (`go/internal/rerank`, the same pure-Go BERT
the reranker uses). It is used when `context_gate_url` is empty; a configured
decision server takes precedence. Value: a model directory, or a name under
`<vault>/.grimoire/models/`. The model is not committed or published: it is
`config.json`, `tokenizer.json`, `model.safetensors` (91 MB) built by
`benchmarks/memory_use/round2/gatemodel_{labels,train,eval,fixture}.py`
(this install: `/mnt/bulk/memory-use-research/round2/gatemodel/model`).
`context_gate_local_threshold` (default `-0.3`) is on the raw logit.

How it was made: `ms-marco-MiniLM-L-6-v2` fine-tuned (6 epochs, lr 5e-5) on
5,577 (request, memory) pairs from the round-1 cases only (gold memory, the
top 5-7 hybrid non-matching candidates, 2-3 random ones; 48 round-1 cases that
share a word 5-gram with any round-2 prompt were dropped), each labelled by Jev
with the strict question above (5,577 calls). The target is
`sigmoid(logit(p_jev) - logit(0.16))`, so logit 0 is Jev's calibrated point.
Round 2 is held out by prompt; the memory store itself is shared.

Round-2 band pairs (gold score 0.35-0.9; 222 right, 109 wrong), threshold set
to keep 86% of the right memories:

| Gate | AUC | near-miss memories removed at 86% kept | latency |
|---|---|---|---|
| Jev strict | 0.844 | 59.6% | ~280 ms, network |
| untuned MiniLM | 0.681 | 25.7% | - |
| **distilled MiniLM** | **0.783** | **39.4%** | **1 / 3 / 5 candidates: p50 15 / 27 / 36 ms, p95 20 / 34 / 47 ms (Go, CPU)** |

Python and Go scores agree to 3e-6 (`TestGateModelMatchesReference`,
`TestGateRound2`, which need `GRIMOIRE_GATE_MODEL_DIR`). With a threshold fitted
on round 1 instead (no peeking), round 2 keeps 80.6% and removes 49.5%.
It beats the untuned model clearly and is 0.06 AUC below Jev, so it stays
**off by default**: on this benchmark it removes about 40% of wrong memories
for 14% of right ones, in 30 ms instead of 280 ms.

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
