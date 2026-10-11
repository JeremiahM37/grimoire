# Task-benefit study

Does a stored project fact change the answer to a convention question?

Fictional project ("Quarry") with 10 arbitrary conventions that a reader cannot
infer from the question or from pretraining. Each is written as a memory entry
into a real Grimoire server (`goserver.py`, embed=off, scratch vault) with 5
Harbor distractor facts. Two arms, 3 repeats each:

- `off`: the question alone.
- `on`: the question with `GET /api/memory/context` output for it injected into
  the prompt, the same endpoint the agent hook calls.

Deterministic checker: every regex in `tasks.py` `require` must match the answer
(case-insensitive). No judge model is used.

Statistics: per-task pass counts per arm, retrieval hits (was the fact in the
retrieved context), and an exact two-sided sign test over tasks whose pass rates
differ. Pairing is by task, not by rep.

## Limits

- 10 fictional-project tasks cannot establish a general effect.
- Facts with no arbitrary value would not need memory at all; the design
  favours checkable answers over realistic ones.
- Retrieval is lexical with no stemming, so a paraphrased question can miss a
  stored fact. Retrieval recall is reported separately, and the on-arm
  result is bounded by it.

## Commands

    python3 run_task_benefit.py dry-run --binary BIN          # offline, oracle + stub
    python3 run_task_benefit.py live --binary BIN --n 3 --max-calls 60 --out DIR
    python3 run_task_benefit.py live ... --resume             # finish an interrupted run
    python3 run_task_benefit.py report --out DIR
    python3 -m unittest test_task_benefit -v                  # offline tests

Live calls are Haiku through `benchmarks/budget.py` and count against the shared
ledger (`benchmarks/evalkit-ledger.jsonl`, total cap 150, per-run cap 60).
