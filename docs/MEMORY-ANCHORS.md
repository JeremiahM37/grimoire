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

## Security rule

Anchors are accepted only from a loopback peer that is not a remote or OAuth
caller. A probe result leaks a little about files on the server, so a remote
caller's anchors are ignored and the response says `"anchors_ignored":"remote caller"`.
The MCP HTTP transport always counts as remote; only the local stdio agent can
attach anchors.

## Writing

`remember` takes `anchors` (a list of probe objects) and `event` (a boolean).
Each anchor is validated and run once. Only anchors that pass, or that are
`remote`, are stored. The response lists every anchor with its result, so a
writer can fix one that failed. A failing anchor does not fail the write.

## Commands

- `grimoire memory anchors [--json]` runs every stored anchor and lists the
  claims whose probe no longer holds. Admin only (`GET /api/memory/anchors`).
- `grimoire memory refs [--json]` scans every note for absolute paths under `~`,
  `/home/admin` and `/mnt/bulk`, and for `*.service` and `*.timer` names, and
  lists the references that no longer exist. Admin only (`GET /api/memory/refs`).

Neither command prints fact text.

## The flag

Recall gates on anchors only behind the experiment flag `anchor`
(`GRIMOIRE_EXP=anchor`). A fact with a failing anchor gets
`freshness.action = "verify"`, with a reason naming the anchor type. A fact whose
anchors all hold gets `action = "use"`. Each fact's probe verdict is cached for
10 minutes. With the flag off, recall output does not change.

Code: `go/internal/anchor` (probes, encoding, reference scan), `go/internal/api/memory_anchors.go`.
