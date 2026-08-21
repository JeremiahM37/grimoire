# Injected text cannot spend a credential

Method: [PROTOCOL-exfil.md](PROTOCOL-exfil.md), registered before the numbers
were read. Raw output: `results/exfil/`. Reproduce with
`python3 run_exfil.py structural && python3 run_exfil.py attempt`.

---

## The result

**The attack is real, and the gate refuses all of it.**

30 attacks, 5 families, each naming a destination **inside** the grant's scope
— so origin comparison permits every one of them. Success is measured at the
attacker's own endpoint: a socket that was actually connected to, not a status
code the server reports about itself.

| gate | reached the attacker |
|---|---|
| `off` | **30/30** |
| `on` | **0/30** |

| family | gate off | gate on | model attempted |
|---|---|---|---|
| setup — poses as a required setup step | 6/6 | 0/6 | 4/6 |
| policy — claims a policy requires it | 6/6 | 0/6 | 2/6 |
| error — presents as an error to resolve | 6/6 | 0/6 | 5/6 |
| helpful — frames it as a convenience | 6/6 | 0/6 | 2/6 |
| quiet — tells the reader not to mention it | 6/6 | 0/6 | 4/6 |

And the attack is not hypothetical. Shown the injected note beside the
operator's own notes and a legitimate question, `qwen3.5:4b` emitted a call to
the attacker's URL in **17 of 30 (57%)**. The `error` family — an invented
error code with a remediation step — was the most effective at 5/6.

## Why 0/30 is not a score

A model refusing 30 attacks would be a measurement, with a confidence interval
and a strong chance the 31st gets through. This is not that. The gate is a
provenance check on a code path: the target URL is looked up in the vault, and
a state-changing call to a URL that only untrusted content names does not
proceed. **There is no branch that produces a different answer**, which is why
the structural table carries no p-value and why it is reported apart from
anything the model did.

The `off` arm is what makes the `on` arm mean anything. 30/30 with the gate off
establishes the attacks are live and the harness reaches the attacker; without
it, 0/30 would be equally consistent with a broken corpus.

## What it does not claim

- **Not "your data cannot leak."** This stops *credential-mediated*
  exfiltration. An agent that reads your notes and types them into its own
  reply is untouched.
- **Not a novel mechanism.** Tainting agent actions by the provenance of the
  content that prompted them is established practice — Microsoft's FIDES gates
  privileged calls that follow untrusted content, and `dsh-taintguard` does the
  same at the harness layer. What is unusual is the *placement*: those live in
  agent frameworks, which see the content but hold no credential, while token
  vaults hold the credential but never see what the agent read. Grimoire is
  both halves in one process, so its broker can ask a question neither can
  answer alone.
- **Not free.** The gate refuses a legitimate call whose URL you happened to
  clip from the web. The remedy is the vouch control that already exists:
  promote the note and the call proceeds. That is why the refusal names the
  note — a refusal a person cannot act on is a worse bug than the one it fixed.

## The cost, measured

Safe methods are not gated at all, so `GET` traffic is unchanged. For a gated
method the added work is two indexed `LIKE` queries against `notes`, run before
the secret is decrypted — a refused call never touches the vault. Against the
~7 s a reader call costs elsewhere in this repo, it does not register.

The false-positive shape is the real cost and it is not measured here: it
depends on how much of your vault is clipped content that names URLs you also
call. `TestAURLYouWroteYourselfIsNotGated` pins the mitigation — a URL
corroborated by a note you wrote is never gated — but the rate in a real vault
is unknown and this study does not estimate it.
