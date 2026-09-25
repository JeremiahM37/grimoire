# Fresh installs and remote access

## Personal local install

`install.sh` installs into `~/.local` without asking for sudo. Set
`GRIMOIRE_INSTALL_PREFIX=/usr/local` for a system-wide installation. Add the
printed bin directory to PATH, then run:

```sh
GRIMOIRE_VAULT="$HOME/notes" grimoire serve
```

The native server binds to localhost by default. The archive includes its
console and plugins; the source build finds `frontend/dist` after
`npm ci --prefix frontend && npm run build --prefix frontend`.
`go install` supplies executable files only: use a release archive or source
build if you also want the browser console.

Install Python-free binaries on Linux, macOS or Windows. PDF text extraction
additionally needs `pdftotext` (Debian/Ubuntu: `poppler-utils`; macOS: `poppler`).
Without an LLM provider, editing, keyword search, local embeddings and MCP
retrieval still work. Generative answers need a configured model provider.

The installer verifies checksums and validates the extracted binaries/console
before replacing an installation. Previous files remain in
`share/grimoire.previous.<id>` for rollback. Your vault is separate from the
installation and must not be stored inside its program directory.

## Docker

```sh
docker compose up -d --build
```

Open http://localhost:9111. Compose uses a named volume owned by the container
user, so first startup needs no manual permission repair. Notes and all
`.grimoire` state survive container recreation. Back up the volume; `down -v`
deletes it. The image includes the local embedding model and PDF extraction.

To use an existing host notes folder, replace the volume with an absolute bind
mount. The container runs as UID 1000: ensure that user can write the folder,
or set Compose `user: "YOUR_UID:YOUR_GID"`. Do not recursively change ownership
of an existing vault just to match the example. Give the selected user a
writable cache if you customize its HOME/XDG_CACHE_HOME.

## Remote browser and MCP

For occasional access, keep the listener private and forward it:

```sh
ssh -L 9111:127.0.0.1:9111 your-server
```

For a private-network deployment, explicitly set the bind address and token:

```sh
export GRIMOIRE_AUTH_TOKEN="$(openssl rand -hex 32)"
# Native server:
GRIMOIRE_HOST=0.0.0.0 GRIMOIRE_VAULT="$HOME/notes" grimoire serve
# Docker Compose instead:
GRIMOIRE_BIND=0.0.0.0 docker compose up -d
```

Keep that token in a private environment file (mode 600) for subsequent
restarts. Use HTTPS through your private reverse proxy for remote access.
Opening the browser now shows a token-entry form; successful sign-in uses an
HttpOnly cookie without putting the token in a URL. The health endpoint remains
public for container/proxy checks. `GRIMOIRE_ADMIN_TOKEN`, if configured, is a
separate protection for administrative operations.

On the agent's machine, install `grimoire-mcp` and configure its environment:

```sh
GRIMOIRE_URL=https://notes.your-private-domain.example \
GRIMOIRE_AUTH_TOKEN=your-token grimoire-mcp
```

This is a client of the remote server; it does not need a copy of the vault.
The separate HTTP MCP transport also requires `GRIMOIRE_MCP_TOKEN` when exposed
beyond loopback. Do not confuse that transport credential with the API token.

## Linux service

Install the release system-wide, then copy `deploy/grimoire.service` into
`/etc/systemd/system` and enable it. The example uses systemd's DynamicUser,
StateDirectory and CacheDirectory to provision its own writable storage; it
needs neither a manually created account nor `/opt/grimoire`.

**This example creates a new vault at `/var/lib/grimoire/vault`.** To serve an
existing personal vault instead, use a service running as its existing owner
and set `GRIMOIRE_VAULT` explicitly. Do not replace an existing unit blindly.
Existing network deployments upgrading from an all-interface default must set
`GRIMOIRE_HOST=0.0.0.0` explicitly (and retain their authentication settings).

## Repeat acceptance tests

`tools/test-first-install.py` tests a real installed prefix or Docker image:
password entry, desktop/mobile note edits and reload, API search, MCP read,
and note/settings persistence after restart or container recreation. It uses
only generated tokens and disposable vaults, with no paid model calls.
`tools/test-install-script.py <archive>` checks the actual shell installer,
including spaces in HOME, repeat installation and failed upgrades.
