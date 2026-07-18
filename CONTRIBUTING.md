# Contributing to Grimoire

Thanks for looking under the hood. Start with
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — especially the **invariants**
section; most review feedback is one of those five rules.

## Dev setup

```bash
python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
.venv/bin/playwright install chromium          # for the e2e suite
GRIMOIRE_VAULT=/tmp/dev-vault .venv/bin/python -m server   # http://localhost:9111
```

The web client is plain ES modules — edit `web/*.js`, reload. The only build
artifact is the CM6 editor bundle; rebuild it only when changing
`tools/editor-entry.mjs` or bumping CodeMirror:

```bash
cd tools && npm install && npm run build       # → web/vendor/editor.js (check in)
```

## Before you push

```bash
.venv/bin/ruff check server/ cli/ tests/       # must be clean
.venv/bin/pytest                               # unit + api + e2e, all hermetic
verify run .verify.yaml                        # live smoke on an isolated port
```

Changed any file in the PWA shell? **Bump `CACHE` in `web/sw.js`** or clients
keep the old version.

## Style

* Python: ruff-enforced (config in `pyproject.toml`). Docstrings explain *why*,
  not *what*; module docstrings state the module's contract.
* JS: no frameworks, no build step, `esc()` everything that enters `innerHTML`.
* Tests accompany every behavior change — including a *negative* test when the
  change touches parsing, paths, or permissions.
* Security posture changes (CSP, vault, broker, private notes) also update
  `SECURITY.md`.

## Plugins

New first-party plugins live in `plugins/<name>/` and follow
[docs/PLUGINS.md](docs/PLUGINS.md). Heavy vendored assets must be lazy-loaded.
