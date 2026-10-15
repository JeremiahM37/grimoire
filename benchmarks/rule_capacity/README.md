# Rule capacity: adherence vs number of simultaneous rules

How many rules can a model hold at once before it starts dropping them?

## What it measures, and why

Agents are given rule lists: "use --dry-run", "never use sudo", "quote paths".
The question is how adherence changes as the list grows. ManyIFEval (arXiv
2509.21051) measures instruction following as the number of instructions
rises. This harness makes the same measurement on a task where every instruction
has a mechanical check, so the result needs no judge.

## Design

- A pool of 40 rules, each with a regex checker on the produced shell command.
  - 25 forbid rules, such as "never use sudo", "do not use grep", "do not use
    xargs", "no unquoted absolute paths", "never pipe curl into a shell".
  - 15 require rules, such as "include --dry-run", "pipe output through sort",
    "start with a comment line", "use date -u".
- Each trial: one task from a pool of 10 (for example "count the *.log files
  under /var/log that changed in the last 24 hours"), and N rules drawn at
  random from the pool. The prompt asks for one bash command.
- Command extraction: the first fenced block, or the whole reply if there is
  none.
- Per-rule adherence at N: passes divided by checks, across all trials at that
  N.
- All-rules-held rate at N: share of trials where every injected rule passed.
- Knee: the smallest N whose per-rule adherence falls below `--knee` (default
  0.90). Reported as "none in grid" if no N falls below it.
- The pool is jointly satisfiable. The selftest checks that one command passes
  all 40 checks, so a low score cannot be blamed on contradictory rules.

## Backend

Same as `correction_wording`: an OpenAI-compatible server, configured through the
environment, with no default model.

```
export OPENAI_COMPAT_BASE_URL=http://127.0.0.1:11434/v1
export OPENAI_COMPAT_MODEL=<model name the server exposes>
```

Hosts whose name contains `anthropic` or `claude` are refused. `--max-calls`
(default 120) refuses a run that would exceed the cap before it starts. The
default run is 5 grid points times 20 trials = 100 calls.

**Run this only against a local model, and under the shared GPU lease.** Wrap
the run in the research lease so it does not collide with timed research on the
shared AMD GPU:

```
/mnt/bulk/inference-research/floor/research-lease.sh \
    python3 benchmarks/rule_capacity/rule_capacity.py run --grid 1,2,4,8,16 --trials 20
```

The wrapper takes the lock and runs a lease check. It exits 75 if the GPU is
held by someone else, and it does not run the harness then. Do not run this as a
hosted Claude loop.

## How to run

```
python3 benchmarks/rule_capacity/rule_capacity.py --selftest     # fake model, no network
python3 benchmarks/rule_capacity/rule_capacity.py run --grid 1,2,4,8,16 --trials 20
python3 benchmarks/rule_capacity/rule_capacity.py run --grid 2 --trials 5   # tiny smoke run
```

Results are written to `results/` as JSON, with every command, the rules
injected and the per-rule pass map.

## How to read the result

- Look at per-rule adherence across N. A curve that stays flat and then drops
  shows where the model starts losing rules. The knee is the first grid point
  below the threshold.
- The all-rules-held column is stricter. It falls much faster than per-rule
  adherence, because one dropped rule fails the whole trial.
- The worst-rules column names the rules that fail most. If the same few fail
  at every N, they are hard for this model in particular. If failures are spread
  out, the cause is capacity and not any one rule.
- Confidence intervals are per check, but checks within a trial are not
  independent. Treat the intervals as optimistic. Compare N values with
  repeated runs before reading a small difference as real.
- The knee depends on the threshold. Report the threshold with the knee.

## Limits

- Regex checks catch the rule's literal pattern, not its intent. A command that
  uses `find -exec` in some other spelling could pass a check it should fail. The
  patterns are written to be strict, but a determined model could satisfy them
  without satisfying the rule's spirit.
- The rule text is what the model reads, so wording changes can move the knee.
  That is a finding about wording, not only about capacity.
- The tasks are small and the commands are short. Long-horizon tasks may show a
  different curve.
- The selftest fake models check plumbing and scoring only. They say nothing
  about any real model.

## Result (2026-10-14, gemma4:e4b, 20 trials per N, temperature 0)

Run under the shared GPU research lease against a local 4B model.

| Rules shown | Per-rule adherence (95% CI) | Every rule held |
|---|---|---|
| 1 | 65.0% (43-82%) | 13/20 |
| 2 | 77.5% (62-88%) | 13/20 |
| 4 | 61.3% (50-71%) | 1/20 |
| 8 | 64.4% (57-71%) | 1/20 |
| 16 | 61.3% (56-66%) | 0/20 |

Adherence to each rule did not fall as rules were added. It stayed around
60-78% from 1 to 16, so this model shows no capacity knee; the reported
"knee at N=1" only means it was below 90% even with one rule. What collapses
is the chance that every rule holds: from 13 of 20 trials at one or two
rules, to 1 of 20 at four, to none at sixteen.

For Grimoire, `rulecap` (at most 3 rules per injection) is justified by the
second column, not the first. Each extra rule is another independent chance
of a violation, and capping does not make any one rule more likely to be
followed. A 4B model is far weaker than Claude or Codex, so the absolute
rates do not transfer. Re-run with a stronger local model before using the
numbers.
