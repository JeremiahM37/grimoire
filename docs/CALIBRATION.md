# Calibration: learning from what happened to agent memory

Status: **implemented, offline-tested, not validated on real traffic.** This
document says what is measured, what is designed, and what is open.

## What it does

`internal/calibrate` closes two loops that were previously hand-set.

1. **Learned reliability.** Each agent gets a conservative reliability: the
   95% lower bound of a Beta posterior over the share of its corrected facts
   that stood. Correction events are:
   - `confirmed`: a *different* writer re-checked the fact and found it true
     (`confirmFact`; a same-agent re-check does not count).
   - `challenged`: a later write contradicted the fact and was refused
     overwrite because its writer ranked lower (`reconcileFact`, challenge branch).
   - `superseded_by_human`: a person ruled against the fact in a settled
     challenge (`settleChallenge`, concede or uphold).
   - `deleted`: a person retracted the fact (`forgetOne`, `who=human`).
   Human-authored and hand-edited facts have no agent and are not scored.
   Counts persist in `calibrate_agents` in the index database and are bumped by
   those four hook points. An in-memory `Snapshot` computes the same numbers
   from facts, and a test asserts the two agree.

2. **Learned retention (ordering only).** `RetentionScore` is
   `2·log1p(used) + log1p(recalled)`, so a fact nobody recalled or used scores
   zero. Eviction proposals (`POST /api/memory/prune`, `grimoire memory prune`)
   now list the least-used, then oldest, facts first. Facts with three or more
   helpful uses, human-authored, immutable, under an open challenge,
   superseded, private or rated 4 or 5 are never proposed. Deletion remains a
   person's or a policy's decision; the prune path still retracts only through
   the existing file re-check.

Exposure: `GET /api/memory/agents/reliability` (admin-only, in the leak probe),
`grimoire memory agents`, and `agent_reliability` on explain recall for
agent-written facts.

## What it does not do

- **It is not a measure of truth.** It measures how often an agent's claims were
  corrected. An agent that is never challenged is not thereby correct; most
  facts are never checked at all, and silence is not agreement.
- **It does not change authority or enforcement.** The authority lattice is
  unchanged. Reliability is shown, not acted on.
- **It does not delete.** Proposals only.
- **Dream does not propose evictions.** `internal/dream` is a checker over
  documents and has no retention logic. The eviction proposals live in
  `internal/api/memory_prune.go`, and that is where the ordering was wired.
- **Counters are not rebuilt from markdown on reindex.** A challenge is not
  recorded as a count in the file, so the learned counts survive only in the
  index database, which a rebuild does not reset. Losing that database loses
  learned trust, not any fact.
- **An agent with no record scores the prior**, about 0.13 at 95%. That is
  deliberately low for a conservative bound. It is not a verdict on the agent.

## Measured

Commands (all deterministic, offline):

    cd go && go test ./internal/calibrate/ -v -run Simulation

Simulation: two agents with true rates of facts that stand, each producing
`n` correction events. 2000 trials, n up to 300, fixed seed.

| true p (high vs low) | agree at n=10 | agree at n=20 | agree at n=50 | first n with agreement >= 0.95 from there on |
|---|---|---|---|---|
| 0.90 vs 0.60 | 0.924 | 0.984 | 1.000 | 14 |
| 0.80 vs 0.50 | 0.893 | 0.964 | 1.000 | 17 |
| 0.90 vs 0.80 | 0.611 | 0.735 | 0.889 | 77 |

"Agree" is the fraction of trials in which the higher-p agent's bound is
strictly higher. A clear gap settles in one to two dozen correction events per
agent. A near-tie (0.90 vs 0.80) needs about 80. Real traffic produces far
fewer correction events per agent than that, so **in practice most agents will
sit in the uncertain region for a long time.** That is the honest result and it
is why the reliability is shown, not enforced.

One-event sensitivity (unit tests):

- 50 confirmations against 20, then one challenge against the leader: the
  ranking holds.
- **Negative result.** 30 confirmations against 25 is a near-tie (lower bounds
  about 0.02 apart). One challenge against the leader reverses it. Any learner
  will do this when one event is larger than the margin. Operators should
  treat a reliability difference under about 0.05 as "not yet separated".
- A single challenge moves the bound by under 0.1 from ten confirmations (a test asserts it).

## Open

- Real correction rates. Nothing here has run against a live vault, so the
  event rates per agent, and therefore how long the uncertain region lasts,
  are unknown.
- Whether the normal-approximation lower bound is adequate at very small
  counts. It is monotone, which is what the ranking needs, but it is not exact.
- Whether recall counting (one write per returned fact) costs noticeable time
  on large vaults. Not benchmarked.
- Whether `Deleted` should also count an agent's own retraction. It does not
  today, on purpose: an agent retracting its own fact is not a correction signal.
