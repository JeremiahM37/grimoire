# Memory anchors

A remembered fact that says how something *is now* can go false without anyone
writing to the memory. An **anchor** is a typed, read-only check attached to such
a fact. It names the thing the fact is about (a file and a literal in it, a unit,
a port, a commit), so the store can ask the system whether the claim still holds.

## Kinds of fact

- **event**: something that happened and stays true as history. No anchors.
- **preference**: something the user wants, or a rule for agents. No anchors.
- **claim**: a statement about how something is now. It should carry anchors for
  the specific thing it is about. A claim that cannot be anchored says why.

## Probe types

Each probe is read-only and fixed. There is no shell and no arbitrary command.

| Type | Arguments | Holds when |
|---|---|---|
| `file_exists` / `dir_exists` | `path` | a regular file / a directory is there |
| `path_absent` | `path` | nothing is at the path |
| `symlink_to` | `path`, `target` | the link resolves to the target |
| `file_contains` | `path`, `text` | the literal appears in the file |
| `unit_active` / `unit_inactive` | `unit` | the systemd unit is / is not active |
| `unit_enabled` | `unit` | the unit is enabled |
| `port_listening` / `port_closed` | `port` | a TCP port is / is not bound |
| `http_status` | `url`, `status` | a GET returns that status |
| `http_contains` | `url`, `text` | a GET returns 2xx and the body has the text |
| `binary_version` | `bin`, `text` | `grimoire` or `lectern` version output has the text |
| `git_commit` | `repo`, `ref` | the commit or tag exists in that local repository |
| `git_branch` | `repo`, `branch` | the local branch exists |
| `remote` | `what` | not checked from here; recorded and reported as skipped |

Paths must be absolute. Units match `^[\w@.\-:]+$`. HTTP probes use a literal
loopback or `100.64.0.0/10` address and never follow redirects. `file_contains`
refuses secret and key paths, `.env` files, and the vault's index directory, and
reads at most 5 MB.

## Host

A claim can be about a machine other than the one the store runs on. Facts
record that as `host` (the `host=` trailer; a machine name of letters, digits,
dots and dashes). A claim written locally, with no `host`, is stamped with this
server's short name. Events, preferences, rules and writes from a remote caller
are stored with no host unless the writer gives one.

Every probe may carry a `host` argument. An anchor with no host runs here. An
anchor that names this machine runs here. Anything else runs over ssh, but only
if the host is in `GRIMOIRE_ANCHOR_HOSTS`, a comma-separated list of
`name=sshtarget`, for example `mediaserver=root@100.64.0.10`. The list is
empty by default, and nothing else is ever contacted. A bad entry stops the
server from starting.

An anchor that names no host takes its fact's host when the fact is about
another machine, so a claim about mediaserver is checked on mediaserver.

What runs remotely: the file, directory, symlink and literal probes, the unit
probes (system manager only), port probes, git commit and branch, and the
reference types. `binary_version` is skipped for another host. The `http_*`
probes always run from here, whatever host is named. `remote` is still never
checked.

Remote probes are fixed command lines run over `ssh` with `BatchMode`, a 5 s
connect timeout and a 15 s limit. Every value used on the other machine must
match `^[A-Za-z0-9_./@+:=,\-]+$` (no spaces, quotes, `$`, backticks, `;`, `~`
or globs). A value that does not match is skipped, never quoted and sent. A
`file_contains` path on the secret denylist is refused. An ssh failure to
connect is an error, not a verdict on the claim.

## Kept contradictions

When an update's old fact still passes its anchors, arbitration keeps it beside
the new one, and the response lists it under `kept`. The response's `advice` then
says which stored fact still holds (by id) and asks the writer to check which is
right, and to anchor the new claim if it is. The write is not blocked.

## Automatic anchors

A claim that arrives with no anchors gets one for each path or unit its text
names, if that thing exists now. At most four are kept, each marked `auto=1`.
Nothing is stored when none exist. These are weaker than writer anchors: they
say a named file or unit is there, not what the claim says about it. The report
counts claims whose anchors are all automatic apart from the others. Turn this
off with `GRIMOIRE_ANCHOR_AUTO=0`.

## Writer advice

A write's response may carry `advice`, a list of short strings. It never blocks
or changes the write. It says when a claim names nothing checkable, and when a
claim reads like something that happened (a sentence that opens with a past-tense
report, or contains a date), so the writer can set `event`.

## Annotating stored facts

`POST /api/memory/annotate` (admin, local callers only) changes the trailer of
stored facts: `event`, `host`, and `anchors`. It never changes the text, the id
or the stamp; only the line's trailer changes. Existing anchors are replaced only
when at least one new one passes. Hand-written facts, whose id no longer matches
their text, are refused. `grimoire memory annotate --file FILE.json` is a dry run
unless `--apply` is given.

Items are applied in order, and each note the request touches is written and
indexed once, so 177 items over 10 notes make 10 note writes. The answer's
`notes_written` counts them. `grimoire memory annotate` sends the request to the
running server when one answers (`GRIMOIRE_URL`, else loopback on
`GRIMOIRE_PORT`); it opens the vault itself only when nothing answers, and it
prints which path it took. Keep the live server the only writer while it runs.
A second process writing the same note can still hit `UNIQUE constraint failed:
notes.path` in the watcher's upsert, because the index's note rewrite is not one
transaction across processes (see the report for the cause).

## Security rule

Anchors are accepted only from a loopback peer that is not a remote or OAuth
caller. A probe result leaks a little about files on the server, so a remote
caller's anchors are ignored and the response says `"anchors_ignored":"remote caller"`.
The MCP HTTP transport always counts as remote; only the local stdio agent can
attach anchors.

## Written procedures

A multi-step task that worked can be saved with its checks: `pre` anchors that
must hold before the steps, `post` anchors for after. See
`docs/MEMORY-PROCEDURES.md`. A procedure's anchors are checked when it is
written, and again whenever `grimoire skills check` asks whether it applies.

## Writing

`remember` takes `anchors` (a list of probe objects), `event` (a boolean) and
`host` (the machine the claim is about, when it is not this one). Each anchor is
validated and run once. Only anchors that pass, or that were skipped (not checked
from here), are stored. The response lists every anchor with its result, so a
writer can fix one that failed. A failing anchor does not fail the write. The
response may also carry `anchors_auto` and `advice` (see above).

## Commands

- `grimoire memory anchors [--json]` runs every stored anchor and lists the
  claims whose probe no longer holds. It counts claims with a writer's anchor
  and claims with only automatic ones, groups claims by host, and counts skipped
  anchors by reason (`remote`, `host_not_allowlisted`, `unsafe_argument`,
  `other`). Admin only (`GET /api/memory/anchors`).
- `grimoire memory annotate --file FILE.json [--apply] [--json]` changes the
  trailers of stored facts (see *Annotating stored facts*). Admin only.
- `grimoire memory refs [--json]` scans every note for absolute paths under `~`,
  `/home/admin`, `/mnt/bulk`, `/etc`, `/opt`, `/usr/local`, `/var/lib`, `/var/log`
  and `/srv` (a unit named inside a path, such as `grimoire.service` in
  `/etc/systemd/system/grimoire.service.d/release.conf`, is part of the path and
  is not reported as a unit), and for `*.service` and `*.timer` names, and
  lists the references that no longer exist. Admin only (`GET /api/memory/refs`).

Neither command prints fact text.

## What recall does with a fact's checks

By default, an explicit recall runs the checks a fact carries when they all run
on this machine. A fact with a failing check gets `freshness.action = "verify"`,
with a reason naming the check. A fact whose own checks all hold gets
`action = "use"`. Each verdict is cached for 10 minutes. `GRIMOIRE_ANCHOR_RECALL=0`
turns this off, and recall output is then the same as for a fact with no checks.

A failed check is a fact about the system, not a guess, which is why it does
not sit behind a flag. Three things still do, behind the experiment flag
`anchor` (`GRIMOIRE_EXP=anchor`): running checks on another machine during a
recall, the staleness floor for unanchored wording, and action cues from anchor
targets. Automatic injection through the agent hook does not use check
verdicts in either mode.

Code: `go/internal/anchor` (probes, encoding, reference scan), `go/internal/api/memory_anchors.go`.

## What was measured

All on one person's live vault and real sessions, on 2026-10-11. Small samples;
read them as direction, not rates.

* **Before anchors.** Of 346 live facts, 17 (5%) could be given a typed probe
  after the fact. 48% were history, 38% were current claims that never named
  the thing they were about. Of the 17 probed, 3 were wrong at the time,
  including a deployed-version fact the freshness model rated 3.8% likely stale.
* **With the write contract.** Six real sessions were summarised by a small
  model under the contract in the `remember` tool description. Of 71 claims, 70
  carried an anchor. Replayed through this server: 94% of stored claims
  anchored, 63 probes passing, 21 naming another machine (not checked from
  here), none failing. A hand read of 14 found the anchors specific (the
  defining line of a file, a commit), not a nearby directory.
* **Anchors cannot be added afterwards.** Scanning 219 prose memory files for
  paths and units that no longer exist found 108, of which a reader classed 5
  as memory that is wrong now. 66 were on another machine, 23 were already
  described as past, 13 existed after all. That is why `grimoire memory refs`
  is a list of pointers to look at, not a list of errors: the reference alone
  does not say which host it is on or whether the text means now.

## Updates and anchors

An update is a new write that would replace an older fact. Before that happens,
the store runs the older fact's stored anchors (the same cached probes recall
uses, on the fact's own host). **On by default** (`GRIMOIRE_ANCHOR_ARBITRATE=0`
turns it off).

- If at least one writer-supplied anchor ran and none failed, the older fact is
  **kept**. The new fact is stored beside it as an addition, unmarked, and the
  response gains `kept: [{id, reason: "its anchors still pass"}]`.
- If any anchor fails, the update goes ahead as it always has.
- If every anchor was skipped or could not be checked, the update goes ahead as
  it always has: no information is not evidence.
- An anchor that the store derived itself (`auto=1`) does not protect a fact.
  Those say a file exists, not that the claim is right.
- A correction from a person is never blocked by this, whether it names its
  target or is inferred. A write whose origin is untrusted cannot claim to be a
  person here either. Corrections from agents are arbitrated like any other.
- Only replacements are arbitrated. A write that retracts a fact (a negation)
  is not.

## History

A fact that describes something that happened, rather than a current state, is
marked as an event. **Marking is on by default** (`GRIMOIRE_EVENT_AUTO=0` turns
it off). It applies on the write side only.

- When a write is split into several facts, or its `category` is `handoff` or
  `checkpoint`, each resulting fact that reads as history is marked. "Reads as
  history" is the same narrow past-tense test the writer advice uses: a sentence
  that opens with a reported action (Fixed, Deployed, Verified, ...) or that names
  a date.
- The caller's own `event` is always respected. Sending `event:false` stops the
  store from marking anything.
- A fact that has an anchor, from the writer or derived by the store, is never
  marked. It is a claim with a check.
- The response lists the marked facts as `events_auto: [ids]`. When the store
  marked at least one fact, the advice to set `event` is dropped from that
  response.

**The `events` flag (default off)** decides what happens to marked facts at
injection. With `GRIMOIRE_EXP=events`, facts with `event=true` are left out of
automatic injection (the hook and context path). They stay in explicit recall,
search, lists and briefings. With the flag off, marking a fact changes nothing
about what is injected.

Explicit recall shows `"event": true` on an event whatever the flag says. Facts
that are not events carry no `event` key, so their output is unchanged.

## Result history

Every probe the store runs is also kept, in `.grimoire/anchors.db` beside the
rules and adherence databases. The verdict is what the store already acted on.
The history adds two things it could not see before.

**What is stored: a hash of what the probe saw, never the content.** Each row
holds the fact id, a stable key for the probe (a hash of its type and arguments),
the verdict (`ok`), a short reason, a timestamp, the caller (`origin`), and the
`observed` value: the first 16 bytes of sha256, in hex.

* `file_exists` and `file_contains` hash the file's bytes, reading at most 1 MiB.
* `unit_active`, `unit_inactive` and `unit_enabled` hash the state string
  systemd printed.
* `port_listening` and `port_closed` carry membership only: whether something is
  bound to the port, not which process.
* `http_status` and `http_contains` hash the response body (at most 200 KiB,
  the body the probe already reads).
* `binary_version` hashes the version output. `git_commit` and `git_branch` hash
  the object the ref resolves to.
* `dir_exists`, `path_absent`, `symlink_to` and remote probes have no observed
  value, so they can flip but never drift.

A denied path (secrets, keys, the sealing store, the vault index) is never hashed,
even for `file_exists`: a digest of a low-entropy secret can be checked against
guesses. Probes that error or are skipped are not recorded at all, because
neither is a claim about the world.

**Why.** Two questions the verdict alone cannot answer:

* *How long does a claim of this kind survive?* A pass that later becomes a fail
  is a survival label. Over many facts these give real staleness lifetimes to set
  the floor against, in place of a guessed number.
* *Is the check still passing, but about something that changed?* A literal can
  survive a rewrite of the file around it. The verdict stays `pass`; the observed
  value does not. That is drift.

**The three states**, from `anchorlog.State(prev, cur)` and
`LatestState(fact, anchor)`:

| state | meaning |
|---|---|
| `live` | the probe passes and nothing about what it saw has changed |
| `dead` | the probe fails now, whatever it said before |
| `drifted` | the probe passes, but the observed value differs from the one before (both passing, both observed) |
| `new` | the first recorded observation, and it passes |

A dead anchor is dead even when it was never seen to pass. A pass with no observed
value on either side never drifts. A pass that follows a fail is `live`, not
drifted: the revival is a transition, reported as one (see below).

**Bounds.** At most 200 rows are kept per (fact, anchor). The oldest are pruned on
every write. Rows are append-only (pruning is the only delete), and nothing is read back
into a verdict: the history is written after the probe answers, and a write
error is logged and otherwise ignored, so an agent never sees a difference.

**Who writes a row** (the `origin`):

| origin | caller |
|---|---|
| `write` | `remember` (every fact a write stored, with the probes it ran), and `POST /api/memory/procedure` pre and post checks |
| `recall` | a fresh probe of a stored fact during recall (cached probes are not re-recorded) |
| `annotate` | `grimoire memory annotate` |
| `cli` | the admin report behind `grimoire memory anchors`, and the procedure tally it includes |
| `dream` | reserved. No dream code runs anchors today |

Not recorded: the dead-reference scan (`grimoire memory refs`), which tests
whether a note's reference exists and is not a claim about a fact; and skill
gating predicates, which have no fact id to key on.

**Reading it.** `grimoire memory anchors history` lists the changes, pass to fail,
fail to pass and drifted, oldest last, then the most recent results.
`--fact ID` limits it to one fact; `--json` prints the server's report. The same
data is at `GET /api/memory/anchors/history?fact=ID&limit=N` (admin only), which
returns the rows and the transitions computed from them.

**Limits.** A body that carries a timestamp or a token drifts on every probe, so
`http_contains` and `http_status` drift is only meaningful for pages that are
otherwise static. The history starts when this code runs; a survival label needs
weeks of observations before it says anything.

## Staleness floor, where, and action cues

These sit behind the **`anchor` flag (default off)**. With the flag off, recall
output is unchanged.

- **Staleness floor.** A claim (not an event, and not a preference or rule) that
  has no writer-supplied anchor, and whose wording goes stale, is asked to be
  checked: `freshness.action = "verify"` with the reason `volatile wording and no
  anchor`. "Wording that goes stale" is the store's volatile shape (addresses,
  ports, "currently", "as of", and so on) plus a three-part version number, a state
  word after "is" or "are" (deployed, running, live, installed, active, enabled,
  listening, pinned), the word worktree, or a path under `/tmp/`. A fact that
  already gets `verify` keeps its own reason. A floor is a prompt to check, not a
  verdict. The regex is `memory.FloorShape` in `go/internal/memory/fresh.go`.
- **Automatic anchors.** A passing automatic anchor no longer forces `use`. The
  model's verdict stands, so a fact with only automatic anchors can still be
  checked by the floor. A failing one still sends the fact to `verify`, and the
  reason names what is missing (the path or unit). Probe literals are never named.
- **Where.** A fact about another machine gets `freshness.where = "<host>"`, so
  the agent knows the claim is not about the box it runs on. Local claims get no
  `where`.
- **Action cues.** A fact's anchored files and units are also stored as action
  cues for that fact (at most six). When an agent's action names one of those
  paths or units, the fact is offered to injection even if its text never spells
  the path. This uses the same match, and the same specificity limit, as cues
  the writer supplies: a path that many facts name fires for none of them.
  Anchors that name another machine give no cue. Fingerprints
  (`go/internal/fingerprint`) are not used for this: they are an offline measure
  of whether a memory was used (`cmd/fpdump`), and the server's action stage
  matches cues.

The context path (the hook's injection) does not apply the floor or `where`. It
keeps its own verdict, as it did before this flag.


## Write budget

Fact extraction and the reconcile decision are the two model calls a write makes.
They share one deadline, `GRIMOIRE_WRITE_MODEL_BUDGET` (default `20s`; `0` means no
limit, as it was before). The default is on. When the deadline passes during a
call, that call falls back to the rules it always had, the fact is stored, and the
response says `model_skipped: "budget"`. A value that does not parse gets the
default, so a typo cannot remove the bound. The count of cut-short calls is the
Prometheus counter `grimoire_memory_write_model_budget_total`, labelled by stage.

## Limits

* A probe checks one concrete part of a claim. It holding does not prove the
  whole sentence.
* A claim about another machine is checked there only when that machine is in
`GRIMOIRE_ANCHOR_HOSTS`. Anything else is skipped and reported as such.
* An `anc=` trailer typed into a memory note by hand is probed like any other
  when the `anchor` flag is on. Anyone who can edit notes can therefore learn
  whether a path exists or a file contains a string on the server. Keep the
  flag off where people you do not trust can edit the vault.
* With the flag on, recall runs probes for facts it has not checked in the last
  ten minutes, which adds their run time to that call.
