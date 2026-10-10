# Deny-probe leakage suite

`go/internal/api/leakprobe_test.go` tests one property end to end: a non-owner
cannot learn anything owned by someone else from any read surface, in any
shape. Other tests in the package check that a handler looks at the caller
(`route_audit_test.go`) and that the filtering is right for one feature
(`multiuser_test.go` and the feature tests). This suite asks the outside
question: does any byte, count or status reach a caller who should not have
it, through any route or MCP tool.

Run it:

```bash
cd go && env -u GRIMOIRE_SESSION go test ./internal/api/ -run 'Leakprobe|BankRoutesGive' -count=1 -v
```

## Fixture

`seedLeakWorld` builds an admin (`root`), a member who owns content (`alice`)
and a member who owns nothing (`bob`). Every owned item carries a canary
string (`LKC-<label>-<hex>`). Seeded as alice:

- notes in her personal space: plain, `private`, `publish`, `publish`+`private`
  (must never be served), a superseded version, a trashed note, a graph note
  whose wikilink target is a canary
- a `private` note in the shared commons (`team/alice-private.md`)
- agent memory (`agent`, `session`, `task`), a second and contradicting fact
- a bank in a space only alice writes: facts, a mental model, a directive,
  plus its observations, sessions, documents and webhook lists
- an imported document (an upload, untrusted origin)

Seeding asserts its own success and sanity-checks that alice can read her own
data, so no probe can pass against an empty fixture.

## Identities

Every probe runs as four identities: `bob`, `anon`, `bob+agent` (bob's key with
an `X-Grimoire-Agent` header, an agent with no grant of its own) and
`anon+agent`.

## Canary classes

- **Owner-only**: alice's personal notes, the bank, the graph note, the
  publish+private note. Must not appear in any response to any non-owner.
- **Commons-shared**: agent memory, the challenged and disputed facts, the
  imported document, the shared private note. The commons is readable by every
  account by design (`auth.PrincipalFor`), and `remember` writes to
  `memory/<topic>.md`, which is commons. Members may see these; anonymous
  callers may not.
- **Published**: the one note the operator published. Allowed only on the
  published routes, for any identity.

## What is asserted

1. **Explicit probes** (`lpProbes`): one entry per content surface and per
   write, with concrete targets (real ids taken from alice's listings).
2. **Coverage** (`TestLeakprobeRouteCoverage`): every registered route must have
   an explicit probe, an `lpExempt` entry with a reason, or be admin-class (and
   admin-class routes are driven by `TestLeakprobeAdminRoutesRefuseMembers`). A
   new route with none of these fails the build. Checked: injecting an
   unprobed route fails both this test and `TestEveryRouteIsClassified`.
3. **Canary check with echo accounting**: a canary may repeat what the request
   said (a question echoed in an answer is not a leak), but a body may not carry
   more copies than the request did.
4. **By-id reads** answer 401, 403 or 404 to a non-owner, never 200.
5. **Count fields** (`totals`, `count`, `degree`, facet, entity, tag counts,
   array lengths) for a canary query and a same-shaped shadow query must match.
6. **Differential counts** (`TestLeakprobeCountsDoNotDependOnHiddenOwnedItems`):
   a second world holds the same commons content without alice's owned items.
   Every list and by-id probe must return identical counts to bob and to anon in
   both worlds. This is the check that catches hidden matches the shadow control
   cannot.
7. **Existence oracle** (`TestLeakprobeNoExistenceOracle`): an existing hidden
   object and a missing one answer the same status to a non-owner.
8. **Generic sweep**: every registered route is also driven with concrete
   stand-ins, so an unlisted route still gets its canary check.
9. **MCP**: every tool the in-process server advertises is called as bob and as
   anonymous with arguments that name alice's objects. The MCP layer is an HTTP
   client of the API, so it authenticates as the key it is given.
10. **Integrity**: after the sweep, alice's data is unchanged (no marker from a
    write probe landed in her notes or bank).
11. **Liveness** (`TestLeakprobeFixtureIsLive`): each channel the probes rely on
    delivers content to its owner (SSE replay, export in every format, the
    publish allowance, MCP as bob reaching bob's note). A probe that reaches an
    empty channel would pass for the wrong reason.

## Findings

Fixed, each in its own commit with a regression test that fails before the fix.

| # | Severity | Surface | Finding | Commit |
|---|---|---|---|---|
| 1 | Medium | `GET /api/graph` | The `unresolved` list came from every unresolved wikilink in the vault with no reader filter. A `[[target]]` written in a private space was returned to anonymous callers and to members outside it. Link targets only, capped at 200, no note bodies. | `69e241f` |
| 2 | Low | `DELETE /api/trash/{tid}` | Purging a missing id answered 204, while an existing entry the caller could not write answered 404. A non-owner could test which trash ids existed. Trash ids are timestamps, so they can be guessed. Now a missing id answers 404, as restore does. | `9333e3c` |
| 3 | Low | bank sub-routes (`sessions`, `webhooks`, `entities`, `stats`) | An absent bank in the commons answered 200 with an empty list (or a different 404 body) while a hidden bank answered 404. The difference named which bank names are real. Bank reads now confirm existence and share one 404 answer. | `b5ec8b4` |

No leak of note body, memory text or bank content was found on any route. The
canary sweep, the MCP sweep and the SSE and export probes all held before and
after the three fixes; the fixes were found by the count and existence checks.

### Design findings (not changed; operator decision)

These are consistent with the access model as written and are not patched.
They conflict with the premise that agent memory and uploads are per-owner.

- **D1. Agent memory is commons-scoped.** `POST /api/memory` writes
  `memory/<topic>.md`, which is in the commons. Every account reads every
  agent's memory through recall, search, export, graph and the changes stream,
  and can supersede it. To make memory per-owner, give remember a personal
  destination (for example `users/<name>/memory/`) and teach the fact index to
  read it.
- **D2. Uploads and imports are commons-scoped with no owner.**
  `POST /api/documents/import` writes `documents/<name>.md` in the commons.
  Any member can read what another member uploaded.
- **D3. `private` is a retrieval flag, not an owner boundary.** A member of a
  space reads a `private` note by path, sees it in listings and in the graph,
  and can opt into it with `include_private` on ask and retrieve. The flag
  excludes it from automatic and agent context and from the e-ink read surface.
  If `private` is meant to be owner-only, that is a behaviour change to make on
  purpose, with the owner resolved from the note.

## Deferred and not covered

- **Timing side channels** are not measured. Wall-clock comparisons are too
  noisy for a unit suite. Count and ordering differences are covered by the
  differential test; a timing-oracle check needs a controlled benchmark.
- **Challenged and disputed entries** are seeded as plain commons memories. The
  suite does not yet create a real challenge object through
  `POST /api/memory/challenge`.
- **Untrusted origin.** The imported document is the untrusted-origin item. The
  `trusted=1` filter is ranking-side and is not a privacy control, so it is not
  probed as one.
- **Profile synthesis** (`synthesize=true`) runs its deterministic fallback, as
  no model is configured in tests. The model path is not exercised.
- **SSE** is read for a bounded window (400 ms). Events after that window are not
  seen, so the stream probe checks what is replayed, not what is pushed later.
