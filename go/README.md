# go/ — the implementation

One module, two binaries:

| binary | what it is |
|---|---|
| `cmd/grimoire` | the HTTP server **and** the CLI (no subcommand = serve) |
| `cmd/grimoire-mcp` | the MCP server agents mount; proxies this API over stdio or HTTP |

```bash
go build -o grimoire ./cmd/grimoire && go build -o grimoire-mcp ./cmd/grimoire-mcp
go test ./...
```

Package responsibilities are listed in [../docs/ARCHITECTURE.md](../docs/ARCHITECTURE.md),
along with the five invariants that most review feedback comes back to.

## "Port of server/*.py"

Many package docs open with a line like *"Port of `server/index.py`"*. That file
is **not in the tree** — Grimoire was originally written in Python, and that
implementation was deleted on 2026-08-14 once this one matched it. The reference
is kept because it explains code that would otherwise look strange: several
places reproduce a Python behaviour exactly rather than improving on it, and
knowing there *was* an original is the difference between "this is odd" and
"this is odd on purpose".

Two things survive that history and still bind:

- **`compat/fixtures/*.json`** — frozen output from the original (crypto keys and
  sealed tokens, note parsing, rendered HTML, CRDT documents, embeddings and
  token ids, path confinement), replayed by `internal/compat` on every `go test`.
  The generator went with the implementation that produced it, so a failing
  fixture means **this** build changed, not that the fixture is stale.
- **`git log`** — the Python tree is in history if you need to read it.

## Duplication that is deliberate

`internal/render` and `web/markdown.js` are two implementations of one markdown
renderer, and they must stay in lockstep: the server renders `/read` and the
HTML export, while the client renders previews, slides and hover cards with no
network. Neither can be deleted, so each names the other in its header — a rule
added there must land in both.

That is the only forced duplication. Everything else with more than one caller
is shared:

- `internal/embed.ChunkText` — indexing and search excerpting chunk identically
- `internal/index.Retrieve` — the one retrieval path; `/api/retrieve`, `/api/ask`,
  memory recall and the MCP tools all enter here
- `internal/fts` — every FTS `MATCH` expression, so the "user input is never
  syntax" invariant has one implementation instead of three
- the CLI's `search` and `export` run the server's own HTTP handlers in-process,
  so a terminal search cannot drift from the API's
