# Judge validity

How often does the repo's LoCoMo judge prompt accept an answer that is wrong on
the decisive detail? Compared with a deterministic key-fact checker on the same
answers.

- 30 hand-written (question, gold, wrong-but-adjacent) triples from LoCoMo
  questions (`triples.py`). Each wrong answer shares the topic and entities and
  differs on one decisive detail.
- 10 of the triples carry a correct paraphrase, to measure false rejects.
- Judge: LoCoMo `JUDGE_PROMPT` and `LEAN_SYSTEM_PROMPT`, unchanged, run on
  Haiku. LoCoMo itself graded with `claude-sonnet-5`; this is a deviation and
  it changes the judge.
- Checker: regexes in `triples.py` `require`, all must match.

Outputs: leniency on wrong answers with a Wilson 95% CI, judge false-reject rate
on paraphrases, checker results, unparseable judge outputs, and every
judge/checker disagreement.

Limits: the wrong answers are authored, so the leniency rate describes these 30
triples, not LoCoMo as a whole. Only LoCoMo questions are used.

    python3 run_judge_validity.py dry-run
    python3 run_judge_validity.py live --max-calls 40 --out DIR   # 40 Haiku calls
    python3 -m unittest test_judge_validity -v
