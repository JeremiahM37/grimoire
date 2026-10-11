# Lab: `prospect` (prospective memory)

Flag: `GRIMOIRE_EXP=prospect`. Off by default. With the flag off, every
existing answer is unchanged: the context and recall endpoints, the remember
tool's schema and its argument list, the CLI's flags and the route table
(the list route answers `"enabled": false`; the ack route answers 404).

## What it does

Memory answers "what is true about X". An intention is "when Y is about to
happen, remind me of Z". An intention is an ordinary fact with a **cue**: a
conjunction of typed predicates, evaluated against the situation of a context
or recall request.

Cue language (`internal/prospect`), terms joined by `&` (AND):

| term | holds when |
|---|---|
| `path:<glob>` | a file being read or edited matches; a glob also matches at any path-component boundary (`deploy/*` matches `/home/me/repo/deploy/a.yml`); `deploy/**` matches any depth |
| `cmd:<prefix>` | the shell command equals the prefix or starts with it followed by a space |
| `project:<name>` | the request's project name, case-insensitively |
| `after:<date>` / `before:<date>` | today (UTC) is on or after / before the day |
| `term:<word>` | the prompt or action contains that whole word, case-insensitively |

Lifecycle: `standing` (default), `once` (retires after a delivered firing is
acknowledged), `until:<date>` (fires through that UTC day, inclusive).

A cue is validated when written. Unknown kinds, malformed dates, `..` in a path,
`<`, `>`, `\`, control characters, more than 4 terms, or longer than 300
characters are refused with 400. A stored cue that does not parse never fires.

Where it is enforced:

- **Storage.** Three trailer fields, `cue=`, `life=`, `ret=`, written last so a
  bullet without them formats byte-identically (`memory/entry.go`). Round-trip
  tests in `memory/intent_trailer_test.go`.
- **Write** (`POST /api/memory`, MCP `remember`, CLI `remember --cue --life`).
  Refused from an untrusted origin (the same rule as `fresh` checks), with a
  non-normal visibility (it could never fire), or without the flag. An
  intention skips model fact extraction, so one cue means one fact.
- **Ordinary recall** (`/api/memory/context` both branches, `/api/memory`).
  A cued fact is removed from ordinary results, whether or not its cue holds.
  It comes back only when its cue holds for that request.
- **Firing.** Due intentions come first, as a `Reminder you set (cue ..., set
  <stamp> by <agent> in <note>): ...` line, at most 3 per request. Each delivery
  is appended to `.grimoire/prospect.jsonl`. A firing that cannot be logged is
  not delivered.
- **Refusals at evaluation.** Superseded, retired, expired, private, sensitive,
  sealed, untrusted-origin, or non-parsing entries never fire, whatever their
  cue says.
- **Acknowledgement.** `POST /api/memory/intention/ack {path,id}` retires a
  `once` intention, but only when a firing of it was logged earlier. It needs
  write access to the note. Any other intention is left as it is.
- **Listing.** `GET /api/memory/intentions` returns the caller's readable,
  non-hidden intentions with status `waiting`, `retired` or `expired`.
- **Action stage.** For the hook's PreToolUse stage the query is
  `<Tool> <target>`. `Bash` gives `cmd:` and `Edit`/`Write`/`MultiEdit`/
  `NotebookEdit` give `path:`, so these cues work with the hook as it is. Explicit
  `intent_path`, `intent_cmd` and `intent_project` parameters also work.

Skillgate's predicate vocabulary (`file:`, `command:`, `env:`, `fact:`) was
**not** reused. Those test the machine. Intentions test the moment. The parsing
discipline is the same: an unknown predicate is unmet, never true.

## Measurement

Virtual-week fixture: `internal/api/prospect_week_test.go`. It uses the real
HTTP handlers on a temporary vault. Twelve intentions are planted, and 61
scripted context loads are replayed at pinned times with explicit situations.
Each load states by hand which intentions must fire. Three acknowledgement steps
sit between loads. Precision and recall are over (load, intention) pairs.

| | value |
|---|---|
| loads | 61 |
| intentions planted | 12 (standing, once, until) |
| (load, intention) pairs | 732 |
| expected firings | 44 |
| true positives / false positives / false negatives | 44 / 0 / 0 |
| precision / recall | 1.000 / 1.000 |
| stability | identical over 3 repeated runs |
| most reminders in one load | 2 (cap is 3, tested separately with 4 candidates) |

Repeating the fixture with the gate disabled (`if false && cued[...]`) fails it at
load 2: `A3 came back as ordinary recall`. The fixture therefore detects a
gate leak. Log: `/mnt/bulk/lab-work/prospect/mutation.log`. The original was
restored and checked byte-for-byte afterwards.

Other checks (all run from `go/`, `env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP`):

- Unit tests (`internal/prospect/prospect_test.go`): parse and refusal,
  canonical round trip, path globs at boundaries, term word boundaries,
  command prefix boundaries, project and date edges, lifecycle, every
  non-firing case in `Fires`, the cap ordering and count, the origin refusal.
- API tests (`internal/api/prospect_test.go`): flag-off refusal, write
  validation, held from ordinary context, action-stage firing without hook
  changes, once-intention retirement after an acknowledged firing, log lines,
  the cap of 3 with its log count, recall gating, and a flag-on/flag-off
  equivalence check on a store with no intentions (identical bytes for ordinary
  context and recall).
- Leak probes: two new routes registered (`leakprobe_test.go`) and classified
  (`route_audit_test.go`).
- Full `internal/api` package with the flag off: pass.

No latency or ranking numbers were taken. The ranking benchmark does not apply
to this idea, as the work package states.

## Verdict

**KEEP BEHIND FLAG.**

The mechanism works on the fixture. It does not yet earn GRADUATE, for these
reasons:

1. The fixture was written by the same author as the code. Precision and recall
   of 1.0 show agreement with the author's reading of the spec. They do not show
   that the cue language fits real use.
2. No real hook traffic was measured. The Claude Code hook does not send
   `intent_project` (the hook has `cwd` but does not pass it to the context
   request) or `intent_path` at the prompt stage. `project:` cues therefore never
   fire from the hook as it is. Acknowledgement is not sent by the hook either.
   Both are changes to `~/.local/share/grimoire/hooks/grimoire_context.py`, which
   is outside this worktree and was not touched.
3. The cost is unmeasured. While the flag is on, each context or recall request
   reads every memory note that has a cued fact. This is a full scan with no
   cache: O(memory notes) file reads per request.

## Limits and open items

- **Not gated:** the `expand`/`hops` path of `/api/memory` (`recallExpanded`).
  A cued fact can still appear there. Fixing it means applying the same filter
  in that function.
- **Acknowledgement is not proof of reading.** It is a POST after a logged firing.
  Nothing checks that the agent used the reminder.
- **Superseding a cued fact drops the cue,** because the replacement is written
  with its own cue (normally none).
- **Two different cue fields.** `remember` already has `cues` (retrieval
  situations, a model-generated hint list). The new `cue` (singular) is a firing
  trigger. The MCP schema says so, but the names are close.
- **Matching is literal.** `term:` is whole-word, not stemmed. `path:` globs match at
  any component boundary, so a glob such as `deploy/*` also matches every
  `deploy` directory in a monorepo.
- **After/before** are evaluated in UTC. Stamps are written in local time.
- **Listing and cancelling.** Listing is `GET /api/memory/intentions` only: there
  is no CLI list and no MCP list. Cancelling is the existing `forget`.
- **Firings in recall** are logged and returned as ordinary hits, with no reminder
  marker.
- **Firing log** is append-only under `.grimoire/prospect.jsonl`. It is never
  compacted or rotated.
- The hook changes listed under verdict item 2 are the next step before any
  real-traffic measurement.

## Reproduce

```
cd go
env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP go test ./internal/api -run VirtualWeek -count=1 -v
env -u GRIMOIRE_SESSION -u GRIMOIRE_EXP go test ./internal/prospect ./internal/memory -count=1
```
