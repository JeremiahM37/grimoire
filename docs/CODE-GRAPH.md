# Code graph

An agent working in a repository often needs two answers quickly: where is
this name defined, and who calls it. The code graph is a derived index of
symbols and the references between them, kept in the same SQLite index as the
notes, so `grimoire` can answer both without the agent reading every file.

It is not a full type checker. Go calls are resolved with `go/types` where the
checker can tell, and Python and TypeScript/JavaScript calls by scope and the
file's imports. Every call says how sure it is. Read the limits section before
trusting a `callers` answer.

## What is indexed

| Language | Declarations | Imports | Call edges |
|---|---|---|---|
| Go | funcs, methods (with receiver type), structs, interfaces, types, consts, vars | yes, with aliases | yes, resolved by type where possible |
| Python | classes, functions, methods (by indentation) | yes, with `as` aliases and `from … import` names | yes, by scope and imports (heuristic) |
| TypeScript / JavaScript | classes, functions, arrow and function-expression consts, methods, interfaces, types, enums, top-level consts | `import`, `export … from`, `require` | yes, by brace scope and imports (heuristic) |

Go is parsed with `go/parser`, so declarations are exact. A `def` or method is
attributed to its class by indentation, and a block's end is found the same way,
so the end line of a Python or JS symbol is approximate. Python and JS calls are
attributed by the innermost enclosing def or function, using indentation for
Python and braces for TS/JS.

## Call resolution and confidence

Every call edge carries a `confidence`, and a `target` when one was found.

- **resolved**: the target is the declaration the call reaches, and the reason
  cannot be wrong about which declaration it is.
- **heuristic**: the target is the likely one, from a name in the same file or
  an import. Shadowing, or dispatch through an interface or a dynamic value,
  could make it another.
- **unresolved**: no target was established. The name and the written qualifier
  are kept, so a name query still finds the call. For a call into another
  package the target is `importpath.Name` (or `importpath.Type.Name` for a
  method), which names the package but is not a repository declaration.

**Go, by type.** Each package directory is type-checked with `go/types` as one
package, tolerantly: imports are answered by a stub that returns an empty
package, so anything from outside the directory is undefined to the checker and
the checker carries on past those errors. A call is resolved when the checker
binds it to a function or method declared in the same package, including one
promoted from an embedded struct of the same package. That gives
`resolved` for `Save()` and `x.Save()` in the same package, and `heuristic` for
a call through an interface, where the target is the interface method, not an
implementation. A call on a value whose type the checker cannot infer is
`unresolved`.

Go targets are written `<package directory>#<Qualified>`: `store#Save`,
`internal/codegraph#Store.Save`. The root package is `#Save`. Two packages with
a function of the same name therefore have different targets, which the name
alone could not tell apart.

Bounds: a package over 400 files, or one whose check runs past 10 seconds, is not
type-checked. Its calls are recorded by name and marked `unresolved`. A timed-out
check keeps running in the background until it finishes, which the file cap
keeps bounded. No code is executed, and the go tool is never run.

**Python and TS/JS, by scope.** A call belongs to the innermost def, or the
innermost brace-delimited function, and a call inside a string or a comment is
not an edge. Targets are `<file>#<Qualified>` for the file's own declarations:

- `self.m()` or `cls.m()` in a method, and `this.m()` in a class method, are
  `resolved` when the class defines `m` in this file.
- A bare name that this file defines is `heuristic`, as is a name bound by an
  import: `from m import f` gives `m.f`, `import m as x` with `x.g()` gives
  `m.g`, and `import x from 'm'` with `x()` gives `m`.
- Anything else is `unresolved`.

Python triple-quoted strings, JS template literals and block comments are
blanked across lines, so a docstring that shows an example call is not an edge.

## Asking

```bash
grimoire code symbol Save                 # bare name, any kind
grimoire code symbol Store.Save --kind method
grimoire code callers persist             # every call to something called persist
grimoire code callers Store.Save          # calls written as Store.Save, or resolved to it
grimoire code callers store#Save          # only the calls resolved to store's Save
grimoire code outline store/store.go      # declarations and imports, in line order
```

Each has `--json`. The callers output shows each call's confidence. Over HTTP,
the same answers come from `GET /api/code/symbol`, `GET /api/code/callers` and
`GET /api/code/outline`, and indexing is `POST /api/code/index` with
`{"path": "..."}`.

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
Use the `#` form when a name is ambiguous and the answer has to be exact.

## Access

Every code route is **admin-only**: they need the admin token on a deployment
that has one, or an admin account on a deployment with accounts. This is on
purpose. A repository has no owner and no space, so a member who could read the
notes should not be able to read a symbol table of a directory they never
chose. The leak-probe suite asserts that no non-admin caller receives indexed
names. See [SECURITY-LEAKPROBE.md](SECURITY-LEAKPROBE.md).

## Limits: read this before trusting `callers`

- **A bare name is broad.** `callers Save` lists every call to anything called
  `Save`, whatever it resolved to. That is the answer for "what could be
  affected". Use the `pkg#Name` form when the answer must be one declaration.
- **Interfaces, function values and reflection are not followed.** A call through
  an interface resolves to the interface method, marked `heuristic`, and not to
  the implementations. A call through a function value or a dynamic dispatch is
  `unresolved`.
- **Types from outside the directory are unknown in Go.** A method on a value
  whose type lives in another package (`testing.T`, `*sql.Rows`, a field of
  another package's struct) has no target. It stays `unresolved` with its name,
  so `callers Fatal` still finds it. Cross-package methods are not resolved to
  their declarations; only their package path is known for package-level
  functions.
- **Python and JS are syntactic.** A variable's type is not inferred, so
  `obj.m()` resolves only through `self`/`this`, a class named in the file, or an
  import. A call that spans lines, or a call inside a JS template literal's
  `${…}` interpolation, is not recorded.
- **Two test packages in one directory are resolved apart.** `x` and `x_test`
  are type-checked as separate packages, so `x_test` calls into `x` are not
  resolved to its declarations.
- **Caller attribution.** A Go call is attributed to the enclosing top-level
  function or method, and calls inside a closure belong to the function that
  contains the closure. Package-level variable initialisers have no caller.
- **Builtins are dropped.** Go's predeclared functions are not recorded, unless
  the package declares its own with the same name. Python and JS builtins are
  dropped only when the file does not define or import the same name.

The `callers` response says it is approximate, and the CLI prints each call's
confidence, so the limit is visible wherever the answer is read.

## Measured here, and what is not

Measured on this repository's `go/` tree (677 files, 8,886 symbols):

| | edges | share |
|---|---|---|
| resolved | 19,838 | 36% |
| heuristic | 549 | 1% |
| unresolved | 34,172 | 63% |

Most unresolved calls are to the standard library and other external packages,
which the stub cannot see. Those carry an `importpath.Name` target and are
correct but unresolved, so the share is a measure of the boundary, not of errors.
Indexing the tree from cold took about 1.0 s.

Precision check: 20 call edges were drawn from the sorted edge list at a fixed
stride and checked against their source line. Of the 9 with a repository target
(8 resolved, 1 heuristic), none had a wrong target on reading the call and the
receiver's type. Of the 11 unresolved, 7 named the right external package and 4
had no target because the receiver was an external type. That is 20 samples, which
does not give a precision rate with a meaningful interval. The declarations were
not re-read for each edge, and the sampling script was not kept.

Designed, not measured: Python and JS precision, because no hand check was run on
them beyond the fixture tests in `calls_test.go`.

## Storage and rebuilds

Three tables in the index: `code_files` (root, path, hash, language, parse
error), `code_symbols`, and `code_edges`. `code_edges` carries the call's
`target` and `confidence` as well. They are derived from source files, so they
can be dropped and rebuilt by indexing again. A full `grimoire reindex` does not
touch them, because the code graph is not part of the vault and is built only
when asked.

Incremental indexing skips a file whose hash is unchanged, with one exception.
Go files in a package directory are re-resolved together: when any file in the
directory changes, or one is removed, every Go file there is written again. So
`Stats.indexed` counts the sibling files as well. The first run after upgrading
re-parses everything once, because call edges written before they carried a
confidence are treated as stale.
