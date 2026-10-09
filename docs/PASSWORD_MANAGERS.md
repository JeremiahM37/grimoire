# Password managers as credential sources

Grimoire's credential model is **use, don't read**: an agent gets a scoped,
time-boxed grant, and `use_credential` makes the request server-side with the
secret injected, so the value never enters the agent's context.

If you already keep secrets in a password manager you don't have to copy them
into Grimoire's vault. A handle can point at an item in the manager instead:

```
grimoire secret link github bitwarden://<item-id>/password
```

`github` is now an ordinary handle. Grants, scopes, expiry, use limits, the
audit log and the `request_credential` approval flow work on it **unchanged**.
The only difference is where the value comes from, and when.

## What changes, and what doesn't

- The value is fetched **at use time**, server-side, and injected into one
  outbound request. It is cached in memory for at most `cache_seconds`
  (provider setting, or `GRIMOIRE_PROVIDER_CACHE_SECONDS`; **default 0**, no
  caching). Locking the vault, the idle lock, and editing or removing a
  provider all drop the cache.
- The value is **never written to disk** by Grimoire, never returned by any API
  or MCP tool, and is scrubbed from the target's response body if the target
  echoes it back. Error text from a manager is stripped of your unlock material.
- `list_grants`, `GET /api/secrets` and `grimoire secret list` show the
  **provider and reference** (`bitwarden://item/password`). A reference locates
  an item; it is not the secret.
- A grant can be issued while the manager is locked. Using it while the manager
  is locked fails with a clear `provider unavailable` error. **There is no
  fallback** to any other value, and a single-use grant is *not* spent by that
  failure.
- Unlock material (Bitwarden session or master password, 1Password token, kdbx
  password, Vault token) lives in Grimoire's own encrypted vault, sealed under
  the vault key, not in env files. The vault must be unlocked for any of this to
  work. Subprocess managers (`bw`, `op`, ...) run with a scrubbed environment and
  receive secrets by environment variable or stdin, never argv.
- Linking a handle never copies the value in. `grimoire secret import` is the
  separate, explicit way to copy items **into** Grimoire's vault.

## Commands

```
grimoire secret provider kinds                 # what is supported and each setting
grimoire secret provider add NAME --kind KIND [--set k=v]… \
        [--secret k] [--secret-env k=ENVVAR] [--secret-file k=PATH]
grimoire secret provider list | test NAME | remove NAME
grimoire secret link HANDLE REF [--provider NAME] [--note TEXT]
grimoire secret unlink HANDLE
grimoire secret import --from bitwarden --folder NAME [--item ID]… [--prefix P] [--yes]
```

Secrets are read from a hidden prompt, an environment variable or a file, never
from an argument (arguments land in shell history and `/proc`). With a single
provider of a kind, `link` picks it from the reference's scheme; with several,
pass `--provider`. A provider cannot be removed while handles still link to it.

HTTP routes (all admin-gated): `GET/POST /api/secrets/providers`,
`DELETE /api/secrets/providers/{name}`, `POST /api/secrets/providers/{name}/test`,
`POST /api/secrets/link`, `POST /api/secrets/{name}/unlink`. No route returns a
value or stored unlock material. Provider administration is deliberately not an
MCP tool: agents use credentials, they do not wire up new sources.

## Supported managers

| Manager | Kind | Scheme | Path used | Unlock material |
|---|---|---|---|---|
| Bitwarden Password Manager, Vaultwarden | `bitwarden` | `bitwarden://` | `bw` CLI, or a loopback `bw serve` | session key, or master password (+ API key or email) |
| Bitwarden Secrets Manager | `bitwarden-sm` | `bws://` | `bws` CLI | machine-account access token |
| 1Password | `onepassword` | `op://` | `op read` with a service account, or a Connect server (REST) | service-account token or Connect token |
| KeePass / KeePassXC | `kdbx` | `kdbx://` | `keepassxc-cli` | database password (+ optional key file path) |
| HashiCorp Vault / OpenBao | `vault` | `vault://` | REST, KV v1/v2 | token, or AppRole role_id + secret_id |
| `pass` | `pass` | `pass://` | `pass show` (gpg) | none stored: gpg-agent must already hold the key |

### Bitwarden (and Vaultwarden)

Reference: `bitwarden://<item-id>/<field>` with field one of `password`
(default), `username`, `totp`, `notes`, `uri`, or `field/<custom field name>`.
Get item ids with `bw list items --search NAME`.

*CLI mode.* Requires `bw`. Two ways to unlock:

```
# A) you hand Grimoire a session key (expires whenever bw locks)
export BW_SESSION=$(bw unlock --raw)
grimoire secret provider add bw --kind bitwarden --secret-env session=BW_SESSION

# B) Grimoire unlocks itself with the master password (survives restarts)
grimoire secret provider add bw --kind bitwarden \
    --secret master_password --secret client_id --secret client_secret
#   client_id/secret: Bitwarden web vault > Account settings > Security > API key
#   (no API key? use --set email=you@example.com instead)
```

With B, Grimoire keeps its own isolated `bw` state directory
(`~/.config/grimoire/bw-NAME`, mode 0700, overridable with `appdata_dir`) so it
never changes your interactive `bw` login. `bw` itself caches your vault there
in encrypted form; that is `bw`'s own cache, not a Grimoire value store.

*Vaultwarden / self-hosted:* add `--set server=https://vault.example.com`.

*Serve mode:* run `bw serve` yourself, on loopback, already unlocked (or let
Grimoire unlock it with `master_password`), then
`--set serve_url=http://127.0.0.1:8087`. Non-loopback addresses are refused:
`bw serve` has no authentication, so anything that can reach it can read your
vault.

`secret import --from bitwarden --folder NAME` lists the folder and asks per
item before copying its password into Grimoire's vault (`--item ID` narrows the
set, `--yes` answers for you). The copies are ordinary stored secrets.

### Bitwarden Secrets Manager

Create a **machine account** and a project containing only what agents need,
issue an access token, then:

```
grimoire secret provider add bws --kind bitwarden-sm --secret access_token
grimoire secret link stripe bws://<secret-id>
```

This uses the `bws` CLI rather than the REST API: Secrets Manager responses are
end-to-end encrypted, and re-implementing that crypto in Grimoire is more risk
than shelling out to the vendor's client.

### 1Password

Reference: `op://vault/item/field` or `op://vault/item/section/field`.

*Service account (recommended):* create one in 1Password with access to **one
dedicated vault**; no desktop app or interactive sign-in is involved.

```
grimoire secret provider add op --kind onepassword --secret service_account_token
```

*Connect server:* `--set connect_url=https://connect.example --secret connect_token`.
Vault and item are looked up by name or id; fields match by label, id or
purpose (`password`, `username`).

### KeePass / KeePassXC

Reference: `kdbx://Group/Sub/Entry#Field`, field defaulting to `Password`
(also `UserName`, `URL`, `Notes`, `Title`, or a custom attribute name).

```
grimoire secret provider add kp --kind kdbx --set database=/path/agents.kdbx \
    [--set key_file=/path/key.keyx] --secret password
```

Uses `keepassxc-cli` (the database never has to be parsed by Grimoire, and no
new Go dependency is added). The kdbx file stays on disk as it always was; only
its password is held in Grimoire's vault. The key *file path* is a setting, so
protect the file itself.

### HashiCorp Vault / OpenBao

Reference: `vault://<mount>/<path>#<key>`. KV v2 is assumed (Grimoire adds the
`data/` segment); `--set kv_version=1` for v1.

```
grimoire secret provider add vault --kind vault --set address=https://vault:8200 --secret token
# or AppRole
grimoire secret provider add vault --kind vault --set address=... --secret role_id --secret secret_id
```

Use a token with a policy that reads only the paths agents need. AppRole logins
are held in memory and renewed on a 403. A sealed or unreachable server, or a
rejected token, is `provider unavailable`. Plain-HTTP and LAN addresses are
allowed (the usual homelab case); link-local and cloud-metadata ranges are
refused as everywhere else in the broker.

### pass

Reference: `pass://path/to/entry` (first line) or `pass://path/to/entry#field`
for a `field: value` line. Grimoire stores no unlock material: `pass`
decrypts with gpg, so **gpg-agent must already hold the key** (a key with no
passphrase, or one cached by an earlier unlock). Grimoire runs gpg with
`--pinentry-mode error`, so a locked key fails fast with an error rather than
hanging on a prompt. Settings: `store_dir`, `gnupg_home`.

## Threat model

Read this before pointing agents at a real vault.

**Once a provider is configured and unlocked, Grimoire can read everything that
credential can read.** A Bitwarden session or master password can reach your
entire personal vault; a 1Password service account, a Connect token, a Vault
token or a machine-account token reaches whatever it was granted. Grants limit
what *agents* can make Grimoire do, not what Grimoire itself is able to fetch.
Anyone who can unlock Grimoire's vault and run the CLI on that host can fetch
those values too (the CLI `run`/`import` commands exist for the operator).

So:

- **Create a dedicated, minimal credential for Grimoire.** Do not give it your
  personal master password if you can avoid it. Prefer, in order: a 1Password
  *service account* limited to one vault; a Bitwarden *Secrets Manager machine
  account* scoped to one project; a Bitwarden *organization collection* shared
  with a separate low-privilege Bitwarden user (use that user's API key and
  master password) that holds only agent-usable items; a Vault policy for a
  few paths; a separate small `.kdbx` database containing only those entries; a
  separate `pass` store/GNUPGHOME.
- Put into that scope only the items you are willing to let an agent *use*.
  A grant is the control on use; the scope of the unlock credential is the
  control on reach.
- The Bitwarden "master password" and "session" options are the weakest:
  they unlock a whole account. Use them only against a dedicated account.
- Grimoire's vault passphrase now protects those unlock secrets. Protect it
  accordingly (the unattended `GRIMOIRE_VAULT_PASSPHRASE_FILE` option makes
  them available to anything running as the service user).
- A leaked grant token lets its holder cause requests, within its scope and
  TTL, with the external value injected. Prefer short TTLs and `max_uses`.
- The target server sees the credential. A response that echoes it is scrubbed
  (exact-match only: a target that transforms or encodes it, or an agent that
  chooses a hostile in-scope URL, is outside what scrubbing can catch). Keep
  scopes to the exact API origin.
- Caching trades safety for speed: a nonzero `cache_seconds` keeps plaintext in
  Grimoire's memory for that long. Default is none.

## What was and wasn't verified

Built and tested against fakes only: stub `bw`, `bws`, `op`, `keepassxc-cli`,
`pass` executables on `PATH`, and `httptest` servers for `bw serve`, 1Password
Connect and Vault. No real account was used. The CLI flags and REST shapes were
written from the vendors' public documentation and, for `bw`, checked against
the locally installed `bw --help` (2026.4.1). **Not verified against live
services:** the `bws` JSON field names, 1Password Connect field/section
matching, `keepassxc-cli show` behaviour across versions, and Vaultwarden
quirks. Run `grimoire secret provider test NAME` and resolve one real
low-value item first.

Not supported: LastPass (`lpass` is unmaintained and needs an interactive
login), Dashlane (CLI is sync-oriented and interactive), and the 1Password
desktop-app integration (needs a GUI session and biometric unlock, which a
server cannot satisfy). A pure-Go kdbx reader was not added to avoid a new
dependency; `keepassxc-cli` covers the need.
