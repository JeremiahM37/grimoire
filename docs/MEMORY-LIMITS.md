# What agent memory still gets wrong, and what Grimoire does about it

Researched 2026-10-10 from 2025-2026 papers, benchmark audits and engineering
write-ups. Evidence quality is marked: **measured here** means a test or
benchmark in this repository covers it; **design** means the mechanism exists
but nothing here measures how much it helps; **open** means unsolved.

The point of this page is the last column. A memory layer that lists only what
it fixes is advertising.

| # | Documented limitation | What Grimoire does | Status |
|---|---|---|---|
| 1 | Retrieval degrades with store size; near-duplicate distractors hurt most | Hybrid FTS + vector + entity arms with rank fusion; bounded top-k; opt-in query expansion and a bounded entity-graph walk (`expand=true`, `hops=1\|2`) | **measured here** (`benchmarks/latency`, synthetic corpus, offline embedder): gold-in-top-10 is 88% at 1k facts, 70% at 10k and 60.5% at 50k, up from 30% at 50k before candidates stopped depending on a recency window; a phrase-run term (`RECALL-SCALE.md`) adds 2 points at 50k (62.5%). 59 of the 66 remaining 50k misses are first-name-only facts that the corpus makes deliberately ambiguous, so the ceiling for ranking alone on this corpus is about 68.5%; the 80% target needs entity linking from outside the text and is not met |
| 2 | Superseded facts are served as current; implicit updates are the hardest case | Write-time reconciliation, `valid_from`/`valid_to` alongside belief time, `valid_at`/`as_of`/range recall, freshness marks (`use`/`verify`) | design; slot rules measured in `benchmarks/longmemeval/REPORT-slots.md` |
| 3 | Memory is a persistent injection channel | Untrusted origins fenced and unable to supersede human-written facts; dream security sweep that now reads obfuscated text (zero-width, homoglyphs, base64/hex/rot13/percent, HTML comments), broader phrase families incl. four non-English languages, and no model rewrite of a note holding pulled text (`INJECTION-DEFENSE.md`) | **measured here**, offline: a fresh held-out set is caught 80%, 5% of hard-negative sentences are flagged, 0.7% of 420 benign repo documents. Paraphrase is still the main bypass, and bank observations carry no origin taint |
| 4 | Cross-scope leakage in shared memory (one multi-tenant service leaked on ~44% of deny probes in a 2026 evaluation) | Access filtering at the index; a deny-probe suite over every route, MCP tool, stream and export, which fails when a new route has no probe | **measured here**: the suite found and fixed 3 real leaks (see `SECURITY-LEAKPROBE.md`) |
| 5 | Deleting a source leaves derived summaries, observations, caches and index rows | `forget --cascade` removes or redacts derived artefacts, purges index rows, then re-searches the index and writes a receipt holding only a salted hash | **measured here** (tests); paraphrases only caught with an embedder. `forget` also writes an HMAC-signed tombstone that sync and cloud pull honour, and a sealed fact's data key is destroyed (crypto-shred) so old ciphertext is unreadable, provided the key store is kept outside the copies (`MEMORY-SEAL.md`). A copy holding both ciphertext and key, and anything already in a model context or log, is out of reach |
| 6 | Stored preferences degrade factual answers and raise sycophancy | `mode=factual` recall excludes personal categories; `exclude_personal` for profiles | design; category-based, so uncategorised preferences still pass |
| 7 | Observed evidence, extracted facts and model inferences are collapsed into one record | A derived `basis` on every entry (`stated`, `observed`, `inferred`, `imported`, `pulled`) and an `evidence` field on writes | **measured here** (tests): an agent's cited evidence is checked to exist (a vault note and quote, a document hash, a fetched URL, an entry id) and unverified evidence caps the basis at `inferred` (`evv=` trailer, `PROVENANCE.md`). That proves the citation exists, not that it is true, and facts written before this are not re-checked |
| 8 | Memory can lower task performance; recall accuracy is not task success | Per-memory utilization trace with a randomized holdout, importance and use-based retention | design for the estimator; a first task-level check (`EVAL-TASK-BENEFIT.md`, 10 invented conventions, Haiku reader): memory off 0/30, on 15/30 before a stemming fix lifted retrieval from 5 to 7 of 10 tasks. Ten authored tasks cannot show a general effect |
| 9 | LLM extraction on every write is slow and expensive | Deterministic rule path on the hot write; a model is optional and bounded by the rules' vocabulary | **measured here** (`benchmarks/latency`) |
| 10 | Benchmarks are unreliable: flawed answer keys, lenient judges, vendor-run numbers | Pre-registered protocols, full-context and no-memory baselines, null results published; no numbers here claim a ranking over other systems | see `benchmarks/*/REPORT.md`. A judge-validity check on 30 authored wrong-but-adjacent LoCoMo answers found the Haiku judge accepted 0/30 (CI 0-11%) and falsely rejected 1/10 paraphrases; that does not describe LoCoMo's own wrong answers. A 20-question BEAM slice (one conversation, Haiku): rubric 66.8%, strict 45%, not comparable to the paper |
| 11 | Skills learned from failures can compound the failure | Mined skill candidates are drafts in a review queue; nothing enters memory until a person accepts one. A statistical gate (support, false-fire rate on held-out sessions, failure contamination) now blocks `accept` unless a person overrides with a recorded reason (`SKILL-GATE.md`) | **measured here** on fabricated sessions only; a sanity gate over recorded sessions, not proof a skill works, and thresholds are untuned |
| 12 | Portability: memory trapped in one product | Open JSONL/markdown export; importers for three other systems' formats | **measured here** (round-trip tests) |
| 13 | Private facts reach default reads, exports and explanations | A `vis=private`/`sensitive` tag: hidden in SQL from every default read, never in export or the stream, sensitive text redacted from explain and receipts | **measured here** (`TestLeakprobeHiddenMemoryStaysHidden`). `private`/`sensitive` remain retrieval filters. `vis=sealed` is the real boundary: text is AEAD-encrypted under a per-fact key, absent from the note file, full-text index and exports, and only its owner reads it (`MEMORY-SEAL.md`). A locked vault or an unrestricted single-user deployment weakens that, and a sealed fact skips model extraction |
| 14 | Context caches miss because recalled memory reorders every turn | `GET /api/memory/prefix`: a deterministic, append-mostly block with a hash chain, so a caller can send only the delta or learn the prefix was rewritten (`PROMPT-CACHE.md`) | **measured here** (byte-stability tests); the cache-hit saving depends on the provider and is not measured here |
| 15 | Contradictions are flagged but nobody can settle them | `GET /api/memory/disputes` and a resolve call (keep, accept challenger, merge) that writes a human-authority entry; agents are refused (`DISPUTES.md`) | **measured here** (tests); the write-path leak probe proves refusal by absence only |
| 16 | Agents cannot ask where code lives or who calls it | A symbol/call graph for Go, Python and TS/JS, admin-only, path-allowlisted. Go calls are resolved by type (`go/types`, tolerant), Python and TS/JS by scope and imports, and every edge carries a confidence (`CODE-GRAPH.md`) | **measured here** (fixtures; 20 hand-checked Go edges with no wrong target, too few for a rate). On this repo 36% of edges resolve, 63% are unresolved external calls; cross-package methods and Python/TS typing stay unresolved |
| 17 | Memory is text-only | Pictures stored content-addressed, retrieved by caption, and findable by pixel similarity (64-bit dHash plus a colour histogram); an opt-in captioner calls a vision endpoint you configure (`IMAGE-MEMORY.md`) | **measured here** (generated images). Similarity is visual, not semantic; it is blind to rotation, flips and crops, WebP has no signature, and no real photo corpus was measured |
| 18 | Agent trust is a fixed ranking, not learned | Per-agent reliability from correction events (confirmed, challenged, superseded or deleted by a human) as a Beta posterior lower bound, used in dispute suggestions only past five events (`CALIBRATION.md`) | **measured here** on simulated streams only: a clear gap stabilises after ~14 events per agent, a near-tie after ~77, and a single event can flip a near-tie. Real correction rates are unknown and corrections are sparse |
| 19 | Retention is heuristic, not learned from use | `RetentionScore` orders prune proposals by use and recall; frequently used, human, immutable, challenged, private and highly rated facts are never proposed | design; proposals only, nothing is deleted automatically, and it is not a measure of truth |
| 20 | Contradictions are flagged but not weighed | Disputes carry a suggested winner and a per-term breakdown (authority, verification, corroboration, recency, reliability); a human side always outranks a non-human one and nothing auto-resolves (`PROVENANCE.md`) | **measured here** (tests); the weights are judgement, not fitted to adjudications |
| 21 | Knowledge can only be retrieved, never baked in | `grimoire memory distill` selects only facts safe to bake (not private or sealed, not superseded or disputed, verified, aged, used) and writes train and held-out chat JSONL, abstain examples, an exclusion report and a manifest for staleness diffs (`PARAMETRIC.md`) | design; it exports data and trains nothing, and weights cannot be forgotten like a note |
| 22 | Procedures are prose, with no checkable preconditions | A skill can carry structured preconditions and postconditions; `skills check` reports which are unmet before a skill is applied (`SKILL-GATE.md`) | **measured here** (fixtures); only file, command, env and fact predicates exist |

## Still open

- **Recall accuracy still decays with store size.** The 50k figure above
  misses the 80% target set for it. The scan-window defect is fixed; ranking
  against many near-duplicate facts about the same entity is not.
- **Benchmark validity is not fixed by being careful.** The best available
  independent audit found a lenient judge accepting most deliberately wrong but
  topically adjacent answers. Any LLM-judged score, ours included, inherits
  that. Treat single-run differences of a few points as noise.
- **Deletion has no guarantee outside the vault.** Tombstones and crypto-shred
  help only where a peer applies them or the key store sits outside the copy.
  A backup that holds both ciphertext and key, and plaintext already shown to a
  model, keep what they had.
- **Basis is only partly attested.** Cited evidence is now checked to exist, but
  an existing citation is not shown to be true, an agent that writes an
  inference without marking it is recorded as having inferred, and facts
  written before the check keep the basis they had.
- **Personal-versus-factual is a category rule.** It does not recognise a
  preference that nobody categorised.
- **Task-level benefit is not demonstrated.** Utilization tracing estimates
  whether a recalled memory was used; it does not show the task went better.
- **Privacy model.** Agent memory lives in a shared commons by design: every
  account can read `memory/<topic>.md`. The per-fact `vis=` tag and the
  note-level `private` flag are retrieval filters, not owner boundaries, and
  a member who passes `include_private=1` reads another member's private
  fact. The tag keeps facts out of default reads, export and the stream; it
  does not encrypt the file or stop raw note reads and full-text search. Single-user
  deployments are unaffected; multi-user ones should read
  `MEMORY-PRIVACY.md` and `SECURITY-LEAKPROBE.md` before relying on isolation.
- **BEAM is a 20-question slice, not a result.** One 100K conversation, Haiku
  reader and judge; no number here should be compared to the paper.
- **Images are recalled by caption, not by meaning.** Pixel similarity finds
  near-duplicates, not "a photo of a dog"; an uncaptioned picture is not
  returned by recall.
- **Code call edges are partly resolved.** Go resolves within a package by type;
  everything crossing a package boundary, and Python and TS/JS typing, stays
  unresolved or heuristic.
- **Recall at 50k stays below target.** The synthetic corpus makes first-name
  aliases ambiguous on purpose, so about a third of queries are not answerable
  from text. Closing the gap needs entity linking from outside the text.
- **Learned trust and retention are simulated, not observed.** No real
  correction history exists yet to validate either.
- **Injection defense is pattern-based.** A paraphrase that matches no known
  family still passes, and bank observations built from injected facts carry no
  origin taint.
- **Task benefit remains unproven in general.** One 10-task study on invented
  conventions shows memory can only help where retrieval finds the fact.
