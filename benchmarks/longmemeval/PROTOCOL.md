# LongMemEval protocol — pre-registered

Frozen before the first scored run. Companion to the LoCoMo study
(`../locomo/`) — second dataset, same reader, same judge, same integrity
rules; anything not specified here follows the LoCoMo protocol.

**Dataset**: [LongMemEval](https://github.com/xiaowu0162/LongMemEval)
(ICLR 2025), `longmemeval_s`: 500 questions, each with its own haystack of
~50 chat sessions (~115k tokens) mixing evidence sessions into distractors.
Fetched from Hugging Face at run time; never committed.

**Questions**: the 30 abstention questions (`*_abs`) are excluded, matching
the LoCoMo category-5 exclusion. From the remaining 470 we draw a stratified
sample of **n = 200** proportional to `question_type`, `random.seed(42)`,
committed to `results/questions.jsonl`. The question's stated ask-date is
part of the prompt in **every** condition (several types need "as of when
was this asked").

**Conditions** (same information enters each):

| condition | context given to the reader |
|---|---|
| `none` | nothing |
| `grimoire-local` | Grimoire retrieval with the optional local model2vec embedder (`pip install model2vec`, zero config beyond that) |
| `grimoire-ollama` | Grimoire retrieval with `GRIMOIRE_OLLAMA_URL` + `nomic-embed-text` |
| `full` | the entire haystack transcript |

Each question's haystack is ingested into a fresh vault, one note per
session, titled with the session date (the LoCoMo round-3 convention),
turns speaker-labelled `User:` / `Assistant:`. Retrieval = the product's
`index.retrieve` top-10 + `/api/search` top-5 with `full=true`, raw
question text, no rewriting.

**Reader**: `claude-haiku-4-5`, one shared prompt template (empty context
block for `none`). **Judge**: `claude-sonnet-5`, strict v2 prompt, blind to
condition. NOTE: this is *not* the official LongMemEval judge — absolute
numbers are not comparable to the paper's leaderboard; comparisons between
the conditions in this table are.

**Metrics**: judge accuracy overall and per `question_type`; reader input
tokens per condition (context cost = median minus the `none` baseline).

**Integrity**: same rules as LoCoMo — no benchmark-aware product code, all
rounds reported, raw per-question outputs committed.
