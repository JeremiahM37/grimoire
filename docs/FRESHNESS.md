# Memory freshness

An agent remembers that Grimoire is on build 1.4.0. Grimoire gets upgraded,
nothing tells the memory store, and the next agent states the old version.
Re-checking every fact on every use fixes this. That is the C `volatile`
approach: always read the real thing. It is also expensive, because almost
nothing an agent remembers ever changes.

Grimoire aims the re-checks instead. Each fact in agent memory carries a
**tier**, an optional **check**, and a **change history**. `recall` uses them to
tell the agent, per fact, whether to rely on the fact or verify it first.

## What agents see

Every current fact that `recall` and `get_briefing` return has a
`freshness` object:

```json
{"tier": "volatile", "action": "verify", "check": "grimoire version",
 "reason": "volatile: re-check on every use", "p_stale": 0.0,
 "verified_at": "2026-10-08 14:02", "age_days": 0, "changes": 2, "verifies": 5}
```

- `action: use` means rely on the fact.
- `action: verify` means check it before acting on it. The `check` field says
  how. Then report the result through `remember` with `target_id`,
  `target_path` and `expected_text`:
  - If the fact still holds, send **the same text**. This records a
    confirmation (`op: VERIFIED`). The fact keeps its id and text, its
    confirmation count goes up, and its clock resets.
  - If it changed, send **the new value**. This supersedes the old fact
    (`op: UPDATE`), and the replacement inherits the old fact's tier, check
    and history.

Facts in the per-prompt context injection (`/api/memory/context`, used by the
coding-agent hook) carry a `verify` field for the same purpose.

Agents verify only the facts they are about to use. Facts that a recall
returns but the agent doesn't act on cost nothing.

## Which tier for which fact

The `remember` tool tells agents this table. People can also set the tier
by hand in a bullet's trailer (`fresh=7d check=...`).

| Tier | Use it for | Examples |
|---|---|---|
| `volatile` | Live state that changes without notice | What is running or deployed, current version, IP or port, a balance, who is on call, disk usage |
| `7d`, `12h`, … | Values that drift on a schedule you can guess | A dependency pin, a config value, a cron schedule, a model in use |
| `stable` | Things that do not change | Decisions and why they were made, history, root causes, preferences, conventions, measured results |
| *(none)* | Unsure | The store learns the rate from the fact's own history |

Give a `check` with `volatile` and interval tiers. It turns a re-check into a
single lookup instead of a search for where the truth lives:
`grimoire version`, `lan.env ROUTER_IP`, `systemctl is-active lectern`, a
file path, or a URL.

## How an untiered fact is judged

- **Rate.** Each fact has a change rate in changes per day, estimated as a
  Gamma-Poisson posterior.
  - The changes are the times the fact was superseded. A replacement written
    within a day of the version it replaces is treated as a refinement, not a
    change.
  - The exposure is the observed span: from the fact's first version to the
    last time anyone confirmed or replaced it. Time since then is not
    evidence of anything, because nobody looked.
- **Prior.** The prior comes from the fact's shape class.
  - Addresses, ports, versions, usage figures and words like "currently"
    start at about one change every two months.
  - Everything else starts at about one every two years.
  - These class rates are relearned from the store's own history, cached for
    ten minutes.
- **Decision.** `p_stale = 1 − exp(−rate × days since last verified)`. Above
  `GRIMOIRE_MEMORY_VERIFY_THRESHOLD` (default 0.3), the action is `verify`.

On a copy of a real 343-fact store, the defaults marked about 1% of recalled
facts for verification, all of them live-state facts. They marked none of the
store's decisions, preferences or findings.

## Decision model

The shape rule can only see values that look volatile: addresses, ports and
versions. Most of an agent's memory is prose about the state of work ("the
verify suite is PASS 7/7", "repo is at /path", "W637 remains unclaimed"), and
the rule misses nearly all of it. Optionally, a typed-decision model can
answer the question directly. Grimoire sends it each untiered, trusted fact
with one yes/no question ("does this describe current state that can change?").
In the same call it also asks how fast the fact changes (hours, days, weeks,
months or years). Grimoire stores the calibrated probability (`vol=0.73`) and
the fact's own starting change rate (`pr=0.31`, changes per day). That rate
is the speed answer averaged in log-rate, so a small chance of "hours" can't
dominate. It is used only when the model is at least 50% sure the fact
changes at all. Asked about a settled fact, a model still spreads some
probability over "hours" and "days", and without the gate that noise sets
fast rates on facts that never change.

That design was picked on the same 90 facts with hand-labelled speeds.
"Caught" means a fact flagged once it had probably changed; "wrong" means a
flag on a stable fact or one not yet due:

| Policy | Day-scale facts caught by day 3 / 7 | Caught by day 14 | Total caught | Total wrong |
|---|---|---|---|---|
| One rate for changing facts, every 60 days | 0 / 0 | 0 of 17 | 28 | 19 |
| One rate, every 30 days | 0 / 0 | 7 of 17 | 43 | 43 |
| Speed, arithmetic mean, ungated | 5 / 5 | 13 of 17 | 63 | 210 |
| **Speed, log-rate mean, gated at 50%** | **2 / 4** | **9 of 17** | **43** | **36** |

Set `GRIMOIRE_DECISION_URL` to any server that speaks TypeSafe's Jev wire
format. Off by default. Measured on 90 hand-labelled facts from a real agent
memory store (25 describe changing state):

| Predictor | AUC | Changing-state facts flagged at 30 days | Stable facts flagged at 30 days | Latency | Cost |
|---|---|---|---|---|---|
| Shape rule (default) | 0.53 | 2 / 25 | 1 / 65 | — | free |
| Laya `english`, local CPU, zero-shot | 0.50 | — | — | ~260 ms | free |
| Laya `typed-decisions`, local CPU, zero-shot | 0.55 | — | — | ~265 ms | free |
| qwen3.5:4b (Ollama, thinking off, yes/no) | 0.61 | 25 / 25 | 51 / 65 | ~230 ms | free (GPU) |
| qwen3.6:35b-a3b (Ollama, thinking off, yes/no) | 0.76 | 25 / 25 | 31 / 65 | ~540 ms | free (GPU) |
| Laya fine-tuned on 418 of the store's other facts, calibrated | 0.87 | 8 / 25 | 3 / 65 | ~275 ms | free (one-off ~25 min CPU training) |
| **Jev 1.13.0**, calibrated | **0.90** | **13 / 25** | **2 / 65** | ~235 ms | ~$0.000015 per fact |

AUC 0.5 is chance. The general LLMs were included as a quality reference.
They call almost every fact changing, so they would send half or more of a
store's settled facts for re-checking. A small decision model that answers
with a calibrated probability is better at this one question, as well as
faster and cheaper. Laya's own model card says its base checkpoints are not
zero-shot decision engines and need fine-tuning on your own decisions, and
that is what we saw. Fine-tuned, it gets close to Jev and costs nothing to run.

To get there, label your own facts with any good judge (Jev here), then run
`laya-train --loss soft-ce` on them and serve the result with
`LAYA_EXTRA_MODELS='{"volatility": "<dir>"}' laya-serve`. Set
`GRIMOIRE_DECISION_MODEL=volatility` and fit its own calibration. For the
checkpoint above that was `GRIMOIRE_DECISION_CALIBRATION=1.851,0.764`. A
fine-tuned checkpoint has learned from the facts it was trained on, so it
belongs to that store and should not be published. Jev's raw probabilities run high (stable facts centre
near 0.45), so they pass through a Platt curve
(`GRIMOIRE_DECISION_CALIBRATION`). It was fitted on the same 90 facts and held
up leave-one-out: log loss 0.60 raw, 0.36 calibrated.

A failed, slow (3 s cap) or unconfigured decision server leaves the fact on
the shape rule. Facts from untrusted origins are never sent, and neither are
facts with a declared tier. Grimoire sends the decision server nothing else
from the store.

## What the dream does with it

The scheduled dream (`grimoire dream`, the `dream` MCP tool) uses the same history. Under
`GRIMOIRE_DREAM_APPLY=safe` it applies the mechanical parts, taking a history
snapshot first:

- **history.** Backfills the change count on facts that were superseded
  before freshness existed, by following the struck-through `sup=` links that
  are still in the note.
- **retier.** Moves a fact to the tier its record supports:
  - A `volatile` fact confirmed at least 5 times and never found changed
    becomes `30d`.
  - A fact found changed 3 or more times, at least every two weeks, becomes
    `volatile`.
  - A `stable` fact that changed twice goes back to `auto`.

  Facts a person wrote are reported, never rewritten.
- **no_check.** Reports a `volatile` or interval fact that has no check.
- **check_command** (security). A check is a command agents run without
  asking, so stored checks are swept the way memory is:
  - Dangerous commands (pipe-to-shell, `rm -rf ~`, …) are High.
  - Anything that changes state (restart, delete, push, `-X POST`, a write
    redirect) is Medium.

## Safety

- A check must be one line of at most 300 characters. The write is refused
  if the check matches a dangerous-command pattern.
- A check sent with an **untrusted origin** (a web page, a connector document)
  is refused. Recall also never offers the check of an untrusted fact. Text
  other people can write must not be able to leave a command for an agent to
  run.
- A re-check that finds a person's fact changed follows the authority
  lattice like any other correction: an agent's write challenges a human
  fact, it doesn't overwrite it.

## Cost

- **Recall.** Judging a fact's freshness costs about 6 µs, so about 0.1 ms on
  a 20-fact recall. Ranking benchmarks were unchanged within noise. Learning
  the class rates is one aggregate query, at most every ten minutes.
- **Agent context.** The guidance in the `remember` and `recall` tool
  descriptions adds about 220 tokens of schema per session.
- **Lookups.** Spent only on `verify` facts the agent is about to use. A
  confirmation pushes the next re-check out: a volatile-shaped fact confirmed
  unchanged after three weeks is not due again for about five weeks.

## Where the design came from

The model was evaluated before it was built.

- **Data.**
  - 16,361 mechanically checkable facts from the git history of four
    repositories.
  - 24,835 real Claude Code and Codex transcripts over seven weeks, replayed
    in time order.
- **Result.** On the held-out half:
  - Aiming re-checks by learned change rate served **0.24% stale answers at
    up to 0.5 lookups per query**, against 0.73% for a fixed re-check timer.
  - Paired bootstrap: 0.52 percentage points fewer errors, 95% CI
    [0.33, 0.77].
  - Declared tiers were the best policy when lookups were scarcest:
    **0.62% vs 1.52%** at up to 0.1 lookups per query.
- **Caveats.**
  - It is one person's data, and the facts were code-shaped. Prose memories
    were not measured.
  - Tuning to an error target did not transfer between periods. Tuning to a
    lookup budget did, which is why the knob here is a probability
    threshold, not an error target.

The evaluation also mined passive evidence (agents' own file reads and
command output) to confirm or refute memories at no lookup cost. It was a
real but modest gain. It is not part of this implementation, because matching
prose memories against tool output needs a model, and that matcher's
false-match rate is unmeasured.
