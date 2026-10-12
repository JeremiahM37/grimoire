# Writing memories worth reading

Agents write most of Grimoire's memory, and a store is only as good as what
goes in. These are the rules `remember` asks agents to follow. They are
checked gently: `remember` still writes, and returns `warnings` an agent can
act on; the dream lists memories that break them.

## The rules

1. **One fact per memory.** Each memory is recalled, corrected, superseded and
   expired on its own. A paragraph of five facts is corrected as a whole or not
   at all. A list of steps is the exception: that is one procedure.
2. **Say the kind.** Pass `kind`:
   - `rule`: a standing instruction that applies whenever its situation comes up.
   - `procedure`: how to do X, as steps. Checked occasionally (see MEMORY_STORE.md).
   - `preference`: how the user likes things done.
   - `fact`: the state of the world. Freshness applies (FRESHNESS.md).
   - `reference`: a pointer to something outside memory: a doc, dashboard, repo.
3. **A rule says Why and How to apply.** "Never force-push" does not help an
   agent at the edge of the rule. "Never force-push main: it rewrote shared
   history once. Apply when pushing anything to a shared branch" does.
4. **Give the situation it applies in.** Pass `context` (and `cues` if you have
   them): what the agent was asked to do when it learned this. Memory is found
   by situation, not by topic.
5. **No secrets.** Store the credential in the vault and write where to find it.
   Memory notes sync to phones and into context.
6. **Nothing the repo or git already records.** Code layout, commit ids and who
   changed what are one `git log` away. Memory is for what the repo cannot say:
   why, what was tried, what the user wants.
7. **Update, do not add.** If a memory exists, correct it (target_id) instead of
   writing a near-duplicate. Reconciliation catches exact repeats, not rewordings.

## What the warnings mean

| code | meaning |
|---|---|
| `multiple_facts` | three or more bullets, or a long multi-sentence text |
| `rule_missing_why` / `rule_missing_how` | a rule with no reason, or no condition |
| `looks_like_secret` | a credential-shaped value (the security sweep also finds it) |
| `repo_info` | commit ids, or a description of code layout |

The dream reports the same checks over the whole store as `memory_quality`
findings (Info, capped at 30 per run). Nothing is rewritten or removed: the fix
is a better `remember`, or editing the note.

## What a write does when the slot holds more than one fact

This is current behaviour, not the intended rule. It is an open finding (F4 in
`docs/lab/oracle.md`), and no code changed for it.

The intended rule is in `ARCHITECTURE.md` and `authority.go`: a write supersedes
every live fact in its slot at its own rung or below, and is recorded alongside
any live fact that outranks it. The store does not apply it to every contender.
It decides on the first contender it reaches, so a slot with two or more live
facts can end up in one of two shapes:

- **Shape A.** An agent writes `6543`; a pulled source writes `6432`, recorded
  as a challenger of the agent's fact; an agent then writes `5432`. The store
  supersedes one fact, so the pulled `6432` stays believed beside `5432`.
- **Shape B.** An agent writes `5432`; a pulled source writes `6432`, recorded as
  a challenger of the agent's fact; a pulled source then writes `6543`. The store
  supersedes the same-rung `6432` and never considers the agent's fact above it,
  so `6543` is not recorded alongside `5432`.

Until this is fixed, a fact that looks settled can have a live contender beside
it. Check `GET /api/memory?q=...` for every live value in a slot, not only the
first answer, before relying on a single value.

## Say what kind of fact it is

A fact is one of three things, and the store treats them differently
([MEMORY-ANCHORS.md](MEMORY-ANCHORS.md)):

- **Event**: something that happened. Pass `event: true`. It is kept as history.
- **Preference or rule**: what a person wants. No checks.
- **Claim**: how something is now. Name the exact file, service, port or commit
  in the text, pass `anchors` for it, and pass `host` when it is about another
  machine. A claim that names nothing checkable cannot be told from a stale one
  later.

A multi-step task that worked and will be repeated is a procedure, not a fact:
use `remember_procedure` ([MEMORY-PROCEDURES.md](MEMORY-PROCEDURES.md)).
