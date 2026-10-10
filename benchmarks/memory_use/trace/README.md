# Utilization-trace validation harness

Ground truth for "did this memory influence this action, and did that help", on a
simulated coding environment, so the tracing pipeline (tags, fingerprints, later
action linking and outcome capture) can be scored against known answers. The design is summarised in `docs/MEMORY_TRACE.md`.

* `env.py`: in-memory repo, a small bash interpreter (ls/cat/grep/find/sed -i/echo, a fake git).
* `families.py`, `tasks.py`: 40 tasks. 30 treatment tasks in 6 families (test command, generated
  files, dry-run-before-deploy, git conventions, package manager, config location), each with one
  relevant memory the repo's own files do not state (the README is deliberately stale), plus 2
  distractor memories; 10 controls where the memory exists but the task does not touch it.
  `python3 tasks.py` checks that every oracle solution passes.
* `agent.py`: qwen3.6:35b-a3b on Ollama with native tool calls, `think` left on (turning it off
  silently disables tool calls for this model), `num_ctx` 16384. No Claude calls.
* `run.py`: arms `with` (relevant + distractors) and `without` (distractors only), paired seeds.
  Writes `runs/*.json` and `events.jsonl` in the shape the hooks see: `UserPromptSubmit`,
  `PreToolUse`, `PostToolUse` (with `tool_response`), `Stop` (with the turn's assistant text).
* `replay.py`: shadow replay on a sample of with-runs. Each decision point is re-sampled 3 times
  with the relevant memory removed from the same conversation prefix; an action is influenced when
  it survives in at most one sample.
* `signals.py`, `report.py`: tag and fingerprint signals computed with the production selector
  (`go/cmd/fpdump`, build it to `$FPDUMP`) and hook matcher; report writes `REPORT.md` and `report.json`.
* `run_under_lease.sh` (homelab-specific: edit `LEASE`/`LOCK` or run without it): holds the shared AMD GPU research lease (the same mechanism as
  `homelab-api/eval/capability/servers/run-under-lease.sh`, which is hardwired to its own run.py).

```
PYTHONHASHSEED=0 ./run_under_lease.sh python3 run.py --out $E --reps 2 --workers 6
./run_under_lease.sh python3 replay.py --out $E
python3 report.py --out $E
```
Data and results go to the `--out` directory you pass.
