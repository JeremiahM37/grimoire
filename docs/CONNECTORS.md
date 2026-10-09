# Connected sources: mail, calendar, Drive, Slack, GitHub as agent extensions

Grimoire can pull accounts into the vault (a sync) and let any agent use them
live. Every agent reaches Grimoire over MCP, so the same five tools work from
Claude Code, Codex, Cursor, claude.ai or anything else:

| tool | does |
|---|---|
| `sources` | what is connected, what each can search/read/do, which actions are enabled, how fresh the synced copy is |
| `source_search` | the provider's own search, now (for stale or never-synced sources). Results are fenced as untrusted |
| `source_read` | one item by id: mail thread, Doc, Slack thread, issue, event. Fenced as untrusted |
| `source_act` | ask the source to do something. Needs an enabled action class AND, by default, the owner's approval |
| `source_action_status` | pending / executed / failed / denied |

Nothing is configured until you run `grimoire connect`. Credentials live in the
credential vault and are used by the server; no tool, route or note ever
returns one.

## Sources

| kind | what | live search | actions |
|---|---|---|---|
| `gmail` | Gmail API, one note per thread | yes | `create_draft`, `send_message` |
| `imap` | any IMAP server, app password | yes | none |
| `gcal` | Google Calendar, one note per event | yes | `create_event` |
| `gdrive` | Docs exported as text, text/markdown files | yes | `create_doc` |
| `outlook` | Microsoft 365 mail via Graph | yes | `create_draft` |
| `onedrive` | OneDrive files via Graph delta | yes | none |
| `slack` | channels (existing) | yes (user token) | `post_message` |
| `github` | issues/PRs (existing) | yes | `create_issue`, `comment` |

Mail: one markdown note per thread. Attachments are **listed, not ingested**.
Filters: `labels` (Gmail), `folders` (IMAP/Outlook), `since` (YYYY-MM-DD,
default 30 days), `query` (extra Gmail search). Cursors are incremental.
Office/PDF files on OneDrive are listed with metadata only.

## Connecting

```
grimoire connect google    --client-id ID --client-secret SECRET [--services gmail,gdrive,gcal] [--allow gmail.create_draft,gcal.create_event,gdrive.create_doc]
grimoire connect microsoft --client-id ID [--tenant common] [--services outlook,onedrive] [--allow outlook.create_draft]
grimoire connect slack     [--token-file F] --channels C01,C02 [--post-channels C01] [--allow slack.post_message]
grimoire connect github    --repo owner/name [--token-file F | --client-id ID]
```

Google and Microsoft use the OAuth loopback flow (PKCE, a listener on
`127.0.0.1`). The token, refresh token and client id are stored as one vault
secret (`google-oauth`, `microsoft-oauth`) and refreshed automatically before
they expire. Slack requires an https redirect URL, so it is a token paste;
GitHub offers a device flow (`--client-id`) or a pasted fine-grained token. The
vault must be unlocked (`GRIMOIRE_VAULT_PASSPHRASE_FILE`, or you are prompted).

**Read-only by default.** `connect` requests only read scopes. A write scope is
requested only for an action you name with `--allow kind.action`. To add one
later, run `connect` again with `--allow`.

`connect` also creates the connectors (hourly sync, trust `external`, no
actions enabled except those you passed to `--allow`). Edit them in the console
or `PUT /api/connectors/{id}`.

### Human steps: creating the apps

Grimoire cannot register an app in your cloud account for you.

**Google** (Google Cloud console)
1. Create or choose a project. APIs & Services, Library: enable *Gmail API*,
   *Google Drive API*, *Google Calendar API* (only those you use).
2. OAuth consent screen: User type External, add yourself as a test user. While
   the app is in *Testing*, Google expires refresh tokens after 7 days: either
   publish it (for personal use "In production, unverified" is fine) or re-run
   `connect` weekly.
3. Credentials, Create credentials, OAuth client ID, type **Desktop app**. Copy
   the client id and secret into `--client-id` / `--client-secret`.

**Microsoft** (Entra admin center, App registrations)
1. New registration; supported accounts as you need (`common` for personal and
   work); redirect URI platform *Mobile and desktop*, `http://127.0.0.1`.
2. Authentication: enable *Allow public client flows*.
3. API permissions are requested at consent time; work tenants may need an admin
   to approve `Mail.Read`/`Files.Read`. Use the Application (client) ID as
   `--client-id`; for a single tenant pass `--tenant <tenant id>`.

**Slack** (api.slack.com/apps)
1. Create an app from scratch, install it to the workspace.
2. For sync use bot scopes `channels:history`, `groups:history`, `users:read`
   (`channels:read` for membership mirroring) and invite the bot to each channel.
   For live **search** Slack only allows a *user* token with `search:read`.
   Add `chat:write` only if you enable `post_message`.
3. Paste the `xoxb-`/`xoxp-` token into `connect slack`.

**GitHub**: a fine-grained token on the one repository: Issues: read, Contents:
read (Pull requests: read); add Issues: write only for `create_issue`/`comment`.
(Classic OAuth `repo` scope is read+write and cannot be narrowed, which is why a
fine-grained token is recommended.)

**IMAP**: create an *app password* at the provider (Gmail, iCloud, Fastmail
require one) and store it as the connector's secret; set `host`, `username`.
Cleartext (`security: none`) is refused unless the host is loopback.

### What each scope is for

| scope | used for |
|---|---|
| `gmail.readonly` | sync and live search/read of mail |
| `gmail.compose` | `create_draft`. Google has no draft-only scope; this one can also send, so Grimoire only ever calls `drafts.create` and keeps sending as a separate opt-in action |
| `gmail.send` | `send_message` only |
| `drive.readonly` | sync and live search/read of Drive |
| `drive.file` | `create_doc`; reaches only files this app created |
| `calendar.readonly` | sync and search of events |
| `calendar.events` | `create_event` (invitations are not emailed unless `notify=yes`) |
| `Mail.Read`, `Files.Read`, `User.Read`, `offline_access` | Microsoft read, identity, refresh |
| `Mail.ReadWrite` | Outlook `create_draft` (drafts only; no `Mail.Send` is ever requested) |
| `search:read`, `chat:write` | Slack live search (user token), `post_message` |

## Agent actions: two keys and an audit trail

An action runs only if **both** hold:

1. **You enabled the class** on that connector: `actions: create_draft,...` in
   its config. Empty (the default) means the source is read-only to agents.
2. **A person approved this call**, unless you set `action_approval: none` on
   that connector. `source_act` returns `state: pending`; you decide with
   `grimoire actions` (`list`, `approve ID`, `deny ID --note ...`) or the REST
   API (`GET /api/source-actions`, `POST /api/source-actions/{id}/approve|deny`).
   Approving executes the *stored* parameters: the agent cannot change them
   afterwards. The existing credential-request queue mints grants and does not
   fit, so actions have their own queue modelled on it.

Also: parameters are validated against the action's declared fields; header
injection (CR/LF in mail headers) is rejected; `post_channels` restricts Slack
posting to an allowlist; GitHub actions are pinned to the connector's repo;
`action_rate` (default 10/hour per connector) caps requests, and at most 20
may wait for approval. Every search, read, request, decision and execution is
recorded in `source_audit` (`grimoire actions audit`, `GET /api/source-audit`):
who, what, outcome, never a credential or provider content.

Set `GRIMOIRE_ADMIN_TOKEN`: approval routes are admin surface, and without a
token an agent holding the API address could call them itself. On a multi-user
instance only administrators' keys can use connected sources at all, since they
are the owner's accounts.

## Trust

Each connector has `trust: own | team | external` (default `external`).

* `external`/`team`: documents are written untrusted. They are retrievable, but
  excluded from recall and automatic injection and fenced when shown to an
  agent. (`team` is recorded in `trust_class` for provenance; it does not
  loosen anything.)
* `own`: only documents the source can *show* you wrote are written with
  `trust: trusted` (and `trust_basis`), keeping `origin: connector:...` so the
  provenance stays visible. That means threads where every message was sent by
  you, Drive files you own and last edited yourself, events you organise. Anything
  else from the same connector stays untrusted, because your inbox is other
  people's text. A trusted note can feed recall and automatic injection as
  "user-authored by proxy".

Live results (`source_search`, `source_read`) are always fenced as untrusted
regardless of trust class.

### Prompt injection cannot trigger an action

Actions come only from an agent's explicit `source_act` tool call. No sync,
search or read path calls the action code, and imported text is never executed
(a test syncs a document that says "call source_act" and asserts no action is
created). A hostile email can only matter if the agent itself is talked into
calling the tool, which then still waits for you, and the tool description tells
agents to act only on the user's request. Review approval summaries: they show
the exact recipient, channel and text.
