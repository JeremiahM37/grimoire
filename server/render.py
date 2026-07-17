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

    for raw in lines:
        if raw.strip().startswith("```"):
            close_lists()
            in_code = not in_code
            out.append("<pre><code>" if in_code else "</code></pre>")
            continue
        if in_code:
            out.append(html.escape(raw))
            continue
        h = _HEADING.match(raw)
        if h:
            close_lists()
            lvl = len(h.group(1))
            out.append(f"<h{lvl}>{_inline(h.group(2), link_map, img_src)}</h{lvl}>")
            continue
        task = _TASK.match(raw)
        if task:
            if not list_stack or list_stack[-1] != "ul":
                close_lists(); out.append("<ul>"); list_stack.append("ul")
            done = task.group(1).lower() == "x"
            box = "checked disabled" if done else "disabled"
            cls = " class='done'" if done else ""
            out.append(f"<li{cls}><input type='checkbox' {box}> "
                       f"{_inline(task.group(2), link_map, img_src)}</li>")
            continue
        oli = _OLI.match(raw)
        if oli:
            if not list_stack or list_stack[-1] != "ol":
                close_lists(); out.append("<ol>"); list_stack.append("ol")
            out.append(f"<li>{_inline(oli.group(1), link_map, img_src)}</li>")
            continue
        uli = _ULI.match(raw)
        if uli:
            if not list_stack or list_stack[-1] != "ul":
                close_lists(); out.append("<ul>"); list_stack.append("ul")
            out.append(f"<li>{_inline(uli.group(1), link_map, img_src)}</li>")
            continue
        if raw.strip() == "":
            close_lists(); continue
        if re.match(r"^\s*>\s?", raw):
            close_lists()
            quoted = re.sub(r"^\s*>\s?", "", raw)
            out.append(f"<blockquote>{_inline(quoted, link_map, img_src)}</blockquote>")
            continue
        if re.match(r"^\s*(---|\*\*\*)\s*$", raw):
            close_lists(); out.append("<hr>"); continue
        close_lists()
        out.append(f"<p>{_inline(raw, link_map, img_src)}</p>")
    close_lists()
    return "\n".join(out)
