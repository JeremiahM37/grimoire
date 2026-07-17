# Security model

mnemo stores notes and — uniquely — an encrypted vault of API keys / tokens that
your AI can *use* but never *read*. This document states the threat model and the
controls that back it. Security-relevant code: `server/crypto.py`,
`server/secrets.py`, and the middleware in `server/app.py`.

## What mnemo protects

1. **Secret values at rest** — API keys, tokens, MCP credentials.
2. **Note contents you mark encrypted** — sealed on disk, out of index/search/RAG.
3. **The confidentiality boundary between the AI and raw secrets** — the AI can
   trigger *scoped, audited* use of a secret but never receives its value.

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
- **Idle auto-lock:** the key is dropped after `MNEMO_VAULT_IDLE_LOCK` seconds of
  inactivity (default 900) to shrink the exposure window.

## USE-not-READ broker

Agents never get raw secrets. They get a **grant** (a random token, scoped to an
exact origin + path prefix, time-boxed) and mnemo **brokers** the outbound call,
injecting the secret into a request header. The response is returned; the secret
value is not.

- **Scope matching is origin-exact + path-prefix** — it parses both URLs and
  compares (scheme, host, port) exactly, then checks the path prefix. This blocks
  the classic prefix bypass (`https://api.github.com` does **not** authorize
  `https://api.github.com.evil.com`).
- **SSRF guard:** the target host is resolved and requests to
  private / loopback / link-local / reserved / multicast / unspecified addresses
  are refused. Cloud-metadata & link-local (`169.254.0.0/16`, incl.
  `169.254.169.254`) are **always** refused, even when internal targets are
  enabled. Self-hosters who legitimately broker to LAN services set
  `MNEMO_BROKER_ALLOW_PRIVATE=1` (metadata stays blocked).
  *Caveat:* DNS is resolved at check time; a determined DNS-rebinding attacker who
  also controls the vault could still race it — but brokering already requires an
  unlocked vault, which is the trust boundary.
- **Every grant and every broker call is written to an append-only audit log.**
- **Broker requires the vault unlocked** *and* a valid grant — defense in depth: a
  leaked grant token alone cannot broker anything.
- Only `http`/`https` schemes are permitted (no `file:`, `gopher:`, …).

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
  (default `SAMEORIGIN`, override with `MNEMO_FRAME_OPTIONS`), and
  `Cross-Origin-Opener-Policy: same-origin`.
- **XSS:** both renderers (`server/render.py`, the PWA's `mdToHtml`) escape HTML
  first, then apply a small allowlist of formatting. Wiki-links/images become
  attributes on escaped text; no user markup reaches the DOM as live HTML. The
  CSP is defense-in-depth on top.
- **Auth token** (optional, `MNEMO_AUTH_TOKEN`): compared with
  `hmac.compare_digest` (constant-time). Prefer the `Authorization: Bearer`
  header over `?token=` (the latter can appear in proxy logs).
- **Path traversal:** every vault path goes through `safe_path` / `safe_raw_path`,
  which resolve and confine to the vault root and reject `.mnemo`. The
  `/api/file` route and vault export exclude `.mnemo` so the secret store and
  index never leave.
- **No CORS headers** are set → browsers enforce same-origin for API calls.

## Deployment guidance

- Front mnemo with your own authenticated reverse proxy (the homelab uses
  Authelia + a Tailscale-gated network). The optional `MNEMO_AUTH_TOKEN` is a
  second factor, not the primary gate.
- Keep `.mnemo/` (which holds `secrets.enc` and the index) off any sync/backup
  that leaves your control unless separately encrypted.
- Choose a strong vault passphrase — Argon2id raises the cost of guessing, but a
  weak passphrase is still the weakest link.

## Reporting

This is a personal / self-hosted project. If you find an issue, open a private
report rather than a public issue.
