# Web connectors: claude.ai and ChatGPT over OAuth 2.1

`grimoire-mcp`'s Streamable-HTTP transport (`/mcp`) can be added as a **custom
connector** in claude.ai (web/mobile) and as a **developer-mode connector** in
ChatGPT. Both vendors' connector UIs run from the vendor's own cloud
infrastructure, not from your browser, and both support exactly one
credential mechanism for a server they don't already know: **OAuth 2.1 with
PKCE and Dynamic Client Registration.** Neither can send a static bearer
header — so `GRIMOIRE_MCP_TOKEN` (still supported, unchanged, for local
clients) does not cover this case, and this document is the other one.

## Facts this design relies on, with sources

- **MCP authorization spec.** The 2025-06-18 revision requires an MCP server
  to publish [RFC 9728](https://datatracker.ietf.org/doc/html/rfc9728)
  Protected Resource Metadata, requires PKCE, and lists DCR
  ([RFC 7591](https://datatracker.ietf.org/doc/html/rfc7591)) as a SHOULD.
  <https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization>
- **The 2025-11-25 revision** downgrades DCR from SHOULD to MAY in favour of
  **OAuth Client ID Metadata Documents** (an HTTPS URL as `client_id`,
  [draft-ietf-oauth-client-id-metadata-document-00](https://datatracker.ietf.org/doc/html/draft-ietf-oauth-client-id-metadata-document-00)),
  and allows OpenID Connect Discovery as an alternative to
  [RFC 8414](https://datatracker.ietf.org/doc/html/rfc8414) authorization-server
  metadata. <https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization>.
  **This server still implements DCR**, not CIMD: as of this writing neither
  vendor's connector flow (see below) has published CIMD support, DCR is
  still explicitly supported by both, and DCR is what the task this feature
  shipped for actually asked for. If a future release needs CIMD, add it
  alongside DCR rather than instead of it — nothing here needs to change to
  add a second registration path.
- **Streamable HTTP transport requirements** — single `/mcp` endpoint, POST
  required, GET must answer either an SSE stream or `405 Method Not Allowed`
  with `Allow: POST`, a JSON-RPC *notification*/*response* POST must get
  `202 Accepted` with no body, `MCP-Protocol-Version` on later requests, and
  `Origin` validation / loopback binding as DNS-rebinding protection:
  <https://modelcontextprotocol.io/specification/2025-06-18/basic/transports>.
  `internal/mcp/http.go` already met the POST/405/202 requirements before this
  feature; the protocol-version check is new (`supportedProtocolVersions`).
  This server does not implement the optional GET/SSE server-push stream or
  `Mcp-Session-Id` — both are MAY/SHOULD in the spec, not required, and this
  transport has always been a stateless one-response-per-POST server; neither
  vendor's connector requires them to work.
- **RFC 8707** (Resource Indicators) — the `resource` parameter binds a token
  to the one MCP server it may be used against. Enforced here by rejecting
  any `resource` that isn't exactly `GRIMOIRE_PUBLIC_BASE/mcp`, and by
  checking it again on every `/mcp` request against the token's stored
  `resource` column.
- **Exact redirect URIs.** Anthropic's and OpenAI's own docs (checked:
  <https://support.claude.com/en/articles/11175166-about-custom-connectors-using-remote-mcp>,
  <https://platform.claude.com/docs/en/agents-and-tools/remote-mcp-servers>,
  <https://developers.openai.com/api/docs/mcp>) do **not** publish a stable
  callback path for either vendor's web-connector OAuth flow — community
  write-ups quote `https://claude.ai/api/mcp/auth_callback`, but nothing
  Anthropic or OpenAI maintains commits to it, and OpenAI's own docs describe
  DCR ("Dynamic client registration remains supported when configured") and
  recommend CIMD without stating a fixed redirect. Since DCR hands the
  server the client's real `redirect_uris` at registration time, this server
  does not need to guess a path — see `internal/oauth/redirect.go`'s
  `RedirectAllowlist`, which bounds registration by **host** (`claude.ai`,
  `*.claude.ai`, `claude.com`, `*.claude.com`, `chatgpt.com`,
  `*.chatgpt.com`, `*.openai.com`, plus `localhost`/`127.0.0.1` on any port
  for MCP Inspector) and lets each client register its own exact path.
  `/oauth/authorize` then holds every client to the *exact* URI it
  registered — the host allowlist is the outer gate, exact match is the
  inner one.
- **claude.ai's public-reachability requirement** — its docs state the MCP
  server "must be reachable over the public internet from Anthropic's cloud
  infrastructure." That's the public listener below.
- **OAuth 2.1 core rules** applied throughout: PKCE required with `S256`
  only (no `plain`); redirect URIs must be `https` or `localhost`;
  authorization codes are single-use and short-lived (2 minutes); refresh
  tokens rotate on every use.

## The design: two listeners, one owner

```
                     ┌─────────────────────────────┐
 claude.ai / ChatGPT │  PUBLIC  (behind a tunnel)   │
 cloud backend  ────▶│  GRIMOIRE_PUBLIC_BASE        │
                      │  /mcp                        │
                      │  /.well-known/oauth-*        │
                      │  /oauth/register              │
                      │  /oauth/token, /oauth/revoke  │
                      └───────────────┬───────────────┘
                                      │ authorization_endpoint
                                      │ points here instead
                                      ▼
                      ┌───────────────────────────────┐
 owner's own browser  │  PRIVATE (tailnet/loopback)    │
 (redirected here) ──▶│  GRIMOIRE_OAUTH_AUTHORIZE_BASE │
                      │  /oauth/authorize               │
                      │  /admin/oauth (client console)  │
                      └───────────────────────────────┘
```

**Why this works even though the client is "public":** nothing in OAuth
requires the authorization endpoint — the one page a *human's browser* has to
load — to be reachable from the same place the client's backend is. The
Protected Resource Metadata and Authorization Server Metadata documents
(served on the public side) simply **name** a different host for
`authorization_endpoint`. claude.ai/ChatGPT's backend calls `/oauth/register`
and `/oauth/token` on the public side; your own browser is redirected to the
private side to approve. The public listener never registers
`/oauth/authorize` at all (see `oauth.Handler.RegisterPublic` /
`RegisterPrivate` in `internal/oauth/server.go`) — asking for it there is an
ordinary 404, not a checked-and-refused request, so there's no path that
could regress into exposing it.

Nobody but the vault's owner can ever approve a connection: the consent page
identifies the caller via the existing Tailscale identity backend
(`internal/identity`, `whois` against `tailscaled`'s LocalAPI), checked
against `GRIMOIRE_OAUTH_ALLOWED_LOGINS`, and falls back to the Grimoire admin
token typed into the page when Tailscale identity is unavailable or the
caller isn't on the allowlist.

## Connecting claude.ai

1. Deploy the public listener behind your tunnel (Cloudflare, etc.) at
   `GRIMOIRE_PUBLIC_BASE`, and the private listener reachable only to you —
   this estate uses `tailscale serve` on a second HTTPS port, the same
   pattern already used for Lectern:
   ```bash
   tailscale serve --bg --https=8444 http://127.0.0.1:9115
   ```
   giving `GRIMOIRE_OAUTH_AUTHORIZE_BASE=https://<host>.<tailnet>.ts.net:8444`.
2. In claude.ai: **Customize → Connectors → Add custom connector**, enter
   `https://<GRIMOIRE_PUBLIC_BASE>/mcp`.
3. claude.ai's backend discovers OAuth via the 401 (`WWW-Authenticate:
   Bearer resource_metadata=...`) or the well-known URI, registers itself via
   DCR, and opens the authorization URL in **your** browser — which lands on
   the private listener.
4. Approve on the consent page (Tailscale identifies you automatically if
   `GRIMOIRE_IDENTITY=tailscale` and your login is in
   `GRIMOIRE_OAUTH_ALLOWED_LOGINS`; otherwise enter the admin token). Leave
   **Credential broker** unchecked unless this connector specifically needs
   it — it's off by default.
5. claude.ai exchanges the code for a token and starts calling `/mcp`.

## Connecting ChatGPT (developer mode)

1. Same public/private deployment as above.
2. In ChatGPT: enable developer mode, add a connector with the same
   `https://<GRIMOIRE_PUBLIC_BASE>/mcp` URL.
3. Same DCR → redirect-to-private-listener → consent → token flow.

## Testing with MCP Inspector

`http://localhost` and `http://127.0.0.1` (any port) are in the default
redirect allowlist specifically for this. Point Inspector at
`https://<GRIMOIRE_PUBLIC_BASE>/mcp`, "Quick OAuth Flow," and the browser tab
it opens will hit your private listener the same way claude.ai's does.

## Configuration

| Variable | Default | What it does |
|---|---|---|
| `GRIMOIRE_PUBLIC_BASE` | *(unset = OAuth off)* | Public HTTPS base URL reachable from claude.ai/ChatGPT's cloud. The protected MCP resource is `GRIMOIRE_PUBLIC_BASE/mcp`. Required together with the next variable to enable OAuth at all |
| `GRIMOIRE_OAUTH_AUTHORIZE_BASE` | *(unset = OAuth off)* | Base URL of the **private**, owner-only consent page — a tailnet or loopback address, never the public one |
| `GRIMOIRE_OAUTH_AUTHORIZE_ADDR` | `127.0.0.1:9115` | Bind address for the private listener process (`cmd/grimoire-mcp` runs it as a second `http.Server`) |
| `GRIMOIRE_OAUTH_DB` | `~/.grimoire-mcp/oauth.db` | SQLite store for registered clients and issued tokens (hashed at rest) |
| `GRIMOIRE_OAUTH_ALLOWED_LOGINS` | *(empty)* | Comma-separated Tailscale `LoginName`s (or `zerotier:...`/backend-qualified subjects) trusted to approve a connection without typing the admin token. Empty means every approval goes through the admin-token fallback |
| `GRIMOIRE_OAUTH_ALLOWED_REDIRECTS` | claude.ai/claude.com/chatgpt.com/openai.com + localhost | Comma-separated `scheme://host` patterns (`*.` prefix for subdomains) DCR may register a `redirect_uri` on. **Replaces**, does not merge with, the default |
| `GRIMOIRE_IDENTITY` | *(unset)* | Reused from `internal/identity` — set to `tailscale` to enable the pre-verified-owner fast path on the consent page |
| `GRIMOIRE_ADMIN_TOKEN` | *(unset)* | Reused: gates the consent page's fallback login and the `/admin/oauth` connected-clients console the same way it gates every other admin surface |

Every other MCP transport variable (`GRIMOIRE_MCP_TRANSPORT`,
`GRIMOIRE_MCP_ADDR`/`_PORT`, `GRIMOIRE_MCP_TOKEN`) is unchanged — see
`docs/CONFIG.md`.

## Scopes

| Scope | Covers |
|---|---|
| `notes:read` | Search, read, browse notes/documents/the knowledge graph, web search |
| `notes:write` | Create/edit notes, capture, import/refresh documents |
| `memory` | `remember`/`recall`/`forget` and the rest of the agent-memory surface |
| `credentials` | `list_grants`, `use_credential`, `request_credential`, `check_credential_request` — the credential broker |

A web connector's default grant is `notes:read notes:write memory` —
**`credentials` is never granted unless the owner explicitly ticks it** on
the consent page, whatever the client requested. Tool visibility
(`tools/list`) and tool calls (`tools/call`) are both filtered to the
token's granted scopes; calling an out-of-scope tool gets `403` with
`WWW-Authenticate: Bearer error="insufficient_scope", scope="..."`.

## Managing connections

`GRIMOIRE_OAUTH_AUTHORIZE_BASE/admin/oauth` lists every registered client,
its granted scopes, and when it was last used, with a **Revoke** button that
deletes every access and refresh token issued to that client (the client
registration itself is left alone, so a reconnection lands back on the
consent page rather than a broken "unknown client" error). Gated by
`GRIMOIRE_ADMIN_TOKEN`, entered once and kept in the browser's
`sessionStorage`.

## Security model

- **Tokens are opaque, random, and stored hashed** (SHA-256) — the same
  idiom `internal/auth`'s sessions and API keys already use. An access token
  lives 1 hour; a refresh token lives 30 days and **rotates on every use**
  (the old one is invalidated the moment a new one is issued).
- **PKCE is mandatory, `S256` only.** `plain` is refused outright, not
  merely de-prioritised.
- **Every token is bound to one resource** — this server's `/mcp` — checked
  both at issuance and on every request; a token minted here cannot be
  replayed against a different MCP server, and a token minted elsewhere
  (there is nowhere else, but defensively) is refused.
- **The static `GRIMOIRE_MCP_TOKEN` bearer keeps working unchanged**, and is
  unrestricted (full tool access) exactly as before — OAuth is additive, not
  a replacement, for local clients that can hold a real secret.
- **The consent page is the one thing standing between DCR (open to
  anyone) and a live token.** DCR itself has no access control by design —
  that's how a client with no prior relationship to this server can use it
  at all — so every registered client still needs a human, on the private
  listener, to approve it per connection. See `internal/oauth`'s package doc
  for the full reasoning.
- **Public listener 404s `/oauth/authorize` unconditionally** — it is never
  registered on that mux, so there is no code path to misconfigure into
  exposing it.
