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

## CLI and API

```
grimoire rules compile|backtest|list|enable|disable|label|receipts
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

## Receipts

`grimoire rules receipts [--status S] [--since 90d] [--agent NAME] [--before YYYY-MM-DD] [--json]`

Receipts answer a different question from precision. Precision asks whether a
check's flags were real violations. Receipts ask how often the agent's tool
calls broke each rule across the history, whether or not the check fired.

For each stored check, receipts walk the same interactive corpus as backtest, once,
in session order. They count:

- **sessions**: sessions with at least one call in the check's tools and scope;
- **actions**: calls that were the rule's subject;
- **violations** and **compliant** actions: actions that broke the rule, and
  actions that kept it, using the same `Violates` logic as the live check;
- **violation rate** = violations / actions, with a Wilson 95% interval.

It also prints labelled precision when the check has labels, and per-day totals
(sessions, tool calls, violations summed over checks). It reads the local store
and transcripts and calls no model.

**Why citation is not adherence.** Earlier we asked whether the agent cited the
memory it was given. It cited about 0.2% of the time, which says little about
whether it followed the rule. A rule can be followed without being mentioned,
and a mention can come with a violation. Receipts measure the calls themselves.

**The holdout guard.** A randomised memory holdout runs from 2026-10-11 to
2026-11-08 (`benchmarks/holdout/PREREG.md`). Its outcomes must not be read by arm
before it ends. `--split-injected` (outcomes split by whether the rule was
injected earlier in the session) is therefore refused while the holdout runs,
with a non-zero exit. The one exception is `--before 2026-10-11`, which restricts
the history to sessions that started before the holdout. The split itself is not
built yet and stops with "not implemented: join to the injection trace" after the
guard passes.

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
