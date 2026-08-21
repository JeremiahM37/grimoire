# LongMemEval through the memory engine — results

Method: `run_memory_arm.py`. Raw output: `results_memory/`. Same frozen
questions, same reader (`claude-haiku-4-5`) and same judge (`claude-sonnet-5`)
as the published table, so every row here lines up with one there.

**This is a negative result, and the most useful thing produced tonight.**

---

## The question

Every published Grimoire number ingests a haystack as plain session **notes**
and retrieves passages from them. That measures retrieval and never touches the
memory engine — the half of the product that decides what a later statement
does to an earlier one, and the half mem0, Zep and Letta are built around.

So: ingest the same haystacks through `POST /api/memory` — facts extracted per
turn, reconciled against everything already written — and answer from recalled
facts instead of chunks. Scoped to `knowledge-update`, the category the
mechanism exists for.

Three arms differing in one thing each:

| arm | what a write may supersede |
|---|---|
| `append` | nothing across sessions (`scope: session`) — an append-only store |
| `reconciled` | anything in the vault, using the shipped rules |
| `slots` | the same, plus value-slot updates (see [REPORT-slots.md](REPORT-slots.md)) |

## The result

n = 31 knowledge-update questions.

| arm | correct | live facts (median) | context chars (median) |
|---|---|---|---|
| memory · append | 20/31 (64.5%) | 723 | 3,676 |
| memory · reconciled | 21/31 (67.7%) | 707 | 3,680 |
| memory · slots | 20/31 (64.5%) | 695 | 3,659 |
| **chunks · hybrid (published)** | **26/31 (83.9%)** | — | — |
| chunks · shipped path (published) | 25/31 (80.6%) | — | — |
| entire haystack in context (published) | 25/31 (80.6%) | — | — |

**Two findings, both against the interesting hypothesis:**

1. **Every memory arm loses to chunk retrieval by 13–19 points.** The shipped
   design — ingest sessions as notes, retrieve passages — is better on this
   category than routing the same text through the fact engine.
2. **Reconciliation makes no measurable difference end to end.** Every
   pairwise contrast is p = 1.00:

   ```
   reconciled vs append  +2 / -1 discordant   p = 1.00
   slots      vs append  +4 / -4 discordant   p = 1.00
   slots      vs reconciled  +2 / -3          p = 1.00
   ```

   And it is not because nothing happened. The arms differ exactly as designed:

   | arm | supersessions |
   |---|---|
   | append | 151 / 23,367 (0.65%) |
   | reconciled | 208 / 23,367 (0.89%) |
   | slots | **348 / 23,367 (1.49%)** |

   The slots arm superseded 2.3× as often as the shipped rules, produced a
   store with 28 fewer live facts per haystack, and answered the same number of
   questions correctly.

## Why — and what it means for the product

The mechanism works in isolation: measured directly, value-slot matching takes
recognised updates from 1/72 to 20/72 with no reader in the loop
([REPORT-slots.md](REPORT-slots.md)). It does not move this number because
**reconciliation is not the bottleneck here**.

Ingesting a 50-session chat haystack through a sentence-level extractor
produces ~700 live facts per question, most of them conversational fragments
("Do you have any tips on how to improve my endurance?"). Recall returns 30.
Whether two of those 700 got merged is invisible next to whether the gold fact
is in the 30 at all.

The design conclusion is the opposite of the hypothesis, and worth stating
plainly: **the memory engine is for facts an agent decided to write down, not
for bulk transcript ingestion.** Short, deliberate, canonical statements —
"the user prefers tabs", "the deploy host is prod-1" — are what it reconciles
well and what `remember` is for. Grimoire should keep ingesting documents as
documents and retrieving passages, which is what it does.

That also reframes the comparison with mem0 and Zep: their published numbers
come from exactly this configuration — extract facts from a transcript, recall
facts — and this run says that configuration is *worse* than plain hybrid
retrieval over the same text, with the same reader and judge. That is one
harness, one category, 31 questions, and not a claim about their systems; it is
a reason to be suspicious of the configuration itself.

## Limitations, which are severe

- **n = 31, with 4–7 discordant pairs.** This study can detect an enormous
  effect and nothing else. "No measurable difference" here means "nothing this
  study could see", not "no difference".
- **One category.** knowledge-update was chosen because it is where
  reconciliation should matter most. The other five were not run.
- **The extractor is the rule-based one.** With an LLM configured,
  `ExtractFacts` splits clauses rather than sentences and `DecideMemoryFrom`
  makes a model judgement — a different and probably much better pipeline,
  which is also the expensive one this project's design deliberately avoids
  requiring. Untested here.
- **Facts carry wall-clock stamps, not session dates**, because the write API
  takes no stamp override. Recency decay is therefore flat across a haystack,
  which can only hurt these arms.
- Sessions are ingested in **date order**; feeding them in storage order would
  let a stale fact supersede a current one and would measure the shuffle.
