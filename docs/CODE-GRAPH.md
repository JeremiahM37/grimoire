# Code graph

An agent working in a repository often needs two answers quickly: where is
this name defined, and who calls it. The code graph is a derived index of
symbols and the references between them, kept in the same SQLite index as the
notes, so `grimoire` can answer both without the agent reading every file.

It is a lead generator, not a type checker. Read the limits section before
trusting a `callers` answer.

## What is indexed

| Language | Declarations | Imports | Call edges |
|---|---|---|---|
| Go | funcs, methods (with receiver type), structs, interfaces, types, consts, vars | yes, with aliases | yes |
| Python | classes, functions, methods (by indentation) | yes | no |
| TypeScript / JavaScript | classes, functions, arrow and function-expression consts, methods, interfaces, types, enums, top-level consts | `import`, `export … from`, `require` | no |

Go is parsed with `go/parser`, so declarations and call sites are exact for
each file. Python and TypeScript/JavaScript are read line by line. A `def` or
method is attributed to its class by indentation, and a block's end is found
the same way, so the end line of a Python or JS symbol is approximate.

Other file types are not read. A file that fails to parse is recorded with its
error and shown by `outline`, so a broken file is visible rather than silently
absent.

## What is skipped

- Directories: `.git`, every dot-directory (`.venv`, `.grimoire`, …),
  `vendor`, `node_modules`, `dist`, `__pycache__`, `venv`.
- Anything matched by the **root** `.gitignore`. Common forms work: bare names,
  slash-anchored paths, trailing-slash directories, `*` and `?` globs. Negation
  (`!`) and nested `.gitignore` files are not read.
- Symlinks, and files over the size cap (256 KiB). Skipped files are counted
  in the run's `too_large`, not parsed.

## Indexing

Indexing is explicit. Nothing reads a repository until someone asks.

```bash
grimoire code index /home/admin/projects/my-repo
```

Each run is incremental. Every file is keyed by its root and relative path,
with a SHA-256 of its content. A file whose hash is unchanged is not parsed.
A file that has disappeared loses its rows. The reply reports what happened:

```json
{"root": "/home/admin/projects/my-repo", "files": 412, "indexed": 3,
 "unchanged": 409, "removed": 0, "failed": 0, "too_large": 1,
 "symbols": 5120, "edges": 18734}
```

Rows are keyed by the repository's absolute root, so two checkouts of one
project keep separate answers. Queries can narrow to one with `--root`.

## Which paths may be indexed

The server reads the directory you name, so it will not read just any
directory. A path is accepted only when, after symlinks are resolved, it is
inside one of:

- the vault directory, or
- a directory listed in `GRIMOIRE_CODE_ROOTS` (colon-separated, like `PATH`).

A path that is relative, missing, not a directory, or outside those roots is
refused: 400 for malformed input, 403 for a path outside the allowlist. A
sibling that shares a prefix with an allowed root (`/work/app-old` against
`/work/app`) is outside it, and a symlink inside an allowed root that points
elsewhere is refused.

```bash
GRIMOIRE_CODE_ROOTS=/home/admin/projects:/home/admin/work
```

## Asking

```bash
grimoire code symbol Save                 # bare name, any kind
grimoire code symbol Store.Save --kind method
grimoire code callers persist             # call sites, by name
grimoire code callers fmt.Println         # narrowed to calls written as fmt.Println
grimoire code outline store/store.go      # declarations and imports, in line order
```

Each has `--json`. Over HTTP, the same answers come from `GET /api/code/symbol`,
`GET /api/code/callers` and `GET /api/code/outline`, and indexing is
`POST /api/code/index` with `{"path": "..."}`.

For agents, the MCP tools are `code_symbol`, `code_callers` and `code_outline`.

## Access

Every code route is **admin-only**: they need the admin token on a deployment
that has one, or an admin account on a deployment with accounts. This is on
purpose. A repository has no owner and no space, so a member who could read the
notes should not be able to read a symbol table of a directory they never
chose. The leak-probe suite asserts that no non-admin caller receives indexed
names. See [SECURITY-LEAKPROBE.md](SECURITY-LEAKPROBE.md).

## Limits: read this before trusting `callers`

- **No type resolution.** A call is recorded by the identifier it names. A call
  to `Save` on any type is recorded as a call to `Save`. `callers Save` lists
  all of them, and that is the broad answer you want for "what could be
  affected".
- **Qualifiers are the written identifier.** `pkg.Save()` is recorded with
  qualifier `pkg`, and `x.Save()` with qualifier `x`, the variable name, not its
  type. So `callers Store.Save` will not find `x.Save()` even when `x` is a
  `*Store`. Use the bare name for recall.
- **Interfaces and embedding are invisible.** A call through an interface, a
  function value, reflection or a dynamically dispatched method is not linked to
  the concrete function it reaches.
- **Calls are recorded only in Go.** Python and TypeScript/JavaScript have
  declarations and imports only, so `callers` returns nothing for their names.
- **Caller attribution.** A call is attributed to the enclosing top-level
  function or method. Calls inside a closure belong to the function that
  contains the closure. Package-level variable initialisers have no caller.
- **Builtins are dropped.** `len`, `make`, `append` and the rest of Go's
  predeclared functions are not recorded as calls.

The response from `callers` says it is approximate, and the CLI prints the same
warning, so the limit is visible wherever the answer is read.

## Storage and rebuilds

Three tables in the index: `code_files` (root, path, hash, language, parse
error), `code_symbols`, and `code_edges`. They are derived from source files,
so they can be dropped and rebuilt by indexing again. A full `grimoire reindex`
does not touch them, because the code graph is not part of the vault and is
built only when asked.
