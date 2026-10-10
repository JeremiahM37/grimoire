# Grimoire — Architecture

A map for contributors. The one-paragraph version: **plain markdown files are
the source of truth; everything else is a rebuildable cache or a view.** The
server is one static Go binary over SQLite (FTS5, pure-Go driver, no cgo); the
client is a no-build vanilla-JS PWA with one vendored artifact (the CodeMirror 6
live editor).

## Invariants (break these and you've broken Grimoire)

1. **The vault is truth.** `*.md` files under `GRIMOIRE_VAULT` are the data.
   The SQLite index (`.grimoire/index.db`) can be deleted at any time and is
   rebuilt on boot (`index.reindex()`); the watcher reconciles external edits.
2. **Escape first, then render.** Both markdown renderers (server
   `go/internal/render`, client `mdToHtml`) HTML-escape input before applying
   rules. Any new rule operates on escaped text.
3. **Private/encrypted never leaks.** Private notes are excluded from vectors,
   RAG, `/read`, export, transclusion and live queries on unauthenticated
   surfaces. Encrypted notes exist on disk / in history / in trash only as
   ciphertext (`grimoire:enc:v1:…`); plaintext lives in memory and the
   authenticated API only, and never in localStorage drafts.
4. **User input never reaches SQL/FTS as syntax.** Parameterized queries only;
   FTS input is quoted; sort keys and columns come from whitelists
   (`go/internal/queries`).
5. **Paths are confined.** All file access goes through `Vault.SafePath` /
   `SafeRawPath` (or the plugin/history equivalents), which resolve and verify
   containment. `.grimoire/` is reserved.
6. **Provenance travels with the text.** Every note has an `origin`, every
   retrieval hit reports the trust level derived from it, and the derivation
   lives in exactly one place (`internal/trust`). A surface that returns note
   text and does not say where it came from is a bug, not an omission — the
   whole model rests on a caller being able to tell your writing from a
   stranger's. Corollary: untrusted text may never supersede a trusted memory
   fact, and the check belongs where the candidates are chosen, not where the
   answer is filtered.

## Server (`go/`)

| Package | Responsibility |
|--------|----------------|
| `cmd/grimoire` | The binary: server *and* CLI. Env-driven config; `GRIMOIRE_*` variables |
| `cmd/grimoire-mcp` | The agent interface: knowledge tools + `remember`/`recall` + `use_credential`/`list_grants` |
| `internal/db` | SQLite connection + schema (modernc.org/sqlite — pure Go, no cgo) |
| `internal/vault` | File I/O, path safety, slugs, note serialization |
| `internal/markdown` | Parsing: frontmatter (order-preserving), links, tags, title derivation |
| `internal/index` | vault ⇄ index reconciliation; links/tags/FTS/vectors rows; hybrid retrieval |
| `internal/render` | Markdown → safe HTML (link map, transclusion, queries) for `/read` + export |
| `internal/queries` | Live-query engine: typed parse → parameterized execution |
| `internal/history` | Per-note version snapshots (ring buffer) |
| `internal/embed` | Embedders: hashing floor, built-in model2vec, Ollama, OpenAI-compatible |
| `internal/bank` | Memory banks: retain content into facts (chunk → extract → resolve entities → markdown), four-arm recall; see [MEMORY_BANKS.md](MEMORY_BANKS.md) |
| `internal/ai` | Answer synthesis, question decomposition, reranking, consolidation, transcription |
| `internal/crypto`, `internal/secrets` | Argon2id KDF, Fernet sealing, secret vault + broker, note encryption, just-in-time grant requests |
| `internal/trust` | Origin → trust level, and the fence untrusted passages are wrapped in before a reader sees them |
| `internal/eval` | Frozen question sets + judge-free scoring of retrieval on the operator's own vault |
| `internal/readlog` | The restricted-read trail, and the burst detector that reads it back |
| `internal/crdt`, `internal/crdtstore` | Sequence CRDT for concurrent note-body merges |
| `internal/sync` | Bidirectional delta sync with a peer |
| `internal/cloudsync` | Sync and backup through a cloud-drive folder: encrypted, one writer per file |
| `internal/watcher` | Debounced filesystem watcher |
| `internal/settings` | UI-editable operational settings (`.grimoire/settings.json`) |
| `internal/api` | HTTP surface, one file per domain; `Routes()` assembles + security headers |

Retrieval has TWO paths and they are not interchangeable. `/api/retrieve` ranks
one query; `/api/retrieve?smart=1` decomposes the question, retrieves per
sub-question and reranks the pool. The second was unreachable on its own until
1.0, with two consequences: the console's "what would the agent see" inspected
a different ranking from the one the agent used, and every published benchmark
number was measured on the plain path rather than the one `/api/ask` used.

Both are now resolved, and the resolution went the other way from the guess:
**`/api/ask` no longer decomposes by default** (`smart: true` opts in), because
measuring it showed plain retrieval nominally ahead at 1/70th the latency —
see benchmarks/longmemeval/REPORT-multihop.md. The console inspects the same
path the answer uses, and the benchmarks now measure the shipped default.
With no LLM configured the two paths are identical anyway, which is why this
is a parameter rather than a mode.

Route-ordering rule: `/api/notes/{path...}` is greedy, and Go's ServeMux
requires a `{path...}` wildcard to be the LAST segment of a pattern — so the
per-note actions (`/pin`, `/rename`, `/history/…`) cannot be routed by pattern
at all. They are dispatched on the trailing segment in `internal/api/dispatch.go`,
and `/api/notes/random` is registered as its own literal route.

## Client (`frontend/` and `web/`)

The console is the React/TypeScript app in `frontend/` (Vite; `npm run
build --prefix frontend` writes `frontend/dist`, which the server prefers and
the release archives ship as `web/`). What remains under `web/` is what the
build draws from:

| File | Responsibility |
|------|----------------|
| `style.css` | The console's stylesheet, imported by `frontend/src/main.tsx` |
| `vendor/editor.js` | Built CM6 bundle — generated by `tools/build-editor.mjs`, never edited by hand; copied into the build as `/vendor/editor.js` |
| `manifest.webmanifest`, `icon.svg`, `apple-touch-icon.png` | PWA manifest and icons |

The service worker is `frontend`'s `react-sw.js`. The pre-React modules
(`app.js`, `markdown.js`, `plugins.js`, …) are gone; `internal/render` is the
markdown engine now.

## Plugins (`plugins/` + `<vault>/plugins/`)

Manifest + ES-module entry; built-ins trusted/enabled, vault plugins disabled
until explicitly enabled. Full contract in [PLUGINS.md](PLUGINS.md).

## Data flows worth knowing

* **Save**: editor → debounced `PUT /api/notes/{path}` → history snapshot of
  the previous body → vault write → `index.upsert` → SSE-less "rev" polling
  picks it up on other open devices.
* **Query block**: preview renders a placeholder → `POST /api/query`
  (authenticated, may see private) → hydrated. `/read`/export run the same
  engine server-side with `include_private=False`.
* **Sync**: CRDT-first per note (shared atom ids after first contact),
  conflict-copy fallback — data is never silently lost.
* **Folder sync** (`internal/cloudsync`): each device writes only
  `GrimoireSync/devices/<id>/` (an encrypted manifest of `{path, keyed hash,
  mtime, deleted, ancestry}` plus content-addressed encrypted blobs) and reads
  everyone else's, so a cloud drive never sees two writers on one file. A
  round: scan the vault (stat cache) → fold local edits into this device's
  manifest → for each peer, diff its manifest against the entry last merged
  from it (the per-peer base) → fast-forward when the peer's ancestry contains
  our version, ignore it when ours contains theirs, otherwise merge through
  the same CRDT documents the peer sync uses (shipped as blobs) or keep a
  conflict copy → upload blobs, then publish the manifest. Unavailable
  objects are skipped and retried, never read as deletions. Device-local
  state and the key live in `.grimoire/cloudsync/`.
* **Pulled document**: connector → `Runner.write` sets `origin:` in the
  frontmatter → ordinary note write → `index.upsert` stamps `notes.untrusted`
  and `vectors.untrusted` → ranking can exclude it (`Filter.TrustedOnly`,
  applied INSIDE ranking so corpus statistics do not carry it) → `ai.Context`
  carries `Untrusted` → `RenderReaderPrompt` fences it. Every hop is where it
  is for a reason; the one that is easy to get wrong is the filter's position,
  because filtering the OUTPUT still lets poisoned rows move BM25.
* **Confirming a note**: console → `POST /api/stale/verify` → `verified:` into
  the note's frontmatter → `index.upsert` → the retrieval cache is PATCHED, not
  rebuilt, so every per-note field the cache holds has to be refreshed there.
  Missing one is invisible until a warm server disagrees with a cold one.

* **Memory bank retain**: `POST /api/banks/{bank}/memories` → chunk and diff
  against the stored chunk hashes → extract changed chunks (model, or
  sentence rules) → resolve entities against the bank → write
  `banks/<bank>/documents/<doc>.md` and `facts/<doc>.md` → `index.upsert`,
  whose bank hook rebuilds that file's `bank_*` rows. A person's edits are
  read from the facts file itself (no trailer, `by=human`, or text that no
  longer matches its hash) and survive every rewrite.

## Agent memory: two time axes

Every memory fact is bi-temporal. The two axes answer different questions, and
a query can ask about both at once.

| Axis | Fields | Meaning |
|------|--------|---------|
| Belief time | `stamp`, `supat` (`superseded_at`) | When the store came to believe the fact, and when a later fact replaced it |
| Validity (event) time | `valid_from`, `valid_to` | When the fact was true in the world |

Both live in the bullet trailer (`internal/memory/entry.go`), and the markdown
stays the source of truth. A bullet with no validity fields is unchanged and
round-trips byte-identically, so existing notes need no migration. A bound is
RFC3339 or `YYYY-MM-DD`. Stored bounds are canonical RFC3339 UTC, and a date is
midnight UTC so the answer does not depend on the server's timezone. Intervals
are half-open: a fact is true on `[valid_from, valid_to)`.

**Supersession closes validity.** When a new fact with a `valid_from` replaces
an old one, the old fact's `valid_to` is set to that instant, but only if it
has none. A `valid_to` a person set is never overwritten, and a replacement
that starts before the old fact did closes nothing (that is a history
correction, and it should be made explicitly).

**Query surface.** `recall` (and `GET /api/memory`, and the MCP `recall` tool)
take:

* `as_of`: what was believed at an instant (unchanged)
* `valid_at`: facts true in the world at an instant
* `valid_since` / `valid_until`: facts true at some point in a closed range

A fact with no validity is always valid. Combined, `as_of` and `valid_at` answer
"what did we believe on A about what was true on B". The belief axis also
governs validity: a closure is written into the old bullet with no time of its
own, so under `as_of` a `valid_to` is only applied if the fact had been
superseded by then; otherwise the closure would answer an earlier question with
a bound learned later (`memory.Entry.ValidAtAsOf`).

**Where it is evaluated.** The index stores both bounds as canonical text
columns, added through the conditional column migration. `valid_at` and the
ranges are SQL predicates. Under `as_of` they are SQL predicates too: the belief
rule compares minute-precision local stamps, which compare correctly as fixed
width text against the instant rendered in the same format, so the predicate is
exact, not a superset (see "Candidate generation above the scan bound"). The
Go-side check still runs on every row that comes back.

**Not covered.** Vector search (`POST /api/memory/search-vector`) does not take
validity parameters yet. Editing the bounds through `PATCH /api/memory` is not
supported; change the trailer or write a corrected fact instead.

## Agent memory: candidate generation above the scan bound

Recall scores at most `DefaultScanLimit` facts (20,000; `GRIMOIRE_SCAN_LIMIT`
overrides it, so a test can put a small corpus over the bound). Below the bound
the answer is the same as an unbounded scan. The window is the newest facts that
pass every check, streamed in recency order until the bound is full.

Above the bound that window is the wrong candidate set: an old fact that answers
the question is simply never scored. A measured 50,000-fact corpus returned the
gold fact in the top ten for 30% of targets, against 88% at 1,000 facts
(`benchmarks/latency/REPORT.md`). So a recall with text or a vector, over the
bound, takes its candidates from four arms over the same filtered set
(`internal/index/memory_candidates.go`):

| Arm | Source | Size |
|---|---|---|
| newest | `ORDER BY stamp DESC` | pool |
| lexical | `memory_fts` (FTS5, porter unicode61) by `bm25`, query terms OR-ed and quoted | pool |
| entity | `memory_entities` rows sharing a stored entity with the query, newest first | pool |
| semantic | brute-force cosine; the first pass decodes only the `embedding` column and keeps a bounded heap | pool |

`pool` is `scanLimit / 10`, with a floor of 10, so the union is at most four
pools (8,000 rows at the default bound). The union's full rows are fetched by
rowid, pass through the same `accept` check as the window, and are ranked by the
unchanged `rankMemory`. The ranking formula, its weights and its tie-breaks are
not touched by this path; only which rows reach it differ.

**Queries with no text or vector** (a filter alone, or `as_of` with no words)
keep the window, which now sees only rows that pass the time predicates. The
window is the right answer for a filter: nothing about the question favours an
old fact.

**FTS table.** `memory_fts` is an external-content FTS5 table over
`memory_entries.text`. Insert, delete and update triggers keep it in step with
every write path: a note rewrite (delete then insert), a removed note, and a full
reindex. A database that predates the table is backfilled on open with the FTS5
`rebuild` command. Tests check it with FTS5's `integrity-check` after each kind
of write.

Supersession does not remove a row from the FTS table, and that is deliberate.
The superseded fact is still a row in `memory_entries` with its old text, and
`as_of` is defined to answer from exactly those rows. Default recall excludes
superseded facts with the existing predicate, so they never surface; removing
them from the FTS table would make `as_of` unable to find them, which is the
one question superseded facts exist to answer. A forgotten fact is deleted, and
its FTS row goes with it.

**Pushdown.** Everything that can be decided in SQL is decided before the bound,
so the bound counts only rows that can answer the question: the note, path, id,
agent, task, session, category, mode and superseded filters; `as_of` (written by
then, not replaced by then, and validity as it was known then); `valid_at` and
the ranges; `private`; and the space and reader-list rule. The reader-list rule
is an exact image of `aclAllows` (`acl=''`, or `instr(acl, ',user,')`), and the
Go check `allows` still runs on every row. Expiry stays in Go, because the stored
`expires` text is not canonical; the streamed window continues past rows that
expiry rejects, so they do not shrink the answer.

On the candidate path an expired fact can still take a pool slot before `accept`
removes it. That only costs recall when a large share of the pool is expired;
moving expiry into SQL needs a canonical expiry column, which is not added here.

**Visibility over the bound.** The lexical arm's bm25 scores use term statistics
of the whole table, including rows the caller cannot read. Those statistics only
order the candidate pool, never the output: every returned row is visible, and
its final score is computed from the visible candidates as before. The residual
effect is that hidden rows can change which visible rows enter a pool of
`scanLimit / 10` when the pool is full. `TestLeakprobeMemoryRecallOverScanBound`
and `TestOverBoundRecallRespectsVisibility` cover the output; the statistical
effect is recorded here rather than removed.

**Cost.** The over-bound check is one `COUNT` capped at bound+1 rows. The vector
arm reads every visible embedding once per recall; that is the linear term that
remains, and it is cheaper than the full-row decode the window did.

## Agent memory: importance, use and eviction

A fact may carry an importance from 1 to 5 (`imp=N` in the trailer). Unrated
is the absence of the field, and it is never written. Importance is an
authored claim, so it lives in the file like every other one.

**Ranking.** At rank time an unrated fact counts as 3, or as 4 when a person
wrote it (`HumanAuthored`: `by=human`, no trailer, or an id that no longer
matches its text). The score is multiplied by `1 + 0.1 * (importance - 3)`,
bounded to 0.8 to 1.2, and the recency half-life is doubled for importance 4
and 5 and halved for 1 and 2. An unrated agent fact has factor exactly 1, so a
store with no ratings scores bit for bit as it did before the feature
(`TestUnratedRankingMatchesThePreImportanceBaseline`). A person's unrated fact
is the one case that changes an existing ordering, on purpose.

**Use.** A helpful vote through `POST /api/memory/feedback` is the use signal.
It increments `uses` and sets `last_used` on the index row. Those columns are
derived: they never reach the markdown, they survive note rewrites and
reindexing, and they are lost only when the fact or its note is removed. The
ranking bonus is `0.05 * (1 - exp(-uses/3))`, so it is small and saturating.
Recall does not itself verify that the fact was returned before the vote;
that is the caller's contract.

**Eviction.** `grimoire memory prune` (or `POST /api/memory/prune`) is a dry
run unless `--apply` is given. A fact is a candidate only if every one of these
holds: agent-written, not immutable, not human-authored, not challenged, never
voted helpful, never used, explicitly rated 1 to 3 (unrated is not a candidate,
and 4 and 5 are never candidates), and last written more than 90 days ago. The
index query and the file-side check (`pruneEligible`) state the same rules, and
the file check runs again immediately before each retraction. Apply goes
through the `forget` path, so the fact is struck through and appears in the
belief-change digest as retracted by `memory-prune`. Nothing is deleted.
## Agent memory: recall expansion and the entity graph

Two opt-in stages extend `recall` (`GET /api/memory`, the MCP `recall` tool and
`grimoire recall --expand --hops N`). Both are off by default, and with them off
the response is byte-identical to a plain recall. Neither calls a model; both
are built on the ordinary recall path so every access rule is applied in one
place (`index.MemoryEntries`).

**Expansion (`expand=true`, or `GRIMOIRE_RECALL_EXPAND=1` for default-on; an
explicit `expand` parameter always wins).** The literal query is searched with
up to three more phrasings, each deduplicated against the others:

* `entity`: the query's entities alone;
* `keyword`: the content words, with question words removed;
* `alias`: the query with each word that is a known alias rewritten to its full
  name, plus the suffix-stripped forms of its content words (`owns` also
  searches `own`).

Aliases follow the bank rule (`bank/entity.go`): a name resolves when exactly
one longer visible entity starts with it. A query word that is only a first
name resolves the same way (`memory.FirstNameAliases`). The alias map is drawn
from the caller's visible entries, so it cannot reveal a name they cannot see.

Each variant runs through `MemoryEntries` with a pool of twice the limit, and
the rankings are merged by reciprocal rank fusion with k=60. The fused score
is scaled into (0, 1], so a fact first under every variant scores 1. The raw
RRF sum is reported as `fused` under `explain`.

**Graph walk (`hops=1|2`, default 0).** The entities of the top five hits seed
a walk over the same visible entries. A fact that shares an entity with a seed
is hop 1; a fact that shares an entity with a hop-1 fact is hop 2. Each reached
fact scores `0.5^hop` times its source's score, carries `via: "graph"`, and
names the entity that linked it in `connect`. At most 20 are added, after the
direct hits. Graph-added facts pass the same filters as direct hits: the walk's
universe is built with the caller's own `Filter` and query filters, so the
reader list, private, superseded, expired and validity rules are not repeated.

**Explain.** Each expanded hit lists the `variants` that returned it, its
`hop` and `via`/`connect` if it was reached by the walk, and under `explain`
the raw fused score. The component scores come from the variant that ranked
the fact highest.

**Cost.** Expansion runs up to four ranked scans and four query embeddings per
recall, plus one unranked universe scan that is shared with the walk and the
alias map. `BenchmarkRecallExpand` (1,000 facts, median of seven runs) measured
about 31 ms for a plain recall and about 117 ms with expansion. The walk itself
was within noise of that.

**Not covered.** Bank recall (`POST /api/banks/{bank}/memories/recall`) has its
own retrieval path and does not take these parameters yet. Vector search does
not expand.

## Agent memory: the change stream and profiles

**One event source.** The belief-change digest (`GET /api/memory/changes`) and
the realtime stream (`GET /api/memory/stream`) both classify with
`beliefChanges` in `api/memory_changes.go`, so they cannot disagree about what
happened. There is no second log: the stream reads the index and remembers what
it has sent.

**Stream (`api/memory_stream.go`).** Server-Sent Events, authenticated like the
other writes (`userOnly`). Events are `memory.added`, `memory.superseded` (one
row per correction, carrying both texts), `memory.forgotten` (with `reason`
`retracted` or `expired`), `memory.challenged` (an agent's refused claim) and
`memory.disputed` (the fact being contested). Access is decided per event at
emit time: the query runs through the caller's filter and each event is checked
with `canRead` again. An event the caller may not read is dropped whole, since
its path would reveal that the note exists. `?agent=` and `?session=` narrow
the feed. A 25 s heartbeat comment keeps proxies open. The poll runs once a
second and each connection clears its write deadline, since the server's
five-minute `WriteTimeout` would otherwise cut it. `Server.Close` ends every
open stream, and the command calls it before `Shutdown`.

The SSE `id` is `<stamp>|<event>|<entry id>`. `Last-Event-ID` resumes from the
start of the minute it names, so nothing is skipped and a few events may repeat;
clients dedupe by id. Known limits: the stream reads state rather than a log, so
a fact added and superseded between two polls appears only as superseded, and
events older than the 2,000-fact scan limit are not replayed.

**Profiles (`api/memory_profile.go`).** `GET /api/memory/profile?subject=user|agent`
returns a compact portrait built from existing entries, with no model in the
default path. Live facts only: superseded, expired, disputed and pulled-from-outside
facts are excluded. Human-written facts come first, then importance, recency
and id. Recent changes from the last seven days go last. Output is markdown in
fixed sections, and every line ends with `[mem:ID]`, so any line can be recalled
and checked. The budget (`budget`, default 400 tokens) is counted by
`bank.CountTokens`.

`synthesize=true` asks the configured model to rewrite the same selection as
prose. The rewrite is kept only if every line cites an offered id and it fits
the budget. Otherwise the response carries `fallback` and the reason, and the
deterministic text in `deterministic` is always present. The memory cursor is a
digest of every field the changes feed reads, plus the times at which an answer
could go stale (an expiry, or a change leaving the window). Responses are cached
under it, so an unchanged memory answers without rebuilding.

## Agent memory: cascade forgetting and receipts

A plain forget (`DELETE /api/memory/entry`, `grimoire forget`) retracts one
bullet and leaves it struck through, which is the right default for a belief
that was merely wrong. It does not reach what was derived from the bullet. A
bank observation, a mental model, a cached profile, a dream report, or a
snapshot in history can all still carry the text.

`POST /api/memory/forget` with `cascade=true` (CLI `grimoire forget PATH ID
--cascade [--dry-run]`, MCP `forget` with `cascade`) reaches those copies. The
work is split across two packages.

**Bank layers (`bank/forget.go`, `CascadeForget`).** Under the bank lock it
walks facts, observations, models and proposals in dependency order:

- a fact that carries the text is removed. A person's fact is kept and reported
  when the forget is agent-initiated;
- an observation with no support left is retracted. One built partly from
  forgotten facts is removed, and its surviving facts are reset in
  `bank_consolidated` so the next consolidation rebuilds it. An observation is a
  paraphrase, so redacting its words would not make it true;
- a model whose basis is gone is deleted. A partly derived one has its body
  cleared, its `scope_sig` dropped and `needs_rederive: true` set, so the next
  refresh writes it again. Its `body_sum` is recomputed so an agent answer is not
  reclassified as human-edited;
- a proposal that carries the text or cites a removed item is dropped.

**Vault layer (`api/memory_cascade.go`).** The memory entries are handled as
follows:

- an agent's entry that carries the text is redacted in place, with its id
  re-derived from the new text. A redaction that still carries the text removes
  the entry. An entry whose id is not re-derived would read as a hand edit, and
  so be attributed to a person;
- a person's entry is never altered by an agent-initiated forget. It gets a
  challenge entry (`chal=`) that disputes it, and a repeat files no second one.
  A cascade whose target is a person's entry is refused for an agent. A
  human-initiated forget cascades fully;
- the target is removed outright, not struck through. A struck bullet keeps its
  text;
- entries superseded by the target are relinked to `retracted:cascade`, so
  nothing is brought back to belief by the removal;
- `Dreams/` reports have the forgotten lines replaced in place. Other notes a
  person wrote are never edited. They are reported as residual;
- snapshot history is rewritten line by line, so no version keeps the text.
  Snapshots are copies, not live entries, so this applies to every note;
- the `bank_vec_cache` table is dropped whenever something changed, since its
  keys are hashes of embedded text that cannot be matched back to the forgotten
  one; the profile memo is reset.

**What "carries" means.** The forgotten text matches as whole words, or as a
run of five words of it with at least three content words. A paraphrase is not
caught by the text rule. The vector check in verification finds one by meaning
and reports it as residual rather than removing it, because a meaning-level
match is a judgement and this path makes none about a person's notes. The
matcher refuses text under 12 characters or fewer than three words.

**Verification and receipts.** After the cascade, verification searches the
index again: memory entries, entity edges of removed ids, FTS over every note
(then line-level confirmation), bank units through `bank_units_fts`, bank source
chunks, snapshot history, and, when an embedder is configured, vector rows at
cosine 0.9 or more. Residual hits in notes the requester cannot read are counted
but not named. `status` is `clean` only when nothing is residual; the CLI exits
non-zero otherwise.

Each real run writes `Memory Receipts/forget-<id>-<ts>.md` and a `.json` copy
beside it (`GET /api/memory/receipts`, `grimoire memory receipts`). The receipt
names the target id and its note, a salted SHA-256 of the normalised text with
the salt that produced it, the word count, every artefact touched with its
action, and the verification. It does not contain the text. The salt is stored
too, so the hash can confirm a guess against that one receipt. Add the folder to
`.stignore` if the receipts should stay off a synced device. A dry run writes
nothing, not even a receipt. A repeat of a completed forget returns the existing
receipt with `already_forgotten: true` and changes nothing.

**Initiator.** An agent is any caller that presents an agent identity (the
`X-Grimoire-Agent` header, or the `agent` query). A caller with none, including
the CLI, is the person. This is the same trust `forget` already attributes
retractions by. The request body cannot claim either role.

**Not covered.** Source documents a bank retained (`bank_chunks`) are reported,
not edited, because they are what the person kept. Human-written facts in bank
files are kept and reported on an agent forget, with no challenge filed for
them. Notes outside `Agent Memory/`, `memory/` and `Dreams/` are residual, not
redacted. Anything outside the vault, such as Syncthing copies on a phone,
backups, restic snapshots and the git history of a vault, is not touched. The
`memory_changes` feed and recall caches are computed from the index and carry
no copy of their own, so they are covered by the verification rather than by
rewriting.
## Agent memory: basis, evidence and recall mode

**Basis is derived, never stored.** Every recalled fact, every profile line and
every `explain` row carries `basis`, computed by `memory.Entry.Basis()` from
fields the bullet already holds. Nothing is written for it, so there is no file
format change, an old vault gets a basis on first read, and a hand edit that
changes the claim changes the basis with it. The cases are applied top to
bottom and the first that matches decides:

| # | Condition on the fact | `basis` |
|---|---|---|
| 1 | origin starts `import:` | `imported` |
| 2 | origin is untrusted (`trust.FromOrigin`: any origin other than `self`, `user`, `me`, `local`, or empty), even when a human claim is present | `pulled` |
| 3 | a person asserted it: `by=human`, a bullet with no trailer, or an id that no longer matches its text (`HumanAuthored`) | `stated` |
| 4 | agent or task starts `consolidat`, `reflect` or `dream`, or category is `inference`, `synthesis` or `reflection` | `inferred` |
| 5 | an `ev=` evidence list is present, or category is `observation`, `observed`, `tool_result`, `transcript`, `document` or `log` | `observed` |
| 6 | anything else: an agent's bare assertion | `inferred` |

Two consequences follow from the order. A pulled origin cannot be made `stated`
by a claimed `human` flag, which is the same rule `AuthorityOf` applies to
authority. An origin such as `web:host` or `connector:x` therefore reads as
`pulled`, not `observed`, because trust already classes it as untrusted.

**Evidence.** `remember` takes `evidence` (note paths, urls or entry ids, at most
16, each up to 512 characters, no comma or line break). It is stored in the
trailer as `ev=a,b` and in the index column `memory_entries.evidence`. It does
not enter the id hash, so it cannot change reconciliation, and it is what moves
an agent's write from case 6 to case 5. Absent evidence on an agent write
leaves it `inferred`.

**Recall mode (`mode=factual|personal|all`).** The default `all` is the
unchanged query. `factual` removes facts whose category is in the personal set,
and `personal` keeps only those. The set is `preference`, `persona`, `style`
and `likes`, matched case-insensitively, and is replaced entirely by
`GRIMOIRE_PERSONAL_CATEGORIES` when that is set. The filter runs in SQL before
the scan bound, so it cannot shrink the top-N. A fact with no category is not
personal, so an uncategorised preference still reaches factual recall. The same
filter applies to `GET /api/memory/context` as `recall_mode=`, to the profile as
`exclude_personal=true`, and to the hook as `GRIMOIRE_RECALL_MODE`.

**Basis filter.** `basis=stated,observed` keeps only the listed bases. Unknown
names are a 400, not an empty answer.

**Known gaps.** Consolidation rewrites a whole note through the model. Bullets
it returns without their trailers read as hand-written, so a consolidated
agent fact can become `stated`. This is the same trailer-loss hazard that
already affects authority, and it is not fixed here. Portability exports do not
yet carry `ev=`, so an imported fact loses its evidence.

## Testing

* `go/internal/*/[_]test.go` — pure logic (renderer, queries, CRDT, crypto…)
  and the HTTP API against a fresh temp vault per test
* `compat/fixtures/` — frozen output from the original Python implementation,
  replayed by `go/internal/compat`. They are a historical contract: the
  generator is gone with the implementation that produced them, so a fixture
  that fails means the Go build changed, not that the fixture is stale
* `tests/e2e` — Playwright against the real binary; `page` fixture pins the
  classic editor, `live_page` runs CM6; shared fixtures in `tests/e2e/conftest.py`
* `.verify.yaml` — live smoke (isolated port 9119; never the production vault)

Lint: `gofmt -l go/`, `go vet ./...`, and `ruff check tests/ benchmarks/` must stay clean (config in
`pyproject.toml`).
