# Contributing to Grimoire

Thanks for looking under the hood. Start with
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) — especially the **invariants**
section; most review feedback is one of those five rules.

## Dev setup

```bash
cd go && go build -o grimoire ./cmd/grimoire && cd ..
GRIMOIRE_VAULT=/tmp/dev-vault GRIMOIRE_WEB_DIR=./web ./go/grimoire   # localhost:9111

python3 -m venv .venv && .venv/bin/pip install -r requirements.txt
.venv/bin/playwright install chromium          # for the e2e suite only
```

The product is Go; Python is only the test and benchmark harness.

The web client is plain ES modules — edit `web/*.js`, reload. The only build
artifact is the CM6 editor bundle; rebuild it only when changing
`tools/editor-entry.mjs` or bumping CodeMirror:

```bash
cd tools && npm install && npm run build       # → web/vendor/editor.js (check in)
```

## Before you push

```bash
cd go && gofmt -l . && go vet ./... && go test ./...   # must be clean
.venv/bin/ruff check tests/ benchmarks/        # must be clean
cd go && go build -o grimoire ./cmd/grimoire   # the e2e suite runs this binary
.venv/bin/pytest tests/e2e                     # real-browser flows
verify run .verify.yaml                        # live smoke on an isolated port
```

Changed any file in the PWA shell? **Bump `CACHE` in `web/sw.js`** or clients
keep the old version.

## Style

* Go: `gofmt` + `go vet` clean. Comments explain *why*, not *what*; package
  docs state the package's contract.
* Python (tests and benchmarks only): ruff-enforced, config in `pyproject.toml`.
* JS: no frameworks, no build step, `esc()` everything that enters `innerHTML`.
* Tests accompany every behavior change — including a *negative* test when the
  change touches parsing, paths, or permissions.
* Security posture changes (CSP, vault, broker, private notes) also update
  `SECURITY.md`.

## Plugins

New first-party plugins live in `plugins/<name>/` and follow
[docs/PLUGINS.md](docs/PLUGINS.md). Heavy vendored assets must be lazy-loaded.
