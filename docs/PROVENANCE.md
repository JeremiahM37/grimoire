# Provenance: verified evidence and weighed disputes

Two limits, one document. Limit #4: `basis` was derived, so an agent that cited
evidence it did not have was recorded as having observed. Limit #16: when a
person, a document and an agent disagreed, the store flagged the dispute and
weighed nothing.

This document says what is checked, what is only scored, and what is still
open. It makes no claim that a fact is true.

## What is verified

When an agent writes a fact with `evidence`, each reference is resolved
(`go/internal/attest`). The check is about existence, never truth.

| reference | resolves when |
|---|---|
| a vault path, `memory/x.md` | the note exists and the caller may read it |
| a path with a fragment, `memory/x.md#quoted text` | the fragment occurs in the note (whitespace-collapsed, case-sensitive), or a heading in the note slugs to it |
| `sha256:<64 hex>` | an imported document has that sha256 of its original bytes, and the caller may read it |
| `https://…` | the URL appears in the raw text of a note with an origin (a note the store pulled), and the caller may read that note |
| a 12-hex memory entry id | an entry with that id exists in a note the caller may read |

What a resolved reference does **not** show:

- that the agent read the source, or read it correctly;
- that a quoted passage supports the fact (a quote is found, its meaning is not checked);
- that a URL was fetched by a particular request (only that a pulled note contains it);
- anything about a reference the caller cannot read. Those report as absent, the
  same as `requireRead`, so a check cannot be used to learn that a restricted
  note exists.

### What the write path does

`memory.Entry.Verification` is recorded as `evv=verified` when every reference
resolves and `evv=unverified` when any does not. It is omitted when the fact
cites nothing, so bullets written before this change round-trip unchanged.

- Evidence is **kept** either way. A reference that does not resolve is still
  what the agent claimed, and dropping it would hide the claim from whoever reviews it.
- An unverified fact is **capped at `inferred`** (`Basis()`), whatever its
  category says. An `observation` category does not lift a forged citation.
- A human write is unaffected: it is `stated` before evidence is considered, so
  the cap cannot reach it.
- The verdict appears in recall (`verification`, on every entry that cites evidence), in the
  explain view, and in each disputes side.
- The check runs at write time only. Facts that were written before this change
  have no `evv` and keep the basis they had. Their citations are not re-checked.

The check is `attest.Checker.Verify`, a method rather than a package function,
because it needs the caller's access rules and the store. Each dependency is a
function field, and a nil field reports `not_checked`, which is unverified, so an
unconfigured checker never looks as if it had checked.

## What is scored, not decided

`go/internal/conflict` scores the sides of a dispute. Each side gets five terms,
and the breakdown is returned with the total so each term can be disagreed with.

| term | weight | value |
|---|---|---|
| authority | 0.35 | human 1.0, agent 0.5, pulled 0.0 |
| verification | 0.25 | verified 1.0, no evidence 0.4, unverified 0.0 |
| corroboration | 0.20 | independent origins asserting the same text: one extra origin 0.5, two or more 1.0 |
| recency | 0.05 | `1/(1+age_days/365)` from the fact's stamp; 0 without a clock or with an unparsable stamp |
| reliability | 0.15 | `ReliabilityFn(agent)`, clamped to [0,1]; 0.5 when none is supplied |

Two rules are not weights:

- **A human side ranks above every non-human side whatever its total.** The
  lattice says a person's fact outranks an agent's; a score that could overturn
  that would be arithmetic overruling the authority rule.
- Ties are broken by newer `ValidFrom`, then by id, so the ranking is repeatable.

`GET /api/memory/disputes` returns, for each dispute, `suggestion`
(`{id, side}`, where side is `disputed` or `challenger`) and `breakdown`
(every side, best first, each with its terms). Corroboration is judged inside
the dispute: sides are corroborated only by other sides asserting the same text
from a different origin.

**Nothing auto-resolves.** The resolve route is unchanged: a person only, and a
resolution still writes a human-authority entry. A test asserts that reading
the suggestion writes no belief change and leaves the dispute open.

## Tests

- `go/internal/attest/attest_test.go`: real references resolve; forged ones
  fail with a reason; one forged reference makes the set unverified; a missing
  resolver is `not_checked`; an unreadable note reads as absent; heading slugs.
- `go/internal/memory/verification_test.go`: `evv` round-trips byte-identically;
  absent when unset; unknown values are ignored; unverified caps at `inferred`
  even under an observation category; human writes are unaffected.
- `go/internal/conflict/conflict_test.go`: forged evidence scores below verified,
  and the gap is the verification term alone; a corroborated pulled claim beats
  a lone unverified agent claim; a human fact never loses to a higher raw score;
  breakdowns sum to the total; reliability defaults and clamps; recency needs a
  clock and does not reward the future; ranking is deterministic.
- `go/internal/api/memory_attest_test.go` and
  `memory_disputes_suggest_test.go`: the same rules over HTTP, including a real
  imported document hash, a pulled URL, an entry id, a human fact with forged
  evidence, the explain view, and the suggestion not resolving anything.
- `memory_basis_test.go` was changed: the observed-evidence test now writes the
  note it cites. It previously cited a note that did not exist, which is exactly
  the forgery this change refuses.

## Limits and open items

- **Weights are judgement, not fitted.** Nothing measured them against resolved
  disputes. The two-versus-one ordering is a constructed case, and the margin in
  it is small (about 0.025). Treat the ordering as a reasoned default until
  adjudications exist to fit against.
- **No reliability source exists.** Every agent scores 0.5, so the reliability
  term does not discriminate yet.
- **Connector items are not hash-verified.** Their stored hash is a truncated
  digest over a composite of fields plus a trust suffix, not a content hash, so
  matching it would look broader than it is. Connector content can still be cited
  by its pulled URL.
- **URL checks read notes.** The check scans up to 5,000 pulled notes' raw text
  per URL reference, on the write path. A larger vault reports older URLs as not
  fetched rather than scanning further.
- **Entry-id detection is by shape.** A 12-character lowercase hex token is read as
  an entry id. A note path of exactly that shape would be misread.
- **Old citations are not re-checked** (see above), so a pre-existing forged
  `ev=` still reads as observed. A reindex does not change that; the verdict is
  written at write time.
- The `verification` field is new and additive. Any client that reads `basis`
  alone gets the capped value without needing to change.
