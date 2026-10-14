# Rules compiled into checks

[MEMORY_ADHERENCE.md](MEMORY_ADHERENCE.md) lets a rule carry a hand-written
`forbid`/`require` regex and, for a hard rule, `enforce: ask`. Writing those by
hand does not scale, and the older proposal pass only asked a model whether a
guessed pattern looked right. This page describes the other route: every rule
memory (kind `rule`, see [MEMORY_STORE.md](MEMORY_STORE.md)) is **compiled** into
candidate checks automatically, each candidate is **measured against your own
transcript history**, and only checks that turn out precise are allowed to act.

```
rule text --compile--> candidate checks --backtest--> matches in real history
                                                        |  sample, label
                                                        v
             suggestion  <-- precision below bar     precision + 95% interval
             reminder    <-- precision >= 0.90, >= 5 labelled
             enforce:ask <-- "never/always" in the rule AND precision >= 0.95
```

Nothing here calls a hosted Claude model. Candidates come from the rule's own
words and from a local Ollama model; the only hosted model is the optional
labeller (Jev, or any decision server), which sees one flagged call at a time.

## Check shapes

| Shape | Reads as | Violated when |
|---|---|---|
| `forbid` | "never `git push --force`" | a call matches `action` |
| `require_before` | "run `go test` before `git push`" | a call matches `action` and no earlier call in the same session matched `before` (a compound `go test && git push` counts) |
| `require_with` | "always pass `-y` to apt" | a call matches `action` and does not also match `with` |

Every shape can carry a **scope**: `tools` (default `Bash`; use `Edit`, `Write`
for file paths), `scope.cwd` (a regex over the session's working directory, for
"in the lectern repo"), and `scope.agent`. Patterns are Go RE2, so matching is
linear time. Compilation rejects patterns over 200 bytes, patterns that match the
empty string or nearly everything (`.*`), lookahead and backreferences (not RE2),
and model output that is not the expected JSON. Matching only reads the first
4 KiB of a target.

## Compile

`grimoire rules compile [--dir STORE] [--prefix "Agent Memory/"] [--no-llm]`

For each rule: deterministic extraction first (backticked commands and flags,
the command after a negation, `before` and `without` clauses, an explicit
"in the X repo" scope, a path after "never edit"), then, when `rules_llm_url`
is set, a local model (default `qwen3.6:35b-a3b`, `num_ctx` 16384, thinking off,
plain JSON reply validated in Go) is asked for up to three more. Duplicates
merge; at most four candidates are kept per rule. A rule that no tool call can
reveal ("keep answers short") compiles to nothing, which is the right answer.
Recompiling keeps the measurements of any candidate that did not change.

Run the model pass while holding the shared GPU lease, as the other research
jobs do:

```
GRIMOIRE_RULES_LLM_URL=http://100.127.85.58:11434 \
  /mnt/bulk/inference-research/floor/research-lease.sh grimoire rules compile
```

## Backtest

`grimoire rules backtest [--since 90d] [--agent NAME] [--budget 1500] [--no-label]`

1. Reads the transcript history of every agent with a profile, keeps
   **interactive sessions only** (the same filter as `memory impact --retells`:
   headless, benchmark and automation sessions are dropped) and removes tool
   calls replayed by resumed sessions.
2. Runs every check over every tool call in one pass, in session order, and
   records calls scanned, calls that were the rule's subject, matches, sessions
   affected and match rate.
3. A check that flags more than `rules_max_match_rate` (20%) of the calls it
   covers is a topic detector, not a violation detector: it is never labelled
   and never acts.
4. For the rest, the labelling model is asked two things. First, **once per
   check**: "is this check a faithful reading of the rule?" (`rules_faithful_threshold`,
   0.5). A model asked only "did this call break the rule" says yes to almost
   anything a check flagged, including checks that invent a requirement the rule
   never states ("go test must carry `-run Enforcement`" was labelled a violation
   10 times out of 10), so a check that fails this question stays a suggestion and
   its matches are not labelled. Second, for faithful checks, **at most 20
   matches per rule** (split across its checks, spread over distinct sessions
   first, deterministic): "did this call actually break the rule?", given the
   call, the rule, and the user's last message before the call (so a rule that
   says "unless asked" is not counted against a push the user requested).
   Answers are cached on disk (`.grimoire/rules-labels.jsonl`, keyed on model,
   rule, check and call), and at most `rules_label_budget` (1500) new calls are
   made per run.
5. Precision = labelled true violations / labelled matches, with a 95% Wilson
   interval. Re-tells of the rule recorded in the outcome log (`contradicted`)
   are reported per check as **retold**: the user had to say it again, so these
   are the misses the check could not have prevented if it had not fired.

The labels are a model's, not ground truth. Treat the interval, not the point
estimate, as the claim; labelled-sample size is shown next to every precision.
Transcript text is redacted by the transcript readers before it reaches the
labeller, and the key is read from the environment, never printed.

## Activation policy

| Status | Earned by | Effect |
|---|---|---|
| `suggestion` | everything else | listed, never acts |
| `active` | precision >= `rules_min_precision` (0.9) on >= `rules_min_labelled` (5) labelled matches | the rule is injected with its tag whenever the check fires at action time |
| `enforce` | `active`, plus the rule text says never/always/must (not "avoid" or "prefer"), plus precision >= `rules_enforce_precision` (0.95) | also returns `permission: ask` |
| `disabled` | `grimoire rules disable ID` | nothing |

`grimoire rules enable ID` activates a check whatever its numbers;
`--enforce` also makes it ask. A person's choice is never auto-demoted.

**Live outcomes keep updating precision.** Every firing of an active check is
recorded. A re-tell of the rule after a firing labels the newest open firing a
true violation automatically; `grimoire rules label FIRING_ID violation|false-alarm`
(or `POST /api/memory/rules/label`) records a person's judgement. Live labels join
the historical ones, the status is re-decided each time, and a check that falls
below the threshold is **demoted** back to a suggestion (`reason: demoted: ...`).

## Wiring

The server evaluates active checks in the same places as hand-written ones:
the action-stage context response (`tool`, `target`, `session`, and optional
`cwd` and `agent` query parameters) and `POST /api/memory/outcome` (`pre:true`
computes the decision; the post-tool call records firings, marks the injected
rule `violated`, and appends the call to the session history `require_before`
reads). `cwd` and `agent` are optional in the outcome body; a check with a scope
does not fire when the caller did not say where it is. History is held in memory
per session (256 sessions, 400 calls each) and is not persisted.

### Deny once (experimental)

**Why.** A `PreToolUse` reminder (`additionalContext`) lasts one turn and is
often ignored. A deny with a reason stays in the context the agent reads, and
changes what it does. Grimoire's default is to ask only: a poisoned memory must
not be able to block tools. So deny exists only behind two opt-ins, and both
are off by default.

**The two opt-ins.**

- Server: `GRIMOIRE_EXP=denyonce` (experiment flag, see [CONFIG.md](CONFIG.md)).
- Hook: `GRIMOIRE_HOOK_ALLOW_DENY=1` in the environment of the context hook.

**What happens when both are on.** A compiled check at status `enforce` (its
precision met the enforce threshold and its rule says never or always) that
fires on a pending action returns a permission `deny` with `once: true` and a
one-line reason: the rule text and why it is enforced. It does this for the
**first** firing of that check in that session. Later firings in the same
session ask, exactly as before. The "already denied" record is kept per
(session hash, check id) in memory, bounded at 10,000 pairs with the oldest
forgotten first. A server restart forgets it.

**Safety.**

- A deny needs both opt-ins. With the server flag and no hook opt-in, the hook
  shows the same reason as an `ask`, so a person is still prompted. A deny is
  never honoured by accident, and `allow` is never honoured at all.
- Only a rule with **human authority** can deny: a memory a person wrote or
  hand-edited. An agent-written or pulled memory asks, as it always has. A
  note target has no authority of its own, so it never denies.
- Only `enforce` status can deny. Active reminders and suggestions never do.
- Once per session. A rule that is wrong costs one blocked action, not a
  session that cannot proceed; the agent is told which rule applies.
- The outcome hook's own `PreToolUse` path (`GRIMOIRE_OUTCOME_ENFORCE=1`)
  never receives a deny. It asks with `pre:true`, and that call does not spend
  the once-only deny.
- A session that is not identified cannot deny, because there is nothing to
  scope the once to.
- The server cannot see whether the hook applied the deny. The deny is recorded
  when it is answered. If the hook did not opt in, the person saw an ask and
  the next firing asks too, which is the behaviour without the flag.

**Tests.** `go/internal/api/memory_denyonce_test.go` covers flag off, the first
deny then ask, agent and non-enforce rules, the pre-decision not spending the
deny, and the bound. `clients/hooks/tests/test_context_hook.py` covers a deny
ignored without the env (shown as ask), honoured with it, and that only the
exact value `1` opts in.

## CLI and API

```
grimoire rules compile|backtest|list|enable|disable|label
```

| Endpoint | |
|---|---|
| `GET /api/memory/rules[?status=active]` | checks with evidence and status |
| `POST /api/memory/rules/compile` `{dir?, prefix?, llm?}` | |
| `POST /api/memory/rules/backtest` `{since?, agent?, label?, budget?}` | |
| `POST /api/memory/rules/enable` `{id, enforce?}` / `/disable` `{id}` | |
| `POST /api/memory/rules/label` `{firing, violation}` | |

All are administrator-only: they read transcripts and change what agents are
asked. Settings are in [CONFIG.md](CONFIG.md) (`GRIMOIRE_RULES_*`).
State lives in `.grimoire/rules.db` beside the adherence log (derived data,
not part of the notes). Hand-written checks and `check/propose` are unchanged
and take precedence in the sense that they are separate: a rule can have both.

## A first run (2026-10-09, copy of the live store, 90 days of history)

41 rule memories; 106 checks compiled for 33 of them (23 deterministic, 83
from qwen3.6; 8 rules compiled to nothing, mostly style rules; 3 model replies
were not valid JSON and were dropped). Backtest over 97 interactive sessions
and 56,167 tool calls (26,821 headless, benchmark and automation sessions were
excluded): 62 checks matched something, 44 matched nothing, 1 was a topic
detector. The faithfulness question rejected 50 of the 61 it was asked about
(checks that invented a flag, or only shared a topic with the rule).
Four checks reached five labelled matches; precision 0.55, 0.80, 0.80 and 1.00.
One auto-activated, at `enforce`: `pgrep -f` (precision 1.00, 95% interval
0.72-1.00, n=10). `git push` against "never push unless I ask" came out at 0.55
(n=20) and stayed a suggestion: most pushes in the history were requested. About
520 labelling calls were spent. The outcome log was empty (the outcome hook had
not been live), so the re-tell column read 0.

These precisions are the labelling model's judgement, not a hand audit, and
with n=10 the interval is wide: read the interval, and enable by hand
(`grimoire rules enable ID`) anything you have verified yourself.
