"""E-ink / Kindle read surface — plain HTML, no JS, big fonts, hyperlinked.

A read-mostly device (Kindle browser, e-reader) can open /read to browse the
whole vault without needing the PWA. Wiki-links resolve to hyperlinks; private
notes are excluded.
"""
import html
import re

from fastapi import APIRouter, HTTPException
from fastapi.responses import HTMLResponse

from .. import db, index, vault

router = APIRouter()

_STYLE = """<style>
body{font-family:Georgia,serif;max-width:40rem;margin:0 auto;padding:1.2rem;
font-size:1.25rem;line-height:1.7;color:#111;background:#fff}
a{color:#000}h1,h2,h3{line-height:1.25}code{font-family:monospace}
.unresolved{color:#888}nav a{display:block;padding:.3rem 0}
hr{border:none;border-top:1px solid #ccc;margin:1.5rem 0}
.back{font-size:1rem}
</style>"""


@router.get("/read", response_class=HTMLResponse)
def read_index():
    rows = db.query("SELECT path, title FROM notes WHERE private=0 ORDER BY title")
    items = "".join(f'<a href="/read/{_u(r["path"])}">{html.escape(r["title"])}</a>'
                    for r in rows)
    return _page("mnemo — notes", f"<h1>mnemo</h1><nav>{items}</nav>")


@router.get("/read/{path:path}", response_class=HTMLResponse)
def read_note(path: str):
    rel = path if path.endswith(".md") else path + ".md"
    row = db.one("SELECT * FROM notes WHERE path=? AND private=0", (rel,))
    if not row:
        raise HTTPException(404, "not found")
    body = _render(row["body"])
    bl = index.backlinks(rel)
    back = ""
    if bl:
        links = "".join(f'<a href="/read/{_u(b["path"])}">{html.escape(b["title"])}</a> '
                        for b in bl)
        back = f"<hr><p class='back'>Linked from: {links}</p>"
    return _page(row["title"],
                 f'<p class="back"><a href="/read">← all notes</a></p>{body}{back}')


def _render(body: str) -> str:
    """Minimal, safe markdown → HTML with wiki-links as hyperlinks."""
    resolved = {}
    for n in db.query("SELECT path, title FROM notes WHERE private=0"):
        resolved[n["title"].lower()] = n["path"]
        resolved[n["path"].rsplit("/", 1)[-1][:-3].lower()] = n["path"]
    out, in_code = [], False
    for raw in body.split("\n"):
        if raw.strip().startswith("```"):
            in_code = not in_code
            out.append("<pre>" if in_code else "</pre>")
            continue
        if in_code:
            out.append(html.escape(raw)); continue
        line = html.escape(raw)
        line = re.sub(r"\[\[([^\]|]+?)(?:\|([^\]]+))?\]\]", lambda m: _wl(m, resolved), line)
        h = re.match(r"^(#{1,3})\s+(.+)$", raw)
        if h:
            out.append(f"<h{len(h.group(1))}>{html.escape(h.group(2))}</h{len(h.group(1))}>")
        elif raw.strip():
            out.append(f"<p>{line}</p>")
    return "\n".join(out)


def _wl(m, resolved) -> str:
    base = m.group(1).split("#")[0].strip()
    label = html.escape(m.group(2) or m.group(1))
    dst = resolved.get(base.lower())
    if dst:
        return f'<a href="/read/{_u(dst)}">{label}</a>'
    return f'<span class="unresolved">{label}</span>'


def _u(path: str) -> str:
    return path[:-3] if path.endswith(".md") else path


def _page(title: str, body: str) -> str:
    return (f"<!doctype html><html><head><meta charset='utf-8'>"
            f"<meta name='viewport' content='width=device-width,initial-scale=1'>"
            f"<title>{html.escape(title)}</title>{_STYLE}</head><body>{body}</body></html>")
