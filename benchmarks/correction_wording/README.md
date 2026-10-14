# Correction wording: how a corrected fact is shown to a model

Does the way a corrected fact is written in context change whether a model
answers with the new value or the old one?

## What it measures, and why

When a fact changes, a memory store can show the model the new value only, a
dated "now Y (changed DATE)", a history "was X, now Y", or a negated old value
"X is no longer true; Y". Two lines of research bear on which of these is
safer:

- Proactive interference: earlier values in context intrude on recall of newer
  ones (arXiv 2506.08184, 2603.00270).
- Restating the old value may raise its use. The "X is no longer true" and
  "was X" arms are the ones that restate it, so they are where this shows.

This harness asks the question with invented facts, so the model cannot answer
from training data. The only source of the answer is the context.

## Design

- `make_items(n, seed)`: n slot facts, each with an invented old value X and a
  later invented value Y. Values are three invented syllables plus a number
  (for example `Velmip-42`). X and Y never overlap. Dates are invented 2026
  dates.
- Four arms. They differ only in the wording of the corrected slot line.
  Distractor lines and line order are identical across arms.
- Question: "What is the current value for the <slot>? Answer with the value
  only."
- Scoring per reply, deterministic and case-insensitive:
  - `contains Y`: the new value appears (the wanted answer)
  - `contains X`: the old value appears
  - `strict correct`: contains Y and not X
- Temperature 0, one sample per item. Wilson 95% intervals on the rates.

## Backend

Any OpenAI-compatible server, configured only through the environment. There is
no default model.

```
export OPENAI_COMPAT_BASE_URL=http://127.0.0.1:11434/v1   # or any llama-server / vLLM
export OPENAI_COMPAT_MODEL=<model name the server exposes>
export OPENAI_COMPAT_API_KEY=...                           # only if the server needs one
```

Hosts whose name contains `anthropic` or `claude` are refused. `--max-calls`
(default 120) refuses a run that would make more calls than the cap, before it
makes any. The default run is 30 items times 4 arms = 120 calls.

## How to run

```
python3 benchmarks/correction_wording/correction_wording.py --selftest
python3 benchmarks/correction_wording/correction_wording.py run --n 30
python3 benchmarks/correction_wording/correction_wording.py run --n 30 --seed 3
```

The selftest uses a fake model that always returns the first invented value on
the target's line. That is the interference failure in miniature, so the arms
must differ. It makes no network call. Results are written to `results/` as JSON
with every reply.

Run against a local model only, and under the shared GPU lease if the GPU is in
use: `/mnt/bulk/inference-research/floor/research-lease.sh <command>`. Do not
point this at a hosted Claude loop.

## How to read the result

- The table gives, per arm, how often the reply contains Y and how often it
  contains X, with intervals, plus strict correct and neither.
- The interesting comparison is strict correct and contains X across arms. If
  "was X, now Y" and "X no longer true" have a higher contains-X rate than "Y"
  and "now Y (changed DATE)", restating the old value is raising its use.
- Both arms should be read against the Y arm, which is the baseline with no old
  value in context at all.
- A tie is a result too. It says the wording does not matter for this model at
  this size.
- Intervals on n = 30 are wide. Use `--n` in the 100s for a comparison that can
  separate arms a few points apart, and check the seed sensitivity by repeating
  with a second `--seed`.

## Limits

- One sample per item at temperature 0. Sampling variance is not measured.
- Invented facts with an artificial pattern. Real memory text is messier, and
  the arms may behave differently on natural phrasing.
- The fake-model selftest checks only the plumbing and the scoring. It says
  nothing about any real model.
