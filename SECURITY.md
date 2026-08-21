# Security model

grimoire stores notes and — uniquely — an encrypted vault of API keys / tokens that
your AI can *use* but never *read*. This document states the threat model and the
controls that back it. Security-relevant code: `go/internal/crypto`,
`go/internal/secrets`, and the security headers in `go/internal/api`.

## What grimoire protects

1. **Secret values at rest** — API keys, tokens, MCP credentials.
2. **Note contents you mark encrypted** — sealed on disk, out of index/search/RAG.
3. **The confidentiality boundary between the AI and raw secrets** — the AI can
   trigger *scoped, audited* use of a secret but never receives its value.
4. **The integrity boundary between text you wrote and text somebody else
   wrote** — connectors put other people's writing into the same corpus an
   agent reads from, and that text must not be able to instruct the agent or
   overwrite what you told it. See *Untrusted content* below. This one is
   partly a mitigation rather than a guarantee, and is measured rather than
   asserted: `benchmarks/injection/`.

## Secret vault

- **Key derivation: Argon2id** (64 MiB, t=3, p=4) from your passphrase + a random
  per-vault salt. Memory-hard → resistant to GPU/ASIC brute force. Vaults created
  before this used PBKDF2-HMAC-SHA256 (240k iters) and still unlock; the KDF is
  recorded per vault (`kdf` field) so upgrades are transparent.
- **Encryption: Fernet** (AES-128-CBC + HMAC-SHA256) — authenticated; tampering
  is detected on decrypt.
- **The passphrase is never stored.** The derived key lives only in process memory
  and only while unlocked. A verifier token validates the passphrase on unlock
  without storing it.
- **Locked by default.** `lock()` / panic-lock drops the key.
- **Brute-force protection:** after 5 failed unlocks the vault enters an
  exponential-backoff lockout (30s → capped at 1h) — even the correct passphrase
  is refused during the window.
- **Idle auto-lock:** the key is dropped after `GRIMOIRE_VAULT_IDLE_LOCK` seconds of
  inactivity (default 900) to shrink the exposure window.
- **Unattended unlock is opt-in and named.** `GRIMOIRE_VAULT_PASSPHRASE_FILE`
  makes the server unlock itself at startup from a local file, which trades the
  "only a human holds the passphrase" property for a broker that survives a
  restart. It is off by default; the file must not be readable by group or
  other, and the server refuses it if it is. It never initializes a vault — an
  empty store sealed under a passphrase nobody chose is a worse failure than a
  locked one. Prefer it to the alternative deployments reach for otherwise: a
  boot-time script that POSTs the passphrase to `/api/vault/unlock` puts the
  passphrase on the HTTP surface and covers only the first start, so any later
  restart leaves the broker locked and every call failing.

## USE-not-READ broker

Agents never get raw secrets. They get a **grant** (a random token, scoped to an
exact origin + path prefix, time-boxed) and grimoire **brokers** the outbound call,
injecting the secret into a request header. The response is returned; the secret
value is not.

- **Scope matching is origin-exact + path-prefix** — it parses both URLs and
  compares (scheme, host, port) exactly, then checks the path prefix on whole
  segments (`/v1` does not authorize `/v10`). This blocks the classic prefix
  bypass (`https://api.github.com` does **not** authorize
  `https://api.github.com.evil.com`), and the inverse where the scope appears
  in the *path* of an attacker's URL.
- **Redirects are re-checked against the scope.** Go strips `Authorization` on
  a cross-host redirect, but the broker injects whatever header the caller
  named, and an `X-Api-Key` would otherwise follow a 302 to another host.
- **SSRF guard:** the target host is resolved and requests to
  private / loopback / link-local / reserved / multicast / unspecified addresses
  are refused. Cloud-metadata & link-local (`169.254.0.0/16`, incl.
  `169.254.169.254`) are **always** refused, even when internal targets are
  enabled. Self-hosters who legitimately broker to LAN services set
  `GRIMOIRE_BROKER_ALLOW_PRIVATE=1` (metadata stays blocked).
  The check runs at CONNECT time on the address the socket is about to use, not
  on a hostname resolved earlier, so DNS rebinding does not defeat it — and it
  covers every redirect hop, since each hop dials again.
- **Every grant and every broker call is written to an append-only audit log.**
- **Broker requires the vault unlocked** *and* a valid grant — defense in depth: a
  leaked grant token alone cannot broker anything.
- Only `http`/`https` schemes are permitted (no `file:`, `gopher:`, …).

## Untrusted content

Connectors (Slack, Confluence, Jira, Drive, GitHub, RSS) and web fetching bring
in text that people outside this vault can write. It lands as ordinary markdown
notes in the same index, retrieved into the same context as your own notes, and
handed to a reader that also holds the credential broker.

Every note carries an `origin` in its frontmatter, and a binary trust level
derived from it (`go/internal/trust`). A note with no origin is trusted —
that is every note written before this existed, and every note a person types.
A note whose origin names a connector, a feed or a host is untrusted. A person
may override either way with `trust: trusted|untrusted`; an unrecognised value
is ignored rather than guessed at, so a typo cannot silently promote a document.

What that enforces:

- **Retrieval reports it and can exclude it.** Every hit carries `trust` and
  `origin`; `trusted=1` on any content route drops untrusted rows. The filter
  is applied INSIDE ranking, not to its output — BM25 IDF is computed over the
  corpus, so an attacker who can post in a connected channel could otherwise
  shift the ranking of your own notes by flooding it with terms.
- **The reader is told.** Untrusted passages are wrapped in
  `<<<UNTRUSTED DOCUMENT n — origin: … >>>` markers with a one-paragraph rule
  above them. The fenced text cannot close its own fence (`trust.Neutralize`),
  which is the mechanical half; the rest is a prompt, and a prompt is not a
  boundary. `benchmarks/injection/` measures what it is worth.
- **Untrusted text cannot rewrite memory.** A fact whose origin is untrusted may
  not supersede or retract a fact that came from you. On the model-assisted
  path this is enforced by never OFFERING trusted facts as candidates, so no
  prompt can name one to overwrite — the same mechanism that protects an
  `immutable` fact.

**Not claimed:** that a determined injection cannot get through. It can. The
controls here reduce a class of it, are measured against a no-intervention
baseline, and are not a substitute for the credential broker's scope checks —
which are the control that actually stops a stolen instruction from being
useful, because an injected URL is not on the grant's origin.

## Requesting a credential

An agent may ASK for a grant it does not have (`POST /api/secrets/requests`).
Asking issues nothing: a pending request confers no access, and the response
carries no token. A person approves or denies.

- Asking does **not** require an unlocked vault — that is precisely when nobody
  is present. Approving does, because approving mints a credential. Denying
  does not: needing to unlock the credential store to refuse access to it is
  backwards.
- Asking for a secret that does not exist is accepted rather than refused, so
  the route cannot be used to enumerate which secrets a vault holds. The name
  is validated at approval, in front of a person.
- `GET /api/secrets/requests/{id}` is the one read in the product that can
  return a live grant token. It answers only the grantee the request was
  created for; an empty or mismatched grantee gets a 404, and the same 404
  covers "no such request" so the route is not an oracle for request ids.
  Listings never carry tokens, and the approval response does not show one to
  the approver.
- A decided request cannot be decided again. An approval that can be replayed
  is one anybody holding the id can replay, minting a fresh grant per attempt.

## Encrypted notes

Marking a note encrypted seals its body with the vault key. The plaintext never
touches disk, the SQLite index, FTS search, the vector store / RAG, the e-ink
`/read` surface, or HTML export (encrypted notes are refused there). Duplicating,
trashing, pinning, or tag-renaming an encrypted note operates on the ciphertext
and never needs the key. Editing requires an unlocked vault.

## HTTP surface

- **Security headers on every response:** a strict `Content-Security-Policy`
  (`script-src 'self'`, `object-src 'none'`, `base-uri 'self'` — no inline or
  external scripts), `X-Content-Type-Options: nosniff`, `Referrer-Policy:
  no-referrer` (so a `?token=` never leaks via Referer), `X-Frame-Options`
  (default `SAMEORIGIN`, override with `GRIMOIRE_FRAME_OPTIONS`), and
  `Cross-Origin-Opener-Policy: same-origin`.
- **XSS:** both renderers (`go/internal/render`, the PWA's `mdToHtml`) escape HTML
  first, then apply a small allowlist of formatting. Wiki-links/images become
  attributes on escaped text; no user markup reaches the DOM as live HTML. The
  CSP is defense-in-depth on top.
- **Auth token** (optional, `GRIMOIRE_AUTH_TOKEN`): gates every route except
  `/api/health`, compared in constant time over SHA-256 digests (fixed width,
  so the comparison cannot leak the token's length). Accepted as
  `Authorization: Bearer`, as a `grimoire_auth` cookie, or once as `?token=` —
  which is then promoted to an `HttpOnly; SameSite=Strict` cookie so the
  credential stops travelling in URLs. Empty means open, as it always has.
  `grimoire-mcp` presents the same token automatically.
- **Path traversal:** every vault path goes through `safe_path` / `safe_raw_path`,
  which resolve and confine to the vault root and reject `.grimoire`. The
  `/api/file` route and vault export exclude `.grimoire` so the secret store and
  index never leave.
- **No CORS headers** are set → browsers enforce same-origin for API calls.

## Reading the read audit

The restricted-read trail (`read_audit`) is now queried as well as written:
`GET /api/admin/reads/anomalies` reports bursts — many distinct documents by
one caller in a short sliding window, or a run of denials. Administrator-only,
like the trail itself. It computes on demand and reports; there is no daemon,
no stored threshold state and no notification path, deliberately. Sample
document paths in a result are capped, because a response that quoted every
restricted document a caller touched would be its own disclosure. An empty
result where nothing is recorded is reported as "not applicable", not as
"clear".

## Sync

Background auto-sync (`GRIMOIRE_SYNC_PEER`) authenticates to the peer with
`GRIMOIRE_SYNC_TOKEN` (sent as a Bearer header, so it never appears in a URL/log).
On the receiving side that token is accepted **only** on the routes a peer
actually calls (`/api/sync/manifest`, `/api/sync/pull`, `/api/sync/push`,
`/api/crdt/*`): a credential shared with another machine does not also unlock
the secret vault. Peer authentication requires `GRIMOIRE_AUTH_TOKEN` to be set,
since that is what turns the gate on.
Sync moves plain note files; it never transmits the secret store (`.grimoire/` is
excluded from every export/sync path). Direction is last-writer-by-mtime, but no
edit is silently lost — a pull that would overwrite a locally-changed note first
preserves the local copy as a `… (conflict …)` file, and pushes are conflict-
copied on the peer.

## Deployment guidance

- Front grimoire with your own authenticated reverse proxy (the homelab uses
  Authelia + a Tailscale-gated network). The optional `GRIMOIRE_AUTH_TOKEN` is a
  second factor, not the primary gate.
- Keep `.grimoire/` (which holds `secrets.json` and the index) off any sync/backup
  that leaves your control unless separately encrypted.
- Choose a strong vault passphrase — Argon2id raises the cost of guessing, but a
  weak passphrase is still the weakest link.
- Leave `GRIMOIRE_FOLLOW_SYMLINKS` off unless you control everything that lands
  in the vault. With it on, a symlink written into the vault — by a sync client,
  a shared folder, another user — decides what the server reads and serves.

## Reporting

This is a personal / self-hosted project. If you find an issue, open a private
report rather than a public issue.
