"""Server-side markdown → safe HTML. Mirrors the PWA's client renderer so the
e-ink surface and HTML export look the same. Escapes first, then applies a small,
well-scoped set of rules. Used by routers/read.py and the export endpoint.

`link_map`: {lowercased title-or-stem: rel-path} → wiki-links become <a>.
`img_src(rel)`: returns the src for an ![[embed]] (a URL, or a data: URI for export).
"""
import html
import re
from typing import Callable, Optional

_WIKILINK = re.compile(r"\[\[([^\[\]|]+?)(?:\|([^\[\]]+))?\]\]")
_IMG = re.compile(r"!\[\[([^\[\]|]+?)\]\]")
_HEADING = re.compile(r"^(#{1,6})\s+(.+)$")
_TASK = re.compile(r"^\s*[-*]\s+\[([ xX])\]\s+(.*)$")
_ULI = re.compile(r"^\s*[-*]\s+(.*)$")
_OLI = re.compile(r"^\s*\d+\.\s+(.*)$")


def _inline(text: str, link_map: dict, img_src: Callable[[str], str]) -> str:
    out = html.escape(text)
    out = re.sub(r"`([^`]+)`", lambda m: f"<code>{m.group(1)}</code>", out)
    out = _IMG.sub(lambda m: _img(m.group(1).strip(), img_src), out)
    out = _WIKILINK.sub(lambda m: _wl(m, link_map), out)
    out = re.sub(r"\*\*([^*]+)\*\*", r"<strong>\1</strong>", out)
    out = re.sub(r"(?<!\*)\*([^*]+)\*(?!\*)", r"<em>\1</em>", out)
    out = re.sub(r"\[([^\]]+)\]\((https?:[^)]+)\)",
                 r'<a href="\2" target="_blank" rel="noopener">\1</a>', out)
    out = re.sub(r"(^|\s)#([A-Za-z][\w/-]*)", r'\1<span class="tag">#\2</span>', out)
    return out


def _img(src: str, img_src: Callable[[str], str]) -> str:
    return f'<img src="{html.escape(img_src(src))}" alt="{html.escape(src)}">'


def _wl(m, link_map: dict) -> str:
    base = m.group(1).split("#")[0].strip()
    label = html.escape(m.group(2) or m.group(1))
    dst = link_map.get(base.lower())
    if dst:
        return f'<a class="wikilink" href="/read/{_u(dst)}">{label}</a>'
    return f'<span class="unresolved">{label}</span>'


def _u(path: str) -> str:
    return path[:-3] if path.endswith(".md") else path


def render(body: str, link_map: Optional[dict] = None,
           img_src: Optional[Callable[[str], str]] = None) -> str:
    link_map = link_map or {}
    img_src = img_src or (lambda rel: "/api/file/" + rel)
    lines = body.split("\n")
    out: list[str] = []
    in_code = False
    list_stack: list[str] = []   # 'ul' | 'ol'

    def close_lists():
        while list_stack:
            out.append(f"</{list_stack.pop()}>")

    i = 0
    while i < len(lines):
        raw = lines[i]
        if raw.strip().startswith("```"):
            close_lists()
            in_code = not in_code
            out.append("<pre><code>" if in_code else "</code></pre>")
            i += 1
            continue
        if in_code:
            out.append(html.escape(raw))
            i += 1
            continue
        # a table: a header row followed by a |---|---| separator
        if not in_code and _is_table_row(raw) and i + 1 < len(lines) and _is_table_sep(lines[i + 1]):
            close_lists()
            j = i + 2
            body_rows = []
            while j < len(lines) and _is_table_row(lines[j]):
                body_rows.append(lines[j]); j += 1
            out.append(_table_html(raw, body_rows, link_map, img_src))
            i = j
            continue
        h = _HEADING.match(raw)
        if h:
            close_lists()
            lvl = len(h.group(1))
            out.append(f"<h{lvl}>{_inline(h.group(2), link_map, img_src)}</h{lvl}>")
        elif _TASK.match(raw):
            task = _TASK.match(raw)
            if not list_stack or list_stack[-1] != "ul":
                close_lists(); out.append("<ul>"); list_stack.append("ul")
            done = task.group(1).lower() == "x"
            box = "checked disabled" if done else "disabled"
            cls = " class='done'" if done else ""
            out.append(f"<li{cls}><input type='checkbox' {box}> "
                       f"{_inline(task.group(2), link_map, img_src)}</li>")
        elif _OLI.match(raw):
            if not list_stack or list_stack[-1] != "ol":
                close_lists(); out.append("<ol>"); list_stack.append("ol")
            out.append(f"<li>{_inline(_OLI.match(raw).group(1), link_map, img_src)}</li>")
        elif _ULI.match(raw):
            if not list_stack or list_stack[-1] != "ul":
                close_lists(); out.append("<ul>"); list_stack.append("ul")
            out.append(f"<li>{_inline(_ULI.match(raw).group(1), link_map, img_src)}</li>")
        elif raw.strip() == "":
            close_lists()
        elif re.match(r"^\s*>\s?", raw):
            close_lists()
            quoted = re.sub(r"^\s*>\s?", "", raw)
            out.append(f"<blockquote>{_inline(quoted, link_map, img_src)}</blockquote>")
        elif re.match(r"^\s*(---|\*\*\*)\s*$", raw):
            close_lists(); out.append("<hr>")
        else:
            close_lists()
            out.append(f"<p>{_inline(raw, link_map, img_src)}</p>")
        i += 1
    close_lists()
    return "\n".join(out)


def _is_table_row(line: str) -> bool:
    s = line.strip()
    return s.startswith("|") and s.count("|") >= 2


def _is_table_sep(line: str) -> bool:
    s = line.strip()
    return bool(re.match(r"^\|?[\s:|-]*-[\s:|-]*\|?$", s)) and "-" in s and "|" in s


def _cells(line: str) -> list[str]:
    return [c.strip() for c in line.strip().strip("|").split("|")]


def _table_html(header: str, rows: list[str], link_map, img_src) -> str:
    head = "".join(f"<th>{_inline(c, link_map, img_src)}</th>" for c in _cells(header))
    body = "".join(
        "<tr>" + "".join(f"<td>{_inline(c, link_map, img_src)}</td>" for c in _cells(r)) + "</tr>"
        for r in rows)
    return f'<div class="table-wrap"><table><thead><tr>{head}</tr></thead><tbody>{body}</tbody></table></div>'
