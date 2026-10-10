# BEAM benchmark harness

Long-term-memory evaluation of Grimoire against the BEAM benchmark. Modelled on
[`../locomo/`](../locomo/): same reader and judge models, same pluggable
endpoint, same ingest-by-running-a-server approach (`../goserver.py`), and the
same retrieval context as LoCoMo's `retrieve_go.py`.

**Status: no run has been recorded.** This directory contains the harness and
an offline self-test only. It reports no accuracy, no token counts and no
comparison, and none should be quoted from it until a real run is written up
(in a `REPORT.md`, as LoCoMo does).

## Dataset and sources

BEAM ("Beyond a Million Tokens: Benchmarking and Enhancing Long-Term Memory in
LLMs", Tavakoli et al., ICLR 2026, [arXiv:2510.27246](https://arxiv.org/abs/2510.27246))
is a benchmark of 100 long, synthetically generated multi-domain conversations
with human-validated probing questions across ten memory abilities.

- Paper: <https://arxiv.org/abs/2510.27246> (PDF: <https://arxiv.org/pdf/2510.27246>)
- Code: <https://github.com/mohammadtavakoli78/BEAM> (repository `LICENSE` is MIT)
- Data, 100K / 500K / 1M tiers (20 / 35 / 35 chats):
  <https://huggingface.co/datasets/Mohammadta/BEAM>
- Data, 10M tier (10 chats): <https://huggingface.co/datasets/Mohammadta/BEAM-10M>

The schema below was read from the live Hugging Face datasets-server (`/rows`)
and the repository's `src/beam/download_dataset.py`, not from memory. The
dataset licence is stated on the Hugging Face dataset cards; check those before
redistributing anything. This repository never commits the data.

Per row (one conversation):

| field | type | used for |
|---|---|---|
| `conversation_id` | string | conversation id |
| `conversation_seed` | dict (`category`, `id`, `subtopics`, `theme`, `title`) | metadata |
| `chat` | list of batches; each batch is a list of messages `{role, content, id, index, question_type, time_anchor}` | the conversation Grimoire ingests |
| `probing_questions` | **Python-literal string** → dict keyed by ability; each value is a list of items | questions and rubrics |
| `user_questions`, `narratives`, `user_profile`, `conversation_plan` | various | not used |
| `plans` (10M tier only) | list | not used |

Each probing item has `question`, `rubric` (a list of strings, usually
`"LLM response should state: ..."`), and an ability-specific reference field:
`ideal_response` (abstention), `ideal_answer` (contradiction resolution),
`ideal_summary` (summarization), or `answer` (the rest). Instruction following
and preference following carry **no reference**, only the rubric. Abilities:
`abstention`, `contradiction_resolution`, `event_ordering`,
`information_extraction`, `instruction_following`, `knowledge_update`,
`multi_session_reasoning`, `preference_following`, `summarization`,
`temporal_reasoning`. Each conversation carries 2 items per ability, so 20 per
conversation and 2,000 in total. That count follows from the row shape and the
100-chat total, and was not checked against a published number.

## Usage

```bash
cd benchmarks/beam

# 1. offline self-test on the synthetic fixture (no network, no model, no Grimoire)
python3 run_beam.py dry-run                                  # writes to a temp dir
python3 -m unittest discover -s . -p 'test_*.py' -v          # the full offline suite

# 2. fetch the dataset (public, no token; Hugging Face datasets-server)
python3 run_beam.py download --splits 1M                     # -> data/ (gitignored)
python3 run_beam.py download                                 # all four splits

# 3. real run (needs the `claude` CLI, or an OpenAI-compatible server)
python3 run_beam.py run --data data --arms none,full,grimoire \
    --binary ../../go/grimoire --embed auto --limit 1       # smoke test, 1 conversation
python3 run_beam.py run --data data --arms none,full,grimoire --binary ../../go/grimoire

# 4. table from an existing run
python3 run_beam.py report --out results
```

Environment: `BEAM_DATA`, `BEAM_RESULTS`, `BEAM_PARALLEL` (default 12). The
`grimoire` arm needs a built server binary (`go build` in `go/`).
`--backend openai --base-url http://host:port` points the reader and judge at any
OpenAI-compatible `/v1/chat/completions` endpoint, e.g. a local llama-server.
`--model` and `--judge-model` override the reader and judge ids.

`dry-run` uses a stub model and the synthetic fixture
(`fixtures/synthetic_beam.json`, which is **not** BEAM data; it has the same
schema shape). Its numbers mean nothing. Its purpose is to show that the
pipeline runs end to end: the stub model must score 0%, and
`--backend stub-oracle` answers with the reference, so the scoring path must
score 100%. Both are checked by the unit tests.

## Protocol

**Questions.** All 2,000 probing questions are used. There is no sampling, so
there is no seed. Items with no rubric are skipped and counted in `summary.json`
(none were seen in the two 1M rows inspected).

**Conditions.** Each arm receives the same reader prompt and the same judge.

| arm | context given to the reader |
|---|---|
| `none` | nothing (floor: what the reader can guess without the conversation) |
| `full` | the entire conversation transcript (`## Batch N — <time_anchor>` headers, `role: content` lines), cut to the **last 400,000 characters** (about 100k tokens at chars/4) when longer. The 1M tier is roughly ten times that, so `full` is a truncated null baseline, not an upper bound. |
| `grimoire` | Grimoire's retrieval for the **raw question**: top-10 `/api/retrieve` chunks, then top-5 `/api/search?full=true` bodies, de-duplicated (LoCoMo's `context_for`, imported unchanged). |

For `grimoire`, each conversation is ingested into a **fresh vault**: one note
per chat batch, with the batch's time anchor in the note title, written to
`<out>/grimoire-vault/`. The server is launched by `goserver.launch` with
`GRIMOIRE_NO_WATCHER=1`, a vault check on `/api/health` (so a stray server on the
port fails loudly), and `/api/reindex` after each rewrite. No query rewriting and
no benchmark-specific preprocessing.

**Reader.** `claude-haiku-4-5` (LoCoMo's `READER_MODEL`), prompt:
answer completely using only the conversation; say plainly when the information
is absent rather than guessing; under 200 words. Abstention is therefore
rewarded for the right reason, unlike LoCoMo's "give your best guess" prompt.

**Judge.** `claude-sonnet-5` (LoCoMo's `JUDGE_MODEL`). One call per
(question, arm): the judge sees the question, the answer, and the numbered rubric
items, and returns one boolean per item. Each question scores the fraction of
items satisfied. "Strict" is the fraction of questions with every item satisfied.

**Reporting.** Per ability and overall: mean rubric satisfaction (%), strict (%),
and n. Also the median approximate context size (chars/4) per arm, so cost is
visible next to accuracy. `summary.json` holds the same values, plus
`status: "dry-run-not-a-result"` on dry runs.

Outputs under `--out` (default `results/`): `questions.jsonl` (the frozen set),
`<arm>/answers.jsonl`, `<arm>/judged.jsonl`, `grimoire-server.log`,
`summary.json`. Every phase is resumable: re-running skips finished qids and
appends only the rest. Failed calls are logged and left out, so a re-run retries
them.

## Deviations from the official BEAM evaluation

The official code is in `src/evaluation/` and `src/prompts.py` of the
repository. It is **not** reproduced here, so numbers from this harness are not
directly comparable to the paper's:

- **Judge.** The official abstention and contradiction-resolution scorers also
  judge per rubric item and average, which matches this design. The
  **event-ordering** and **summarization** scorers use other metrics (`compute_metrics.py`
  has Kendall-tau-style ordering scores, BLEU, ROUGE, and embedding-based fact
  alignment). This harness does not implement those, so its numbers for those two
  abilities are not the paper's metric.
- **Judge model and prompt** are this harness's own, and LoCoMo's models were
  chosen for comparability with LoCoMo, not with BEAM.
- **Context budget.** `full` is truncated (above). The paper's long-context
  runs and its RAG and LIGHT baselines are not reproduced here.
- **Memory ingestion.** One note per chat batch, the way LoCoMo writes one note per
  session. The official pipeline's ingestion for memory systems is not used.
  `user_questions` are not ingested.

If a comparison with the paper is ever claimed, use the official evaluation
scripts, or write the deviations above into the report.

## Files

- `run_beam.py`: loader, `download`, the arms, the judge, `report`, `dry-run`.
- `test_beam_dryrun.py`: offline tests (unittest; pytest collects them too). The
  Grimoire test writes a fake server that speaks the four endpoints the harness
  calls, so launch, vault check and context assembly all run without a Go build.
- `fixtures/synthetic_beam.json`: 2 synthetic conversations in the BEAM schema,
  for tests and dry runs only.
- `data/`, `results/`: created at run time. `data/` is gitignored.
