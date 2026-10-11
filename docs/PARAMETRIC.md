# Parametric memory export (`grimoire memory distill`)

Status: **built and tested as a data exporter. Nothing here trains a model, and
no model was trained or evaluated while building it.** This document is the
recipe for doing that yourself, plus the reasons it should be done carefully.

## What exists

- `go/internal/distill`: `Select` applies the bake-in rules and records one
  exclusion reason per refused fact; `Build` turns the selection into chat-format
  examples from fixed templates (no model call); `Run` writes the four output
  files.
- `grimoire memory distill --out DIR [--min-age-days N] [--min-uses N]`
  (defaults 30 and 0). Read-only: it reads every fact from the index and writes
  only inside `DIR`. The vault and the index are not changed.

Output in `DIR`:

| File | Contents |
|---|---|
| `train.jsonl` | Chat-format examples for the student. Each line: `messages` (system, user, assistant) plus audit keys `id`, `template`, `kind`. |
| `eval.jsonl` | Held-out examples over the same facts, using different templates. Same shape. |
| `exclusions.json` | Every refused fact: `id`, `note`, `reason`. Never the text. |
| `manifest.json` | Date, thresholds, counts (including per-reason totals), template IDs per split, a sha256 per file, a `facts` list (id, note, sha256 of the text), and the content hash. |

Two runs over the same facts on the same date are byte-identical. The test
`TestOutputIsByteIdenticalAcrossRuns` checks this.

## Selection rules

A fact is selected only if it passes every check. The first failing check is its
recorded reason.

| Reason | Rule |
|---|---|
| `sealed`, `private`, `sensitive` | `vis=` is not normal. Hidden facts never leave the store. |
| `superseded` | A later fact replaced it. |
| `expired`, `validity_ended` | Past `expires`, or past `valid_to`. |
| `image` | An image caption. Not text to bake in. |
| `open_challenge` | This fact contests another, and the challenge is unresolved. |
| `disputed` | Another live fact contests this one. Both sides of an open dispute are refused. |
| `imported`, `untrusted_origin` | Came from an import, or from text other people can write. |
| `unverified_evidence` | Cited evidence that did not resolve. |
| `inferred` | An agent's bare assertion, or a synthesis (consolidation, reflection, dream). |
| `agent_unverified` | Agent-written, with evidence but no verification recorded. |
| `no_timestamp`, `too_young` | Stamp unreadable, or younger than `--min-age-days`. Age is measured from the stamp of the current version, which is the conservative choice. |
| `too_few_uses` | Used fewer than `--min-uses` times (only when set). |

So the admitted set is: a person's assertion, or an agent fact with verified
evidence, that is current, old enough, and not in dispute.

## Examples

Facts that parse as subject-predicate-value (`memory.Attribute`) produce two
training examples and two held-out examples:

- train `train-qa-fact`: "What does your memory say about *subject*?" → the fact
- train `train-complete-blank`: "Complete this memory entry: *subject* *predicate* ___" → the value
- held-out `heldout-recall-fact`: "Recall what is on file about *subject*." → the fact
- held-out `heldout-fill-blank`: "Fill in from memory: *subject* *predicate* ?" → the value

Other facts use a positional key (first six normalized words) and one template
per split. That key is weak; the manifest's `templates` block and the counts
show how much of the set relies on it.

Abstain examples: one per selected fact, in each split. The question names a
subject built from a fixed word list that appears in no fact in the store, and
the answer is "I do not have that in my memory, so I cannot say." Train and
held-out abstain subjects come from disjoint runs of the generator.

The two splits never share a question string or a template ID. The test
`TestHeldOutHasNoTemplateOverlap` checks both. Held-out examples reuse the same
facts on purpose: the question is whether the fact can be recalled under
phrasings the student did not see, not whether it was memorised as a string.

## Recipe: LoRA SFT on a small local model

These values are a starting point. They are not measured on this vault.

- Base: a small instruct model you can run locally (the homelab has
  qwen3.5:4b and gemma4:e4b on Ollama; use whichever your trainer supports).
- Adapter: LoRA, rank 16, alpha 32, dropout 0.05, target `q_proj,k_proj,v_proj,o_proj`.
- Optimiser: AdamW, learning rate 2e-4, cosine decay, warmup 3%.
- Schedule: 2 to 3 epochs. The set is small, and more epochs mostly memorise
  phrasing. Stop early if the held-out score falls.
- Batch: 8, with gradient accumulation to fit memory. Max sequence length 512.
- Loss on the assistant turn only.
- Use the `messages` field and ignore `id`, `template`, `kind`.

## Held-out eval

1. Load the adapter, and run the base model with the adapter disabled as a control.
2. For each line in `eval.jsonl`, send `messages[0..1]` (system and user) and
   collect the reply.
3. Score by `kind`:
   - `fact`: the reply contains the reference answer's key content. Use the
     string match for the value on `heldout-fill-blank` and a token-overlap
     threshold on `heldout-recall-fact`. Fix the threshold before you look.
   - `abstain`: the reply declines (matches the abstain answer's meaning). Count
     any confident answer to an abstain question as a hallucination.
4. Report both numbers, with the control, and with the number of runs. A single
   run is not a result.

## Detecting a stale baked fact

Baked weights do not forget. A fact that is later superseded, expired or
disputed stays in the model until it is retrained, and nothing removes it. So:

1. Rerun the export on the current vault, to a new directory.
2. Diff the two manifests' `facts` lists:

   ```bash
   diff <(jq -r '.facts[] | "\(.note) \(.id) \(.sha256)"' old/manifest.json | sort) \
        <(jq -r '.facts[] | "\(.note) \(.id) \(.sha256)"' new/manifest.json | sort)
   ```

   Lines starting with `<` are facts the trained model may still believe that
   are no longer selected. Lines starting with `>` are facts that changed or
   were added since training.
3. Any `<` line means the model is stale for that fact. Retrain, or accept and
   record that the model is behind.

`distill.FactDiff` does the same comparison in Go.

## Honest limits

- **Weights cannot be forgotten like a note.** Deleting a fact from the vault
  does not remove it from a trained model. Re-validation is the only control:
  rerun the diff, retrain when it is non-empty.
- **A baked fact is not re-checked.** The manifest is a snapshot taken when the
  export ran. Nothing watches the vault afterwards.
- **Selection is only as good as the stamps and evidence.** A wrong fact with a
  person's assertion and an old timestamp passes every rule. Those rules are
  there to make a bad admission rare and visible, not impossible.
- **The positional key is weak.** Facts without a subject-predicate-value shape
  get one question per split.
- **Abstain training is not calibration.** It teaches declining on subjects the
  store does not hold. It does not teach the model when its own knowledge of a
  held fact is uncertain.
- **Trained weights may memorise private text in ways a file does not.** Only
  facts that passed the visibility checks are exported, but the trained model
  can reproduce its training text. Treat the adapter as sensitive as the vault.
- **This exports data; it does not train anything.** There is no training code
  in the repository.

## What is not done

- No training run, no held-out score, and no comparison with retrieval. These
  are the measurements that would decide whether this is worth doing, and they
  are open.
- No HTTP route. Export is CLI only, so no leak probe is needed.
- The examples are template-generated. Whether paraphrased or model-written
  questions would generalise better is untested.
