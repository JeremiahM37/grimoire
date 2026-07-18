"""Live queries — the engine behind ```query fenced blocks.

A page can embed a fenced block like:

    ```query
    tag: project
    path: work/
    text: quarterly
    linked-to: Roadmap 2026
    pinned: true
    sort: updated desc
    limit: 10
    render: table
    columns: title, updated, tags
    ```

and the block renders as a live list/table/count of matching notes wherever the
note is displayed (editor preview, e-ink /read, HTML export).

Design rules (security first — these blocks live inside user content and are
rendered on the *unauthenticated* /read surface):

* The block is parsed into a typed `QuerySpec`; the user's text NEVER reaches
  SQL directly. Every filter becomes a parameterized clause; sort keys and
  columns are whitelisted.
* Private and encrypted notes are excluded unless the caller explicitly opts in
  (the authenticated PWA does; /read and export do not).
* Results are capped (`MAX_LIMIT`) so a hostile block can't dump the vault.
"""
from __future__ import annotations

from dataclasses import dataclass, field

from . import db

# Whitelists — the only identifiers that can ever appear in generated SQL.
SORT_FIELDS = {"title", "updated", "created", "path", "mtime"}
COLUMNS = {"title", "path", "updated", "created", "tags"}
RENDERS = {"list", "table", "count"}

DEFAULT_LIMIT = 50
MAX_LIMIT = 200


@dataclass
class QuerySpec:
    """A parsed, validated query block. All fields are optional filters."""
    tag: str | None = None
    path: str | None = None            # path prefix, e.g. "journal/"
    text: str | None = None            # FTS5 match (escaped as a quoted phrase)
    linked_to: str | None = None       # notes that link TO this title/path
    pinned: bool | None = None
    sort: str = "updated"
    sort_desc: bool = True
    limit: int = DEFAULT_LIMIT
    render: str = "list"
    columns: list[str] = field(default_factory=lambda: ["title", "updated"])
    errors: list[str] = field(default_factory=list)   # parse problems, surfaced in output


def parse(block: str) -> QuerySpec:
    """Parse the text inside a ```query fence. Forgiving: unknown keys and bad
    values are collected into `spec.errors` rather than raising — a broken block
    should render an explanation, not take the page down."""
    spec = QuerySpec()
    for raw in block.splitlines():
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        key, _, val = line.partition(":")
        key, val = key.strip().lower().replace("-", "_"), val.strip()
        if not val and key:
            spec.errors.append(f"missing value for '{key}'")
            continue
        if key == "tag":
            spec.tag = val.lstrip("#")
        elif key == "path":
            spec.path = val
        elif key == "text":
            spec.text = val
        elif key == "linked_to":
            # accept "[[Title]]", "Title" or a path
            spec.linked_to = val.strip("[]").strip()
        elif key == "pinned":
            spec.pinned = val.lower() in ("true", "yes", "1")
        elif key == "sort":
            parts = val.lower().split()
            if parts[0] not in SORT_FIELDS:
                spec.errors.append(f"unknown sort field '{parts[0]}'")
            else:
                spec.sort = parts[0]
                spec.sort_desc = (len(parts) < 2) or parts[1] != "asc"
        elif key == "limit":
            try:
                spec.limit = max(1, min(int(val), MAX_LIMIT))
            except ValueError:
                spec.errors.append(f"limit must be a number, got '{val}'")
        elif key == "render":
            if val.lower() not in RENDERS:
                spec.errors.append(f"unknown render '{val}' (use list|table|count)")
            else:
                spec.render = val.lower()
        elif key == "columns":
            cols = [c.strip().lower() for c in val.split(",") if c.strip()]
            bad = [c for c in cols if c not in COLUMNS]
            if bad:
                spec.errors.append(f"unknown column(s): {', '.join(bad)}")
            spec.columns = [c for c in cols if c in COLUMNS] or spec.columns
        else:
            spec.errors.append(f"unknown key '{key}'")
    return spec


def execute(spec: QuerySpec, include_private: bool = False) -> list[dict]:
    """Run a validated spec against the index. Returns note rows with the
    whitelisted display fields (never bodies — a query block must not become a
    read-everything gadget on the unauthenticated surfaces)."""
    where, params = [], []
    if not include_private:
        where.append("n.private = 0")
    if spec.tag is not None:
        where.append("n.path IN (SELECT note FROM tags WHERE tag = ? COLLATE NOCASE)")
        params.append(spec.tag)
    if spec.path is not None:
        where.append("n.path LIKE ? ESCAPE '\\'")
        params.append(_like_prefix(spec.path))
    if spec.pinned is not None:
        # pinned lives in frontmatter; the index stores frontmatter_json verbatim
        op = "LIKE" if spec.pinned else "NOT LIKE"
        where.append(f"n.frontmatter_json {op} '%\"pinned\": true%'")
    if spec.linked_to is not None:
        where.append(
            "n.path IN (SELECT src FROM links WHERE resolved=1 AND "
            "(dst = ? OR target = ? COLLATE NOCASE))")
        params.extend([_as_md_path(spec.linked_to), spec.linked_to])
    if spec.text is not None:
        # FTS5: pass the text as a single quoted phrase — user input can't
        # inject MATCH syntax (NEAR/OR/column filters) through it.
        phrase = '"' + spec.text.replace('"', '""') + '"'
        where.append("n.path IN (SELECT path FROM fts WHERE fts MATCH ?)")
        params.append(phrase)

    order = f"n.{spec.sort} {'DESC' if spec.sort_desc else 'ASC'}"
    sql = ("SELECT n.path, n.title, n.updated, n.created FROM notes n "
           + ("WHERE " + " AND ".join(where) if where else "")
           + f" ORDER BY {order} LIMIT ?")
    params.append(spec.limit)
    rows = db.query(sql, tuple(params))
    if "tags" in spec.columns or spec.render == "table":
        for r in rows:
            r["tags"] = [t["tag"] for t in
                         db.query("SELECT tag FROM tags WHERE note=?", (r["path"],))]
    return rows


def run(block: str, include_private: bool = False) -> dict:
    """Parse + execute in one step. The shape consumed by renderers and the
    /api/query endpoint: {spec, rows, errors}."""
    spec = parse(block)
    rows = execute(spec, include_private=include_private) if not spec.errors else []
    return {
        "render": spec.render,
        "columns": spec.columns,
        "rows": rows,
        "count": len(rows),
        "errors": spec.errors,
    }


def _like_prefix(prefix: str) -> str:
    """Escape LIKE metacharacters so a path prefix is matched literally."""
    escaped = prefix.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
    return escaped + "%"


def _as_md_path(title_or_path: str) -> str:
    return title_or_path if title_or_path.endswith(".md") else title_or_path + ".md"
