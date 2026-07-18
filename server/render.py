"""Server-side markdown → safe HTML. Mirrors the PWA's client renderer so the
e-ink surface and HTML export look the same. Escapes first, then applies a
small, well-scoped set of rules. Used by routers/read.py and the export
endpoint.

Extended markdown supported here (kept in sync with the client renderer):
headings (with stable anchor ids), lists, tasks, tables, callouts, fenced code
(with dependency-free highlighting), blockquotes, hr, ==highlight==, ~~strike~~,
footnotes ([^id] / [^id]: definition), wiki-links incl. [[note#heading]],
![[image]] embeds, ![[note]] transclusion (depth-limited, cycle-safe) and
```query live-query blocks.

Rendering is configured through a `RenderContext` so callers opt in to the
capabilities they can safely provide (e.g. /read passes include_private=False
and its own link style; export inlines images as data: URIs).
"""
from __future__ import annotations

import html
import re
from collections.abc import Callable
from dataclasses import dataclass, field

_WIKILINK = re.compile(r"\[\[([^\[\]|]+?)(?:\|([^\[\]]+))?\]\]")
_EMBED = re.compile(r"!\[\[([^\[\]|]+?)\]\]")
_HEADING = re.compile(r"^(#{1,6})\s+(.+)$")
_TASK = re.compile(r"^\s*[-*]\s+\[([ xX])\]\s+(.*)$")
_ULI = re.compile(r"^\s*[-*]\s+(.*)$")
_OLI = re.compile(r"^\s*\d+\.\s+(.*)$")
_FOOTNOTE_DEF = re.compile(r"^\[\^([\w-]+)\]:\s+(.*)$")
_FOOTNOTE_REF = re.compile(r"\[\^([\w-]+)\]")
_IMAGE_EXTS = (".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".avif")

# Transclusion depth: an embedded note may embed another, once. Prevents both
# runaway nesting and infinite cycles (a cycle also trips the seen-set check).
MAX_EMBED_DEPTH = 2


@dataclass
class RenderContext:
    """Everything a render pass may need. All capabilities are optional —
    a bare context renders plain markdown with unresolved links."""
    link_map: dict = field(default_factory=dict)      # lower title/stem -> rel path
    img_src: Callable[[str], str] = lambda rel: "/api/file/" + rel
    note_body: Callable[[str], str | None] | None = None   # rel path -> body
    run_query: Callable[[str], dict] | None = None            # query block -> result
    link_href: Callable[[str], str] = lambda rel: "/read/" + _strip_md(rel)
    depth: int = 0                                    # current transclusion depth
    _embedding: set = field(default_factory=set)      # cycle guard (rel paths)


def render(body: str, link_map: dict | None = None,
           img_src: Callable[[str], str] | None = None,
           ctx: RenderContext | None = None) -> str:
    """Render markdown to safe HTML.

    Accepts either a full `ctx` or the legacy (link_map, img_src) pair; the
    legacy form builds a bare context so existing callers keep working.
    """
    if ctx is None:
        ctx = RenderContext(link_map=link_map or {})
        if img_src is not None:
            ctx.img_src = img_src
    elif link_map is not None:
        ctx.link_map = link_map

    footnotes = _collect_footnotes(body)
    lines = body.split("\n")
    out: list[str] = []
    list_stack: list[str] = []   # 'ul' | 'ol'

    def close_lists():
        while list_stack:
            out.append(f"</{list_stack.pop()}>")

    i = 0
    while i < len(lines):
        raw = lines[i]
        # footnote definitions are rendered once, at the end
        if _FOOTNOTE_DEF.match(raw):
            i += 1
            continue
        # fenced block — code, or a live ```query block
        if raw.strip().startswith("```"):
            close_lists()
            lang = raw.strip()[3:].strip()
            j = i + 1
            buf = []
            while j < len(lines) and not lines[j].strip().startswith("```"):
                buf.append(lines[j]); j += 1
            if lang == "query" and ctx.run_query is not None:
                out.append(_query_html("\n".join(buf), ctx))
            else:
                cls = f' class="lang-{html.escape(lang)}"' if lang else ""
                out.append(f"<pre><code{cls}>"
                           f"{highlight_code(chr(10).join(buf), lang)}</code></pre>")
            i = j + 1
            continue
        # a callout: > [!type] title  followed by more > lines
        cm = re.match(r"^\s*>\s*\[!(\w+)\]\s*(.*)$", raw)
        if cm:
            close_lists()
            kind = cm.group(1).lower()
            title = cm.group(2).strip() or kind.capitalize()
            j = i + 1
            quoted = []
            while j < len(lines) and re.match(r"^\s*>", lines[j]):
                quoted.append(re.sub(r"^\s*>\s?", "", lines[j])); j += 1
            inner = render("\n".join(quoted), ctx=ctx) if quoted else ""
            out.append(f'<div class="callout callout-{html.escape(kind)}">'
                       f'<div class="callout-title">{_inline(title, ctx)}</div>'
                       f'<div class="callout-body">{inner}</div></div>')
            i = j
            continue
        # a table: a header row followed by a |---|---| separator
        if _is_table_row(raw) and i + 1 < len(lines) and _is_table_sep(lines[i + 1]):
            close_lists()
            j = i + 2
            body_rows = []
            while j < len(lines) and _is_table_row(lines[j]):
                body_rows.append(lines[j]); j += 1
            out.append(_table_html(raw, body_rows, ctx))
            i = j
            continue
        # a whole-line ![[Note]] → block-level transclusion (images stay inline)
        em = _EMBED.fullmatch(raw.strip())
        if em and not _is_image(em.group(1)) and ctx.note_body is not None:
            close_lists()
            out.append(_transclude(em.group(1).strip(), ctx))
            i += 1
            continue
        h = _HEADING.match(raw)
        if h:
            close_lists()
            lvl = len(h.group(1))
            text = h.group(2)
            out.append(f'<h{lvl} id="{heading_id(text)}">{_inline(text, ctx)}</h{lvl}>')
        elif _TASK.match(raw):
            task = _TASK.match(raw)
            if not list_stack or list_stack[-1] != "ul":
                close_lists(); out.append("<ul>"); list_stack.append("ul")
            done = task.group(1).lower() == "x"
            box = "checked disabled" if done else "disabled"
            cls = " class='done'" if done else ""
            out.append(f"<li{cls}><input type='checkbox' {box}> "
                       f"{_inline(task.group(2), ctx)}</li>")
        elif _OLI.match(raw):
            if not list_stack or list_stack[-1] != "ol":
                close_lists(); out.append("<ol>"); list_stack.append("ol")
            out.append(f"<li>{_inline(_OLI.match(raw).group(1), ctx)}</li>")
        elif _ULI.match(raw):
            if not list_stack or list_stack[-1] != "ul":
                close_lists(); out.append("<ul>"); list_stack.append("ul")
            out.append(f"<li>{_inline(_ULI.match(raw).group(1), ctx)}</li>")
        elif raw.strip() == "":
            close_lists()
        elif re.match(r"^\s*>\s?", raw):
            close_lists()
            quoted = re.sub(r"^\s*>\s?", "", raw)
            out.append(f"<blockquote>{_inline(quoted, ctx)}</blockquote>")
        elif re.match(r"^\s*(---|\*\*\*)\s*$", raw):
            close_lists(); out.append("<hr>")
        else:
            close_lists()
            out.append(f"<p>{_inline(raw, ctx)}</p>")
        i += 1
    close_lists()
    if footnotes:
        out.append(_footnotes_html(footnotes, ctx))
    return "\n".join(out)


# ---------------------------------------------------------------- inline rules

def _inline(text: str, ctx: RenderContext) -> str:
    out = html.escape(text)
    out = re.sub(r"`([^`]+)`", lambda m: f"<code>{m.group(1)}</code>", out)
    out = _EMBED.sub(lambda m: _embed_inline(m.group(1).strip(), ctx), out)
    out = _WIKILINK.sub(lambda m: _wl(m, ctx), out)
    out = _FOOTNOTE_REF.sub(
        lambda m: (f'<sup class="fn-ref" id="fnref-{m.group(1)}">'
                   f'<a href="#fn-{m.group(1)}">{m.group(1)}</a></sup>'), out)
    out = re.sub(r"==([^=]+)==", r"<mark>\1</mark>", out)
    out = re.sub(r"~~([^~]+)~~", r"<del>\1</del>", out)
    out = re.sub(r"\*\*([^*]+)\*\*", r"<strong>\1</strong>", out)
    out = re.sub(r"(?<!\*)\*([^*]+)\*(?!\*)", r"<em>\1</em>", out)
    out = re.sub(r"\[([^\]]+)\]\((https?:[^)]+)\)",
                 r'<a href="\2" target="_blank" rel="noopener">\1</a>', out)
    out = re.sub(r"(^|\s)#([A-Za-z][\w/-]*)", r'\1<span class="tag">#\2</span>', out)
    return out


def _embed_inline(target: str, ctx: RenderContext) -> str:
    """Inline `![[...]]`: images render as <img>; a mid-line note embed renders
    as a plain link (block-level transclusion only happens on its own line)."""
    if _is_image(target):
        return (f'<img src="{html.escape(ctx.img_src(target))}" '
                f'alt="{html.escape(target)}">')
    rel = ctx.link_map.get(target.split("#")[0].strip().lower())
    if rel:
        return f'<a class="wikilink" href="{html.escape(ctx.link_href(rel))}">{html.escape(target)}</a>'
    return f'<span class="unresolved">{html.escape(target)}</span>'


def _wl(m, ctx: RenderContext) -> str:
    raw_target = m.group(1).strip()
    base, _, anchor = raw_target.partition("#")
    label = html.escape(m.group(2) or raw_target)
    dst = ctx.link_map.get(base.strip().lower())
    if dst:
        href = ctx.link_href(dst)
        if anchor:
            href += "#" + heading_id(anchor)
        return f'<a class="wikilink" href="{html.escape(href)}">{label}</a>'
    return f'<span class="unresolved">{label}</span>'


# ------------------------------------------------------- footnotes & headings

def heading_id(text: str) -> str:
    """Stable, url-safe anchor for a heading. Shared by links and headings so
    [[note#My Heading]] scrolls to <h2 id="h-my-heading">."""
    slug = re.sub(r"[^\w\s-]", "", text.strip().lower())
    slug = re.sub(r"[\s_]+", "-", slug).strip("-")
    return f"h-{slug or 'heading'}"


def _collect_footnotes(body: str) -> dict[str, str]:
    return {m.group(1): m.group(2)
            for m in (_FOOTNOTE_DEF.match(line) for line in body.split("\n")) if m}


def _footnotes_html(notes: dict[str, str], ctx: RenderContext) -> str:
    items = "".join(
        f'<li id="fn-{html.escape(k)}">{_inline(v, ctx)} '
        f'<a class="fn-back" href="#fnref-{html.escape(k)}">↩</a></li>'
        for k, v in notes.items())
    return f'<div class="footnotes"><hr><ol>{items}</ol></div>'


# ------------------------------------------------------------- transclusion

def _transclude(target: str, ctx: RenderContext) -> str:
    """Render another note's body inline. Depth-limited and cycle-safe; the
    body callback decides what may be embedded (private/encrypted excluded)."""
    base = target.split("#")[0].strip()
    rel = ctx.link_map.get(base.lower())
    label = html.escape(base)
    if not rel:
        return f'<div class="embed embed-missing">![[{label}]] — not found</div>'
    if ctx.depth >= MAX_EMBED_DEPTH or rel in ctx._embedding:
        return (f'<div class="embed embed-cycle">'
                f'<a class="wikilink" href="{html.escape(ctx.link_href(rel))}">{label}</a>'
                f' (embed depth limit)</div>')
    inner_body = ctx.note_body(rel)
    if inner_body is None:
        return f'<div class="embed embed-missing">![[{label}]] — unavailable</div>'
    sub = RenderContext(link_map=ctx.link_map, img_src=ctx.img_src,
                        note_body=ctx.note_body, run_query=ctx.run_query,
                        link_href=ctx.link_href, depth=ctx.depth + 1,
                        _embedding=ctx._embedding | {rel})
    return (f'<div class="embed"><div class="embed-title">'
            f'<a class="wikilink" href="{html.escape(ctx.link_href(rel))}">{label}</a></div>'
            f'{render(inner_body, ctx=sub)}</div>')


# ------------------------------------------------------------- query blocks

def _query_html(block: str, ctx: RenderContext) -> str:
    """Render a ```query block via the context's runner (server/queries.py)."""
    result = ctx.run_query(block)
    if result.get("errors"):
        errs = "; ".join(html.escape(e) for e in result["errors"])
        return f'<div class="query query-error">query error: {errs}</div>'
    rows = result["rows"]
    if result["render"] == "count":
        return f'<div class="query query-count">{len(rows)}</div>'
    if result["render"] == "table":
        cols = result["columns"]
        head = "".join(f"<th>{html.escape(c)}</th>" for c in cols)
        trs = []
        for r in rows:
            tds = []
            for c in cols:
                if c == "title":
                    tds.append(f'<td><a class="wikilink" '
                               f'href="{html.escape(ctx.link_href(r["path"]))}">'
                               f'{html.escape(r.get("title") or r["path"])}</a></td>')
                elif c == "tags":
                    tds.append("<td>" + " ".join(
                        f'<span class="tag">#{html.escape(t)}</span>'
                        for t in r.get("tags", [])) + "</td>")
                else:
                    tds.append(f"<td>{html.escape(str(r.get(c) or ''))}</td>")
            trs.append("<tr>" + "".join(tds) + "</tr>")
        return (f'<div class="query table-wrap"><table><thead><tr>{head}</tr></thead>'
                f'<tbody>{"".join(trs)}</tbody></table></div>')
    items = "".join(
        f'<li><a class="wikilink" href="{html.escape(ctx.link_href(r["path"]))}">'
        f'{html.escape(r.get("title") or r["path"])}</a></li>' for r in rows)
    return f'<div class="query"><ul>{items}</ul></div>'


# ----------------------------------------------------------- code highlighting

# lightweight, language-agnostic syntax highlighting (no external deps)
_KEYWORDS = {
    "const", "let", "var", "function", "func", "fn", "return", "if", "else", "elif",
    "for", "while", "class", "struct", "interface", "type", "enum", "import", "export",
    "from", "package", "use", "pub", "async", "await", "new", "def", "lambda", "try",
    "except", "catch", "finally", "throw", "with", "match", "case", "switch", "break",
    "continue", "in", "of", "is", "not", "and", "or", "public", "private", "protected",
    "static", "void", "int", "float", "double", "string", "str", "bool", "true", "false",
    "True", "False", "None", "null", "nil", "self", "this", "super", "impl", "trait",
    "yield", "do", "then", "end", "module", "namespace", "typedef", "extends", "implements",
}
_TOKEN = re.compile(
    r"(//[^\n]*|#[^\n]*|/\*[\s\S]*?\*/)"          # comments
    r'|("(?:\\.|[^"\\])*"|\'(?:\\.|[^\'\\])*\'|`(?:\\.|[^`\\])*`)'  # strings
    r"|(\b\d[\d_.]*(?:[eE][+-]?\d+)?\b|\b0[xX][0-9a-fA-F]+\b)"      # numbers
    r"|([A-Za-z_$][\w$]*)")                        # identifiers


def highlight_code(code: str, lang: str = "") -> str:
    def repl(m):
        if m.group(1):
            return f'<span class="hl-com">{html.escape(m.group(1))}</span>'
        if m.group(2):
            return f'<span class="hl-str">{html.escape(m.group(2))}</span>'
        if m.group(3):
            return f'<span class="hl-num">{html.escape(m.group(3))}</span>'
        word = m.group(4)
        if word in _KEYWORDS:
            return f'<span class="hl-kw">{html.escape(word)}</span>'
        return html.escape(word)
    out, last = [], 0
    for m in _TOKEN.finditer(code):
        out.append(html.escape(code[last:m.start()]))
        out.append(repl(m))
        last = m.end()
    out.append(html.escape(code[last:]))
    return "".join(out)


# ------------------------------------------------------------------- tables

def _is_table_row(line: str) -> bool:
    s = line.strip()
    return s.startswith("|") and s.count("|") >= 2


def _is_table_sep(line: str) -> bool:
    s = line.strip()
    return bool(re.match(r"^\|?[\s:|-]*-[\s:|-]*\|?$", s)) and "-" in s and "|" in s


def _cells(line: str) -> list[str]:
    return [c.strip() for c in line.strip().strip("|").split("|")]


def _table_html(header: str, rows: list[str], ctx: RenderContext) -> str:
    head = "".join(f"<th>{_inline(c, ctx)}</th>" for c in _cells(header))
    body = "".join(
        "<tr>" + "".join(f"<td>{_inline(c, ctx)}</td>" for c in _cells(r)) + "</tr>"
        for r in rows)
    return (f'<div class="table-wrap"><table><thead><tr>{head}</tr></thead>'
            f'<tbody>{body}</tbody></table></div>')


# -------------------------------------------------------------------- helpers

def _is_image(target: str) -> bool:
    return target.lower().strip().endswith(_IMAGE_EXTS)


def _strip_md(path: str) -> str:
    return path[:-3] if path.endswith(".md") else path
