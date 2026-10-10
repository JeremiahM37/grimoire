# Regression tests for memory

A memory store changes under an agent's feet: the dream rewrites and deletes
lines, consolidation merges notes, `memory index` regenerates `MEMORY.md`, a
supersession replaces a fact, a person rewords a rule. Each change is usually
right, and each can quietly stop a memory from firing in the situation it
existed for. Nothing tests for that.

Memory replay is that test. Every context injection is kept as a **situation**
(what the agent was asked, or about to run) together with the memories that
fired and whether they turned out useful. Before a change lands, the situations
are replayed against the store as it is and as it would be:

- **lost**: a memory that fired and was useful no longer fires (removed, below
  `min_rel`, or displaced);
- **gained**: a memory fires that did not; on a situation where nothing useful
  fired, or one a seed case says must stay quiet, it is a **new false fire**;
- **moved**: a useful memory still fires at a different rank.

It uses no model and writes nothing: situation vectors are cached, the proposed
store is an overlay on a snapshot, and only the memories a change touches are
rescored. 1,000 situations against 1,500 memories take about 0.1 s to score on
32 cores (about 1.5 s on one) and about 1 ms per proposed change.

## What is kept

`.grimoire/replay.db`, beside the index. Derived telemetry: never part of the
notes, never synced to a phone. It holds **what the injection log already
holds** (memory identity, stage, relevance, the session's outcome) **plus the
request text the hook sent**, which the injection log has never stored; that
text is the one new thing, and it is why the corpus is bounded and scrubbed:

| | |
|---|---|
| Situation | one row per distinct (request text, stage): text, stage, `min_rel`, item limit, byte budget, times seen, first/last seen |
| Text | whitespace collapsed, cut to 400 characters; opaque tokens (24 or more characters with a digit: keys, hashes, pasted tokens) replaced by `…` |
| Expected memories | per situation, up to 12 `(memory, weight)` pairs, folded from the adherence log: **cited 1.0, followed 0.7, violated 0.5** (it was right to fire), pending 0.2, **ignored and contradicted 0** (fired, not useful: baseline only). Weight 0.5 or more is *useful*: what a change must not lose |
| Retention | live situations not seen for 60 days are dropped (`replay_retention_days`) and the newest 5,000 are kept (`replay_max_situations`); unresolved firings are dropped after 14 days |
| Seeds | benchmark or hand-written cases, separate cap of 5,000, never aged |
| Recorded when | a hybrid `/api/memory/context` request carries a `session`, whether or not anything fired (a request that fired nothing is how a change that adds noise is caught). `replay_log=0` turns it off |

Outcomes arrive late (a turn ends after the injection), so a firing is folded
into its situation once it is ten minutes old, matched to the adherence row of
the same session, memory and stage.

### Seed suite

With no live log yet, load the memory-use benchmark's cases
(`benchmarks/memory_use`, the `cases.json` files described there):

```
grimoire memory replay seed /mnt/bulk/memory-use-research/bench/cases.json
```

A positive case becomes a situation that must keep firing its memory; a
near-miss becomes one where that memory must stay quiet. Action cases use the
hook's floor (0.7), limit (2) and budget (1,200 bytes). A hand-written file
`{"cases":[{"text","stage","min_rel","expect":[],"avoid":[]}]}` works too, and
so does `POST /api/memory/replay/seed`.

## What the replay reproduces

The ranking of `GET /api/memory/context`, over an in-memory snapshot of the
store: hybrid relevance from embedding similarity and term overlap, learned
and agent cues, the action-stage path triggers, `min_rel`, no duplicate text,
the item limit and the byte budget. Candidates are limited the way the
endpoint limits them (the 40 strongest facts; notes through the 30 best chunks
of the whole corpus, memory-note chunks included since they take slots there).
Tests pin the formulas to the endpoint's (`relevance`, `cueRelevance`, the
query terms).

What it cannot reproduce is the BM25 leg and the rank fusion of note
retrieval: replay approximates "in the endpoint's top 30 chunks" with its own
relevance. Measured on 908 benchmark situations against the real endpoint
(see *Measured* below), it agrees on the whole fired set in 550 of 908 (61%)
and on whether the gold memory fires in 444 of 588 (76%). It fires 358
memories the endpoint does not and misses 341 that it does.

One deliberate difference: **a cue learned from a situation never vouches for
that situation.** Otherwise a situation taught to a memory would pass whatever
happened to the memory's text. This is leave-one-out validation: the replay
asks whether the memory still fires through its text and the cues other
situations taught it.

## Using it

```
grimoire memory replay                                   # corpus size
grimoire memory replay --diff --note PATH --with FILE    # a note becoming FILE
grimoire memory replay --diff --change change.json       # {notes,edits,remove,merge,remap}
grimoire memory replay --diff --index                    # a regenerated MEMORY.md
grimoire memory replay seed cases.json
```

`--json` prints the raw report, `--brief` only the counts. The exit status is 2
when the change would be held. `POST /api/memory/replay` takes the same body
(administrators only: the report quotes past requests) and returns

```json
{"verdict": {"checked": true, "hold": true, "reason": "would lose 3 useful recall(s) in 3 situation(s)"},
 "report": {"situations": 889, "checked": 211, "lost_situations": 3, "lost_memories": 3,
            "gained": 1, "new_false_fires": 0, "moved": 0, "score": 0.98,
            "diffs": [{"text": "...", "stage": "prompt", "lost": [{"target": "note:...", "why": "below_min_rel",
                       "before": 0.64, "after": 0.41, "weight": 1}]}]}}
```

`score` is the share of useful recalls (by weight) that survive. A verdict is
`checked: false`, and never holds, until the corpus has `replay_min_useful`
(3) situations with a useful recall on file.

## Where it gates

| Change | Automated? | What happens on a loss |
|---|---|---|
| Dream fixes (`applyDreamFixes`) | yes | the document is left as it is; the dream report gets a **Held by memory replay** section with the situations that would have stopped firing, and `grimoire dream` prints them. Each document is judged against the store as the earlier fixes left it |
| Consolidation (`POST /api/memory/consolidate`) | yes (a model rewrites) | the note is not rewritten; the response lists it under `held`. `{"force": true}` applies it anyway |
| Index regeneration (`grimoire memory index --write`) | yes | not written, exit status 2, with the reason; `--force` writes it. `memory replay --diff --index` shows the report first |
| `PATCH /api/memory/entry` (a person's edit) | **never blocked** | applied; the response carries `replay: {lost_recalls, situations, examples}` |
| `remember` that supersedes a fact | **never blocked** | applied; that result carries `replay` when the replacement stops a useful memory from firing |

What holds an automated edit is `replay_max_lost` (default 0 useful recalls)
and, if you set it, `replay_max_false`. `replay_gate=0` turns the gate and the
warnings off. If replay itself fails, the edit goes through: a safety net must
not become the thing that stops memory working.

### A merge keeps its identity (remap)

When memory A is merged into B (a supersession, a dream merge, a consolidation
that re-mints a fact's id), a situation that expected A counts **B firing as A
kept**, and A's cues move to B. In the overlay that is `remap`; once a
supersession really happens (`remember` on a fact that supersedes another) the
cues are copied to the replacement and the corpus is rewritten
(`replay.Store.Remap`), so later replays keep counting it. A rewritten note's
facts are paired with their old versions by word overlap (Jaccard 0.4 or more)
when the rewrite re-minted their ids.


## Measured

`TestReplayEval` (`go/internal/api/memory_replay_eval_test.go`, run with
`REPLAY_EVAL_VAULT` set; no LLM calls) on a copy of the live vault: 889
situations, 1,315 memories, 173 gold notes against 887 candidate notes. It
simulated 50 wording edits and 50 merges, each judged against two truths:
round 1 (the cues it learned, in-sample) and round 2 (held out).

| Gate, 50 edits | harmful | held | precision | recall |
|---|---|---|---|---|
| vs round-1 truth (in-sample) | 13 | 10 | 0.90 | 0.69 |
| vs round-2 truth (held out) | 6 | 10 | 0.30 | 0.50 |
| vs either | 14 | 10 | 0.90 | 0.64 |

| Gate, 50 merges | harmful | held | precision | recall |
|---|---|---|---|---|
| vs round-1 truth (in-sample) | 5 | 3 | 0.33 | 0.20 |
| vs round-2 truth (held out) | 12 | 3 | 0.67 | 0.17 |
| vs either | 14 | 3 | 0.67 | 0.14 |

Across all 100 trials, held-out precision is 0.38 and recall 0.28; against
either truth, 0.85 and 0.39. On held-out merges, 13 useful recalls were lost
with the merge's cues carried over and 14 without; merges held with remap were
3, without remap 6. The gate therefore catches only a minority of the harmful
merges, and it holds some edits that do no harm on held-out truth. Treat
`replay_max_lost` as a warning, not a guarantee.

Parity with the real endpoint, round 1: identical fire sets 550/908; gold hit
agrees 444/588; replay fires 358 memories the endpoint does not and misses 341
it fires. Mean replay time: 715 ms per edit, 397 ms per merge.
