# Written procedures

A **procedure** is a multi-step task that worked and that someone will need to
repeat: a deploy, a release, an install, a migration, a recovery. A written
procedure is one the agent that did the work saves on purpose, with the checks
that make it reusable. It is a skill note in the memory store, so it is read by
`grimoire skills check` and published by `grimoire skills export` like any other
skill.

Use it for work that is done once and needed again. Not for an edit-and-test
loop, and not for an investigation that was a one-off.

## Fields

`POST /api/memory/procedure` (or the MCP tool `remember_procedure`, or
`grimoire skills write --file FILE.json`) takes:

| Field | Meaning |
|---|---|
| `name` | 3 to 80 characters. The skill's name; a second procedure with the same name replaces the first only if the route wrote it. |
| `when` | The situation it applies in, one sentence. This is the trigger. |
| `steps` | 2 to 20 steps, one line each, at most 400 characters. The commands as run, with changing parts written as `<slot>`. |
| `pre` | One or more anchors that must hold before starting. |
| `post` | One or more anchors that hold once the steps have worked. |
| `host`, `topic`, `agent`, `session` | Optional. `host` is the machine the checks are about. `session` is the provenance. |

An anchor is a probe object of the same kind `remember` takes (see
`MEMORY-ANCHORS.md`), for example `{"type":"unit_active","unit":"lectern.service"}`.
Probes are read-only and fixed; a procedure cannot hold a shell command.

`grimoire skills write` sends the procedure to the running server when one answers
at `GRIMOIRE_URL` (or loopback on `GRIMOIRE_PORT`, default 9111), and opens the
vault itself only when nothing answers. It prints which path it took.

## What the store does with it

1. Validates the whole request. Nothing runs and nothing is written when it fails.
2. Runs every `pre` anchor once. If one **fails**, nothing is stored and the
   response (HTTP 422) lists every check, so the writer can fix the one that
   failed. A check that cannot be made from this machine (its host is not in
   `GRIMOIRE_ANCHOR_HOSTS`) is **skipped**, and a skipped check does not block
   the write, as for ordinary memory writes.
3. Runs every `post` anchor that has no slot once. A failing `post` is reported
   but does not block the write: the procedure may have worked in a different
   state than the one it was written in.
4. Saves the note with `metadata.skill_source: written`. The trigger is stored as
   `metadata.skill_trigger` and as a cue. Each check is stored as an
   `anchor:` predicate, and the steps are the numbered list in the body.

Response: `stored`, `replaced`, `file`, `skill`, `checks` (one entry per anchor,
with its result), and `needs_values` (the `post` anchors that carry a slot).

## Slots

A `post` anchor may carry a slot in an argument, written `<name>`, for example
`{"type":"binary_version","bin":"grimoire","text":"<version>"}`. Its shape is
checked with a placeholder in the slot's place. It is **never run** at write
time or by `skills check`; it is reported as `needs values`. Slots are not
allowed in `pre`, because a precondition must be checkable as written.

## The security rule

Procedures are accepted only from a local caller that is not remote, the same
gate as anchors. The route has the same access class as `remember`: any signed-in
caller may write one, and an anonymous caller gets 401. It is not admin-only, so
a local agent whose MCP entry carries no admin token (Codex, or a session a
launcher starts) can save a procedure, as it can save an anchored memory. A
signed-in member on the loopback peer is trusted for procedures exactly as for
`remember`; a multi-user deployment that does not want members writing shared
skills should not give them a local shell on the server. A remote caller,
including one that arrives through the MCP HTTP transport (which always counts
as remote), is refused with 403 even when it is an administrator; this is a
refusal, not an ignored field.

Steps are refused if they look like a credential: a `password=`, `token:`,
`api_key=` or `Authorization:` followed by a value, an `X-…-Token` or `-Key`
header with a value, or anything the repository's secret scanner flags. The 400
names the step number and never echoes the matched text. The same check covers
the trigger. Keep secrets in the credential vault and refer to them by name.

A `remote` probe is not accepted in a procedure: it is never checked from here,
so it could not show that a precondition holds.

## Checking

`grimoire skills check [--cwd DIR] [--json]` lists the stored procedures and
whether each applies here. A procedure applies when every `pre` check passes
here. A check that fails, or is skipped, is listed as unmet by its predicate, so
the reason is visible. A procedure with a slot in `post` is listed with its
`needs_values`.

`GET /api/memory/anchors` (admin) reports a `procedures` block: `total` (procedure
notes in the store), `with_spec` (those with a spec), `pre_pass` (every `pre`
check passes now) and `pre_fail` (at least one fails now). A procedure whose
checks are all skipped is in neither count.

## Predicate form

A precondition or postcondition written as an anchor is `anchor:` followed by the
anchor's encoding (`go/internal/anchor`, `Encode`). The list separator `;` inside
the encoding is written as `%3b`, which the encoder never produces, so the spec's
lists still split correctly. Other predicates (`file:`, `command:`, `env:`,
`fact:`, `ran:`, `last-ok`) are unchanged.

## Relation to `skills export`

A written procedure is exported like any other procedure note: a folder with a
`SKILL.md` whose description is built from `when`, and a `scripts/steps.sh` when
the steps are shell blocks. The export does not carry the checks, so the exported
skill cannot tell an agent whether its preconditions hold. Use `skills check`
for that.

## Mining is kept, and it returns habits

`grimoire skills mine` is still there, and `skills accept` still writes mined
candidates. It is not the way to get procedures. On one owner's real sessions it
returned 20 candidates, all habits (`git diff -> git status -> git diff`). Two
better offline miners found at best 5 real procedures in 24 candidates from 801
sessions, all with steps missing. Asking the agent that did the work to write the
procedure, with typed checks, produced 12 recognisable procedures from 6 real
sessions. Mining command history does not recover procedures.

## Limits

* A check tests one concrete part of a procedure. Holding does not show that the
  steps will work again.
* A slot is recognised by the shape `<name>` in an argument. A literal such as
  `<html>` in a `file_contains` text is read the same way, so it is never run as
  a postcondition. Write such a text without the angle brackets, or accept that
  the check is not run.
* `skills write` saves into the server's memory store (`memory_canonical_dir`).
  It does not take `--dir`, because the server, not the CLI, writes the note.
* The checks run on the machine that runs the server, unless `host` names one in
  `GRIMOIRE_ANCHOR_HOSTS`.
