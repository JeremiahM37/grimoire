# Pre-registration: does an injected memory change what the agent does next?

Written and committed on 2026-10-11, before the holdout was switched on. The
commit that adds this file is the timestamp. Changes after that go in the
"Amendments" section at the end with a date and a reason; the text above it is
not edited.

## Question

Grimoire injects memories into agent sessions automatically
([MEMORY_USE.md](../../docs/MEMORY_USE.md)). Every measurement of that so far is
either observational or run on tasks we wrote ourselves. This experiment asks,
on one person's real daily agent work:

> When an eligible memory is about to be injected and is withheld at random
> instead, are the agent's next actions worse, better or the same?

## Setting

One Grimoire instance, one owner, live work in Claude Code and Codex sessions
(including their subagents). No task is created for the experiment and no model
is called by it. The work happens anyway.

## Randomisation

The existing holdout ([MEMORY_TRACE.md](../../docs/MEMORY_TRACE.md)), with
`GRIMOIRE_MEMORY_HOLDOUT_RATE=0.10`. Each eligible item the server is about to
inject is withheld independently with probability 0.10. The probability is
logged on both arms (`injections.p_withhold`, `trace_withheld.p_withhold`).

Never withheld, and so not part of the experiment: memories a person pinned,
memories with an `enforce: ask` check or an active compiled check, and items an
enforce decision forced into a response.

The agent is not told which items were withheld. No experiment flag
(`GRIMOIRE_EXP`) is on during the run, and the holdout rate is not changed.

## Unit, outcome and estimand

* **Unit:** one decision, an eligible item at one injection request.
* **Outcome Y:** the share of bad outcomes among the next five observed actions
  in the same session, as `adherence.Store.Records` defines it. An action is bad
  when it failed, had failing tests, was later re-edited or reverted, repeated
  three or more times, was denied, or its turn was corrected by the person. A
  decision followed by no observed action has no outcome and is dropped.
* **Estimand:** mean Y of withheld decisions minus mean Y of shown decisions,
  over decisions that faced the draw. Positive means showing the memory lowered
  the bad-outcome rate.

## Primary test

A randomisation test of the sharp null (no decision's outcome depends on its
arm): each decision's arm is redrawn with its logged probability, 5,000 times,
two-sided, alpha 0.05. This is valid although decisions made close together
share outcome actions, because only the assignment is random. The reported
interval is a percentile bootstrap that resamples whole session-days.

The analysis is [`analyze.py`](analyze.py) at this commit, seed 20261011, run
once on the union of the daily snapshots and the live database.

## Sample, duration and stopping

* Start: when the rate is set, on 2026-10-11. End: 28 days later.
* One analysis, after the end. Until then only `analyze.py --counts-only` is
  run, which prints arm sizes and the checks below and never an outcome by arm.
* If fewer than 1,000 withheld decisions have an outcome at day 28, the run is
  extended once to day 56. That choice uses counts only.
* The run stops early only if the owner asks. An early stop is reported as such
  and analysed the same way.

Power, measured on the pre-period trace with `analyze.py --power` (1.6 days,
3,042 decisions, 10 sessions, bad-outcome rate 6.0%): at a 10% rate and that
volume, 4 weeks detects an absolute change of about 0.5 points (9% of baseline)
with 80% power. Those two days were unusually busy and one session supplied 37%
of the decisions, so real power is probably lower.

**Smallest effect of interest:** 0.6 points absolute. If the whole interval lies
inside plus or minus 0.6 points, the result is reported as "no effect larger
than a tenth of the baseline bad-outcome rate".

## Validity checks (reported whatever they show)

1. The withheld share is within three standard errors of 0.10.
2. Relevance, kind and stage are balanced between arms.
3. The share of decisions with no following action is the same in both arms.
   If it differs (|z| > 3), withholding changed whether the agent acted at all;
   the primary result is then reported as conditional on acting and that
   difference is reported beside it.

## Secondary and exploratory

* **Secondary:** the same test within each kind (fact, rule, procedure,
  reference) that has at least 30 decisions per arm, Holm-corrected.
* **Exploratory, no tests:** by stage (prompt, action), and each outcome
  component alone (failure, failing tests, re-edit or revert, repetition,
  denial, correction).

Nothing else is reported as a finding. Anything found by looking further is
labelled post hoc.

## What this cannot show

* It measures the automatic injection of one item, given everything else the
  agent already has. The agent keeps its own memory files, can call `recall`,
  and a withheld item can be replaced by the next candidate. All of that pulls
  the estimate toward zero. A null does not mean the knowledge is useless.
* The outcome is a proxy read from tool results. It is not task success.
* One person, one vault, a small number of long sessions. The result is a case
  study and a reusable method.
* Pinned and enforced memories, which are probably the most useful ones, are
  excluded by design.

## Considered and left out

* **Task-level outcomes from Lectern** (accepted, changes requested, check
  passed). The join is possible, but only 24 task attempts carry a start date
  across September and October 2026. That cannot detect anything, so it is not
  an outcome here.
* **Scheduled `check=` commands as staleness ground truth.** Seven facts in the
  live vault carry a check. Too few to measure a staleness rate.

## Data

The trace holds session hashes, memory ids and outcome codes, never text. Daily
snapshots are kept outside the 30-day retention for the length of the run.
Whether the decision table is published is the owner's decision after the run.

## Amendments

* 2026-10-11, minutes after the start, before any item had been withheld: the
  `--counts-only` output computed the expected withheld share from decisions
  with an outcome only, so it read 0 when every decision so far lacked one. It
  now averages over all decisions. The test and the outcome are unchanged.
