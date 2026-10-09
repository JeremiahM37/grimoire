# Memory-use benchmarks

Research harness behind `docs/MEMORY_USE.md`. It measures whether Grimoire's
memory path surfaces the right memory at the right moment, and whether an agent
acts on a memory once it is in context.

The data (memories, generated cases, transcripts) is private to the deployment
and lives under `/mnt/bulk/memory-use-research`, not in this repo. Scripts use
hardcoded paths under that directory on purpose.

## Pipeline

1. `build_vault.py`: build a scratch vault of memories.
2. Run a scratch Grimoire server on `127.0.0.1:9141` against that vault.
3. `harness.py <name> <url> [k=v]`: retrieval run. Scores prompt and action
   hits, and near-miss false fires. Writes `res_<name>.json`.
4. `paired.py a.json b.json`: exact McNemar test between two runs.
5. `gen_cues.py` / `load_cues.py`: generate cues with local Ollama (qwen) and
   load them back.
6. `learn_exp.py`: learned-cue experiment, reported as a difference in
   differences.
7. `real_cases.py`: verified real re-tells of past mistakes.
8. `replay.py`: replay the real prompt stream with learned cues.
9. `retell_judge.py`: decision-model judge for re-tells. Needs a Jev key in
   `~/.ai-secrets/jev.yaml`.
10. `adherence.py` and `adherence_long.py`: does the agent follow or violate a
   memory placed in its context. Calls `claude -p` from a clean working
   directory with no MCP servers and no settings, so the agent sees only what
   the arm provides. `adherence_long.py` imports helpers from `adherence.py`,
   so keep both files together.

## Notes

- Most scripts make live calls (Grimoire, Ollama, `claude -p`) and cost time
  and tokens. Read the usage line at the top of each script before running it.
- Compare runs only on matched cases and repeat before reading a small
  difference as real. Single runs swing by several points.
- Nothing here writes to the production vault. Point the harness at the scratch
  server only.
