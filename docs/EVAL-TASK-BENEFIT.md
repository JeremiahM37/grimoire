# Task benefit and judge validity (evalkit)

Status: measured, small. Three harnesses under `benchmarks/`. Live model calls
were Haiku only, counted in `benchmarks/evalkit-ledger.jsonl`: **141 of 150**
(1 smoke call, 60 task-benefit, 40 judge-validity, 40 BEAM).

## A. Task benefit (`benchmarks/task_benefit/`)

Fictional project "Quarry", 10 arbitrary conventions, 3 repeats per arm, real
Grimoire server (embed=off, scratch vault), memory retrieved via
`/api/memory/context` (the hook's endpoint). Deterministic regex checker.

| arm | pass |
|---|---|
| off | 0/30 |
| on  | 15/30 (95% Wilson CI 33-67%) |

- Retrieval recalled the fact for 5 of 10 tasks (T01, T03, T08, T09, T10). The
  on arm passed exactly those 5 tasks, 3/3 each, and 0/3 on the other 5.
- Exact two-sided sign test over tasks: 5 on-better, 0 off-better, p = 0.0625.
- Limit: 10 fictional tasks cannot establish a general effect. Lexical retrieval
  without stemming missed paraphrased questions (for example "linter" vs "lint",
  "migrations" vs "migration"); the on-arm result is bounded by that recall.
- Reproduce offline: `python3 benchmarks/task_benefit/run_task_benefit.py dry-run --binary BIN`.
- Raw: `benchmarks/task_benefit/results/`. The first live run stopped after 30
  rows when the grimoire server stopped answering (no log reason); it was resumed
  with `--resume`, which skipped completed rows and re-spent nothing.

## B. Judge validity (`benchmarks/judge_validity/`)

30 hand-written LoCoMo triples (wrong answer shares topic and entities, differs
on one decisive detail) plus 10 correct paraphrases. LoCoMo's judge prompt,
unchanged, on Haiku (LoCoMo graded with claude-sonnet-5, so this is a judge
change too).

- Judge leniency on wrong answers: 0/30 (95% Wilson CI 0-11.4%). The key-fact
  checker also rejected all 30.
- Judge false rejects on correct paraphrases: 1/10. The checker accepted all 10.
- Disagreement list: `c2q103` paraphrase "It was Max, I think." judged false,
  checker true. Otherwise judge and checker agreed on all 40.
- Gold answers: checker accepts 30/30 (sanity).
- Limit: these triples are authored, and one judge at one temperature. A zero
  leniency here says nothing about the judge on LoCoMo's own wrong answers.

## C. BEAM slice (`benchmarks/beam/`)

One BEAM 100K conversation, its 20 probing questions only, `grimoire` arm,
embed=off, Haiku reader and Haiku judge (the harness defaults were claude-haiku-4-5
reader and claude-sonnet-5 judge; both overridden). Not a BEAM result.

- Rubric satisfaction 66.8%, strict (every rubric item) 45.0% (9/20; Wilson CI
  26-66%). n = 20 from one conversation: a small slice, not comparable to the
  paper.
- Median approx. context tokens 6436. Harness change: `_claude` now reserves a
  shared-ledger slot; `--max-calls` added.
- Raw: `benchmarks/beam/results-haiku-slice/`.

## Not done

- No ranking against any other system. No general claim about memory benefit.
- Harness dry-runs are offline; tests for each use stdlib `unittest`
  (`python3 -m unittest benchmarks/task_benefit/test_task_benefit.py`, etc.).
- LongMemEval triples were not built; only LoCoMo questions were used.
