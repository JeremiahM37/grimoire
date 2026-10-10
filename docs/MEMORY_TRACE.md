# Was the memory used, did it change what the agent did, and was that good?

[MEMORY_ADHERENCE.md](MEMORY_ADHERENCE.md) answers "was this memory acted on".
This page follows one injection further, for any agent with a profile and with no
cooperation from the agent:

```
exposure -> uptake -> influence (linked to a concrete action) -> outcome -> benefit
```

Every number is labelled **associated** (observational) or **caused** (from a
randomised holdout) and carries its n and a 95% interval. Nothing here calls a
model, and nothing stores text: the trace holds hashes, ids and small codes.

## What is recorded

All of it lives in the adherence database (`.grimoire/adherence.db`), bounded to
30 days and a row cap per table.

| Stage | Table / column | Evidence |
|---|---|---|
| exposure | `injections` | one row per memory shown: session hash, tag, kind, stage, relevance, the holdout probability it faced |
| uptake | `injections.cited`, `fp_hit`, `meaning` | the tag cited, how many of the memory's fingerprints appeared, an optional meaning-match score (0-1, from a client or judge that compared the memory with what the agent did; `POST /api/memory/outcome {"meaning": {"tag": 0.8}}`) |
| actions | `trace_actions` | one row per executed tool call: tool, sequence number, session stage, the tool_use id, a hash of the target, outcome codes |
| influence | `trace_links` | `injection -> action` with the evidence: `fp`, `tag`, `check`, `changed` |
| reminders | `injections.pend`, `rem` | the action-stage natural experiment (below) |
| holdout | `trace_withheld` | items withheld and the probability, so a would-have-injected row exists for them |

An action's stage is its position in the session: `early` (calls 1-10), `mid`
(11-40), `late`.

## Influence: which action did the memory touch

An action counts as influenced by memory M when it carries M's uptake evidence
within two hours of the exposure:

* a fingerprint of M appears in the call itself (`ev`, counted for that call
  alone; the cumulative `fp` counts the adherence log uses cannot say which call),
* the call carries M's tag (a commit message that says `(m:3e99)`),
* M's `require` check is met by the call.

### The action-stage natural experiment

When the context hook injects at `PreToolUse` for a pending call P, the server
stores P's hash (never P) and the tool_use id when the harness sends one. The
next executed call settles it:

| Resolution | Meaning |
|---|---|
| `unchanged` | the call that ran has P's hash: the reminder did not change the plan |
| `changed` | a different call ran. This is strong influence evidence and is linked to that call with `changed` |
| `abandoned` | the turn ended and P never ran |

With tool_use ids the match is exact, so parallel calls do not read as changes.
Without them the call that follows within two minutes (thirty for an ask) is the
one, and a call that matches any pending reminder resolves that one rather than
its neighbour. A rule that forced an `enforce: ask` decision is recorded with
`ask`: `unchanged` means the user approved and it ran, `abandoned` that it never
ran (declined or dropped). Limit: for `Edit` and `Write` the pending hash covers
the tool and path, so a different edit of the same file reads as `unchanged`.

## Outcomes (the hook, per profile)

`grimoire_outcome.py` runs on `PostToolUse`, on the agent's failure event
(`PostToolUseFailure` in Claude Code), on the next prompt and on `Stop`. Events,
tool names and payload fields come from the agent profile (`--agent NAME`), so
an agent described only by a profile file works; `event_fields.tool_response` and
`event_fields.tool_use_id` name those payload fields if an agent differs from the
defaults (`tool_response|tool_result|error`, `tool_use_id|toolUseId|call_id|...`).
It reads the response locally and sends **codes and counts only**:

| Code | From |
|---|---|
| `err`, `exit` | `is_error`, `error`, `success: false`, `interrupted`, an `exit_code` anywhere in the response (Codex: `metadata.exit_code`), or `Exit code N` / `Process exited with code N` in the text. A response with no error marker is `err: 0` |
| `tp`, `tf` | pass and fail counts parsed from `go test`, `pytest`, `npm`/`yarn`/`pnpm test`, `jest`, `vitest`, `mocha`, `cargo test` output, only when the command is such a runner |
| `region`, `reedit`, `revert` | edits are reduced to hashes of their lines; an edit that changes lines an earlier edit wrote marks that earlier call re-edited, one that restores what an earlier edit removed marks it reverted. Understands `old_string`/`new_string`, `edits`, whole-file writes and patch text (Codex `apply_patch`). Failed edits leave no region |
| `thrash` | the same non-edit command has now run three or more times |
| `denied` | "the user doesn't want to proceed", "rejected by the user", "blocked by hook", `is_interrupt` |
| `corr` (on the prompt event) | a local regex says the next prompt corrects the agent ("no", "stop", "undo", "I told you", "why did you", "that's not what I asked", ...). The server's own re-tell detector (`retellRE`) is a second vote. The previous turn's actions are marked corrected |

`GRIMOIRE_TRACE=0` removes all of these from what the hooks send.
`grimoire agent install` registers the outcome hook on the prompt and failure
events as well; re-run it once to pick them up.

An action is **bad** when it failed, had failing tests, was later re-edited or
reverted, thrashed, was denied, or its turn was corrected. It is **observed**
when at least one signal exists for it; an action the hook could not read says
nothing and is left out.

## Benefit (observational): "associated"

For a memory, a kind, or all memories: the bad-outcome rate of influenced
actions against actions that were not, **matched on tool and session stage** and
restricted to sessions in which some memory had been shown. The difference is
stratified (each stratum weighted by its influenced actions) with a Wald
interval. It is labelled `associated`: an agent may pick up a memory on easier
work, and outcomes such as re-edits arrive late, so read it as a pointer, not a
verdict. Strata with no comparison actions are dropped and the card says how
many influenced actions were left unmatched.

## Holdout (causal): "caused"

`memory_holdout_rate` (`GRIMOIRE_MEMORY_HOLDOUT_RATE`, default `0` = off, at
most `0.5`; `0.05` is a sensible start). When above 0, each eligible item the
server was about to inject is withheld with that probability. Withheld items

* are not in the context, the tags or the fingerprint wire (the agent never sees
  them),
* are still listed in `keys`, so the hook's 30-minute dedupe keeps them withheld
  instead of re-drawing on the next prompt,
* are logged in `trace_withheld` with the probability, so the would-have-injected
  decision exists for outcome comparison. Shown items that faced the draw carry
  the same probability.

**Never withheld:** a memory a person pinned (`immutable`), any memory with an
`enforce: ask` check or an active compiled check ([MEMORY_RULES.md](MEMORY_RULES.md)),
and items forced into a response by an enforce decision. Rules without a check
are eligible. The adherence counters, usefulness feedback and the ignored
down-rank only ever see items that were shown.

Each decision becomes a `utilization.Record` (`go/internal/utilization`): target,
kind, session, stage, tool, whether it was shown, the probability, and an
outcome `Y`, the share of bad outcomes among the next five observed actions in
the session (a decision followed by none is dropped). `Store.Records` hands them
to a `utilization.Estimator`. The estimators (IPS, doubly robust, pooling by
kind, utility EMA) are built separately in `internal/utilization/estimate`;
until one is set on `Server.Estimator` the card reports **estimator not
installed** with the arm sizes, never a number.

| Causal status | Meaning |
|---|---|
| `holdout off` | rate is 0 and nothing was ever withheld |
| `insufficient data` | fewer than `memory_trace_min_arm` (10) shown or withheld decisions |
| `estimator not installed` | enough data, no estimator plugged in |
| `estimated` | label `caused`, with the estimate and interval |

Limits: the outcome window can include actions that other memories influenced,
and an item that is withheld may be replaced by the next candidate in a later
request. Pool by kind for power; one memory rarely has enough decisions.

## API and CLI

```
GET /api/memory/trace?target=fact:ID|note:PATH&days=30   one memory's card
GET /api/memory/trace/summary?days=30                     by kind and overall (admin)
grimoire memory trace [--target T] [--days N] [--json]
```

The card has `exposures`, `withheld`, `uptake` and `influence` (each with k, n,
rate and a Wilson interval), `link_evidence`, `adherence_outcomes`,
`linked_actions` (failed, tests failed, re-edited, reverted, thrash, denied,
corrected), `reminders` (unchanged, changed, abandoned, open, change rate with
interval, enforce-ask results), `benefit` (label `associated`) and `causal`.
Uptake counts only injections where it could be seen (fingerprintable or cited);
influence counts injections that had at least one action after them. A memory
the caller cannot read answers 404.

## Settings

| Setting | Default | |
|---|---|---|
| `memory_holdout_rate` / `GRIMOIRE_MEMORY_HOLDOUT_RATE` | `0` | holdout probability, 0 to 0.5 |
| `memory_trace_min_arm` / `GRIMOIRE_MEMORY_TRACE_MIN_ARM` | `10` | decisions per arm before the estimator is called |
| `GRIMOIRE_TRACE` (hooks) | `1` | `0` sends none of the trace fields |
