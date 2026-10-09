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
