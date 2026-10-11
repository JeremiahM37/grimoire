# Sealed facts and tombstones

Two limits from `MEMORY-PRIVACY.md` are addressed here.

- **Limit 5.** `vis=private|sensitive` is a retrieval filter. The fact's text
  stays in the note file, the raw note reads and the full-text index, and
  nothing makes the owner a boundary.
- **Limit 6.** `forget --cascade` reaches the vault, but not backups, git
  history or sync peers. A forgotten fact can come back from any of them.

`vis=sealed` and the tombstone log are the answers. Each is described below
with what is measured here, what is designed but not measured, and what is open.

## Sealed facts (`vis=sealed`)

### What is written

```
- **2026-10-10 09:00 · claude** — [sealed] <!--m id=3f9c0a1b7e2d vis=sealed owner=user:jam key=0a1b… sealed=gAAAAAB…-->
```

- The text is the placeholder `[sealed]`. The fact is not in the bullet.
- `sealed=` is the ciphertext: a Fernet token (`internal/crypto`, AES-128-CBC
  with HMAC-SHA256, the same primitive the secret vault and encrypted notes
  use). No new algorithm is introduced.
- `key=` names a per-fact data key. The data key is 256 random bits, and it
  lives only in the key store, wrapped by the vault session key (a Fernet token
  under the session key).
- `owner=` is the writing account: `user:<id>` on a multi-user deployment, or
  `local` on an unrestricted one. An anonymous caller cannot seal.
- The id is a hash of the ciphertext, not the plaintext. Hashing the plaintext
  of a short fact would give anyone who can read the note a guessable hash of it.

Sealed facts are appended, never reconciled. Reconciliation sends the new fact
and its candidates to a model, which a sealed fact must not do. Extraction
(`infer`) is skipped for the same reason, and so is the volatility estimate.

### Who reads what

| caller | `recall` with `include_private=1` |
|---|---|
| the owner, vault unlocked | the plaintext |
| any other account, or the owner with the vault locked | the placeholder |
| anonymous | nothing: no account, so no owner |

Without `include_private` a sealed fact is hidden, like `private`. The index
never holds its text, so search, context, briefing, profile, export and the
stream never carry it. The `expand`, `hops`, `explain` and `shape=notes` views
are not reached by the reveal: they show the placeholder, or leave the bullet
out.

Writing a sealed fact needs an unlocked vault (423 otherwise). Editing its text
in place is refused (409): an edit would put plaintext beside the old
ciphertext. A sealed fact is removed on forget and never struck through,
because its key is destroyed and a struck bullet would be ciphertext no one can
open.

### Key store

The key store is one JSON file, mode 0600, that holds wrapped data keys.

- Default: `<vault>/.grimoire/seal-keys.json`, beside the secret vault. The
  `.grimoire` directory is not synced to phones (`.stignore`), but it is inside
  the vault tree, so a backup of the vault takes it too.
- `GRIMOIRE_SEAL_KEYS=/path/to/seal-keys.json` moves it. Point it at a path
  that backups do not cover. Crypto-shred only holds if no copy of the key
  store exists anywhere the ciphertext does.

The trade-off is real. A key store outside the vault is not restored with the
vault, so after a move to a new machine the sealed facts cannot be read there.
That is what crypto-shred means, but it is a choice the operator must make.

### Forget and crypto-shred

Forgetting a sealed fact (any forget path, cascade or not):

1. removes the bullet from the note,
2. destroys its wrapped data key (the key store entry is deleted and the file
   rewritten),
3. writes a tombstone for its id.

After this, every copy of the ciphertext (a backup, an old git commit, a
snapshot in the history store, a sync peer's history) is unreadable, because
the only key that opens it no longer exists in the store. A second forget of
the same fact succeeds with nothing to destroy.

### Measured here

`go test` only, deterministic, offline. These are pass/fail facts, not timings.

- `internal/seal`: round trip, wrong session key fails, a shredded key returns
  `ErrShredded`, a repeated shred is a no-op, other keys survive a shred, the
  key store is 0600, two seals of one text share no key.
- `internal/memory/seal_test.go`: a sealed bullet round-trips byte for byte, is
  not read as hand-edited, is hidden and redacted, and an unknown `vis=` still
  fails closed to private. An ordinary bullet formats byte-identically.
- `internal/api/memory_seal_test.go`:
  - the canary text is absent from the note bytes, search results and export;
  - a locked vault refuses the write with 423;
  - alice (owner) reads the text back, bob and the administrator get the
    placeholder, over the real HTTP surface with three accounts;
  - an in-place edit is refused and leaves the note unchanged;
  - a saved copy of the note taken before the forget cannot be opened after it,
    and the restored copy is stripped again on reindex.

## Tombstones

### What is written

`memory-tombstones.md` at the vault root, one JSON object per line:

```
{"salt":"…","hash":"…","at":"2026-10-10 09:00","reason":"forget","mac":"…"}
```

- `mac` is an HMAC-SHA256 over salt, hash, time and reason. Its key is
  HMAC-SHA256 of the label `grimoire/seal/tombstone-mac/v1`, keyed by the vault
  session key (the derivation uses `crypto/hmac`, as `internal/crypto` does; no
  new algorithm). A passphrase change re-signs signed lines under the new
  session key before the key is swapped.
- Each line is checked when the log is loaded:

| status | when | effect |
|---|---|---|
| `verified` | `mac` checks under the session key | suppresses |
| `unsigned` | no `mac`: written while locked, or appended by anyone with write access | ignored and reported; never suppresses |
| `unverified` | `mac` present, vault locked, so it cannot be checked | ignored and reported; nothing is stripped while locked |
| `rejected` | `mac` present and wrong | ignored and logged by count; the line stays in the file as evidence |

  Only a verified line suppresses. A line anyone could append (a hash of a
  visible fact id, with no MAC) therefore deletes nothing, and nothing is
  stripped from any note while the vault is locked. The cost: a fact forgotten
  while the vault was locked gets an unsigned line that is not honoured, so
  that fact is not protected from a stale copy until it is forgotten again
  while unlocked. The production vault unlocks itself at start, so this is
  the rare case.

- `hash` is `sha256(salt ‖ fact id)`. The fact id is not stored. The salt is
  per tombstone, so two tombstones for the same id cannot be matched.
- It is a markdown file on purpose: note sync and the peer manifest walk
  markdown only, so a log in another format would never leave the device.
- It is indexed as a note and appears on phones. It holds hashes and reasons,
  not text.

### Where it is applied

- **Every hard removal** writes a tombstone (`removeEntry`, which the hard
  forget and the cascade both use). A sealed fact's removal also destroys its
  key.
- **Reindex** strips tombstoned bullets from every memory note before the index
  is rebuilt (`POST /api/reindex`).
- **Sync merge** strips them from a note a peer just pushed, before the index
  sees it (`mergeCRDT`, the peer merge route).
- **Cloud-folder pull** (`internal/cloudsync`) strips tombstoned bullets at
  write time, through `Engine.Filter`, which the API sets to the same strip.
  A pulled note is therefore never written with a forgotten bullet in it, even
  before a reindex.
- **Write** refuses a new fact whose id is tombstoned (409). This is what
  stops a restore of an old export from putting a forgotten fact back.

A stripped bullet is removed from the file, with a snapshot of the note before
the rewrite, so the history still has it.

### Measured here

- `internal/seal/seal_test.go`: the log stores no id in clear, repeated adds
  are one tombstone, `Suppressed` matches exactly the forgotten id, and
  `StripBullets` removes only the tombstoned line and changes nothing else.
- `TestForgetShredsTheKeyAndNoSavedCopyOpens`: a restored copy of a forgotten
  bullet is stripped by reindex.
- `internal/seal`: MAC verification (a forged line is rejected, a line signed
  under another key is rejected, unsigned lines suppress and are reported, a
  locked load reports unverified), re-signing on a passphrase change, and the
  keystore rewrap.
- `TestStalePeerCRDTPushCannotResurrectAForgottenFact`: a peer's document
  fetched before the forget is pushed through `POST /api/crdt/merge`; the note
  does not contain the fact afterwards, and recall does not return it.
  **Measured, and it does not show the guard is needed.** With the
  suppression call removed from the merge route, the test still passes: the
  CRDT's own deletion wins for this shape, because the peer's atoms share
  history with the local deletion. The guard is defence in depth for shapes
  the CRDT does not cover. I did not find a realistic shape that reaches it.
- `TestCloudFolderPullCannotResurrectAForgottenFact`: device A forgets, device B
  (still holding the fact) publishes a newer version, and A pulls it through
  the real folder sync. The fact is absent after the pull. **Measured, and it
  does not show the guard is needed either:** with `Engine.Filter` disabled
  the test still passes, for the same CRDT reason.
- `TestPassphraseChangeKeepsSealedFactsAndTombstones`: after
  `POST /api/vault/change-passphrase`, the sealed fact is still readable by its
  owner, and a tombstone written before the change still verifies and
  suppresses.

### Behaviour change

`TestMemoryImageExportsAndImportsItsBytes` used to remove a bullet with
`removeEntry` and import it again from an export. Removal now tombstones the
id, so that import is refused. The test clears the tombstone log before the
import, which stands for the operator's deliberate restore. Nothing else in the
suite depended on a removed fact coming back.

## What remains out of reach

These are real, and the design does not claim to close them.

- **Ciphertext and key together.** A copy of the note and of the key store
  made before the forget opens the fact. Crypto-shred only holds for copies
  that are missing the key store.
- **Plaintext already elsewhere.** A fact written before sealing, or a
  plaintext copy made before the forget, is still plaintext in whatever holds
  it. A fact that was sent to a model in a prompt, or logged, stays there.
  Cues recorded with `context` are not sealed (`addAgentCues`); a caller that
  puts the fact in `context` puts it in the cue store.
- **Peers do not hold the key.** The key store is not synced. A sync peer
  receives sealed bullets as ciphertext it cannot open. Sealed facts are
  readable on the device that holds the key store, and nowhere else.
- **Soft forget of a sealed fact.** Not offered: forget removes it.
- **Tombstones are only as good as the session key.** A peer's signed line
  verifies only if it was signed under the same session key, which two devices
  share only by sharing the passphrase, so a second device honours none of
  another device's tombstones otherwise. Anyone who can write the log can
  delete lines from it (undoing a forget) even though they can no longer forge
  new ones.
- **Owner is bound in the key store, not the note.** `owner=` on a bullet is a
  label. The key store records which account sealed each key and refuses
  anyone else, so a bullet copied into another account's note, or an edited
  `owner=`, opens nothing. A deployment with no accounts has one owner,
  `local`.
- **Concurrency.** Every read-modify-write of the key store and the tombstone
  log is serialised by a process-wide lock per file and written through a
  unique temp file, so a forget cannot be undone by a concurrent write. Two
  server processes sharing one key store are not covered.
- **Retracted facts keep their key.** `memory prune` skips sealed facts
  (`visibility=''` in its query), and a plain retraction leaves the bullet in
  the note, so only `forget` shreds a key. Hand-deleting a bullet or a whole
  note also leaves the key in the key store; run `forget` to shred it.
- **Raw note reads show the sealed bullet.** `GET /api/notes/...`, the MCP
  `read_note` and `backlinks` tools, history versions and `shape=notes` return
  a memory note's body as stored, so a reader of a shared memory note sees
  another owner's `sealed=` ciphertext, `key=` id and `owner=`. Without the key
  store, and with the owner bound there, that opens nothing, but it does name
  that a sealed fact exists and who owns it. The body is returned unfiltered on
  purpose: an editor that reads a note and writes it back would otherwise
  strip the ciphertext.
- **Metadata is not sealed.** `ev=` (evidence text), `chk=`, task, category and
  session stay in clear on a sealed bullet and are returned to every reader.
  Do not put the secret in the evidence.
- **A superseding write can inherit the seal after its text was processed.**
  A plain write that supersedes a sealed fact becomes sealed, but its own text
  has already been through extraction, so write the replacement with
  `vis=sealed` yourself.
- **Notes with hidden facts are not consolidated.** `consolidate` skips any
  memory note that holds a private, sensitive or sealed fact and lists it as
  held, because a model rewrite could drop the bullet without shredding its key
  and the prompt would carry the ciphertext. Cascade forget of a sealed fact
  runs the plain forget (there is no text to match), and a write that goes
  through the note API re-applies the tombstone log.
- **Passphrase change.** Wrapped keys and tombstone signatures move to the new
  session key before the swap and are moved back if any later step fails.
- **Recall expansion views.** `expand`, `hops`, `explain` and `shape=notes`
  do not reveal the text to the owner. Owners get it from flat recall with
  `include_private=1`.
- **Existence.** A sealed bullet is visible in the note file: its time, its
  agent, that it is sealed, and its owner. Only the text is hidden.
- **Unrestricted deployments.** With no accounts, every caller is `local` and
  so is the owner. The boundary between people starts with accounts.
- **Not measured:** the cost of decryption on recall, and the cost of the
  tombstone read on every write. Both are small by construction (one key
  unwrap per owned sealed hit, one small file read per write) but neither has
  been timed.
