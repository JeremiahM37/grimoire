"""E-ink / Kindle read surface — plain HTML, no JS, big fonts, hyperlinked.

A read-mostly device (Kindle browser, e-reader) can open /read to browse the
whole vault without needing the PWA. Wiki-links resolve to hyperlinks; private
notes are excluded.
"""
import base64
import html
import mimetypes

from fastapi import APIRouter, HTTPException
from fastapi.responses import HTMLResponse

from .. import db, index, render, vault

router = APIRouter()

_STYLE = """<style>
body{font-family:Georgia,serif;max-width:40rem;margin:0 auto;padding:1.2rem;
font-size:1.25rem;line-height:1.7;color:#111;background:#fff}
a{color:#000}h1,h2,h3{line-height:1.25}code{font-family:monospace}
.unresolved{color:#888}nav a{display:block;padding:.3rem 0}
hr{border:none;border-top:1px solid #ccc;margin:1.5rem 0}
img{max-width:100%;height:auto}
mark{background:#fdf3b0}del{color:#888}
.hl-kw{color:#6a4bd8;font-weight:600}.hl-str{color:#2f6f6a}.hl-com{color:#888;font-style:italic}.hl-num{color:#b26b3a}pre code{background:none;padding:0}
.callout{border:1px solid #ddd;border-left:4px solid #888;border-radius:8px;margin:1rem 0;padding:.5rem 1rem}
.callout-title{font-weight:600;margin-bottom:.2rem}
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


def _link_map() -> dict:
    resolved = {}
    for n in db.query("SELECT path, title FROM notes WHERE private=0"):
        resolved[n["title"].lower()] = n["path"]
        resolved[n["path"].rsplit("/", 1)[-1][:-3].lower()] = n["path"]
    return resolved


def _render(body: str) -> str:
    """Full markdown → safe HTML with wiki-links + images, via the shared renderer."""
    return render.render(body, _link_map())


def _data_uri(rel: str) -> str:
    """Inline a vault image as a data: URI so an exported file is self-contained."""
    try:
        p = vault.safe_raw_path(rel)
        if not p.exists() or p.stat().st_size > 8 * 1024 * 1024:
            return "/api/file/" + rel
        mime = mimetypes.guess_type(str(p))[0] or "application/octet-stream"
        return f"data:{mime};base64," + base64.b64encode(p.read_bytes()).decode()
    except Exception:
        return "/api/file/" + rel


@router.get("/notes/{path:path}/export.html", response_class=HTMLResponse)
def export_note(path: str, download: bool = False):
    """Standalone, self-contained HTML (images inlined). Opens inline for
    print-to-PDF by default; `?download=1` forces a file download."""
    rel = path if path.endswith(".md") else path + ".md"
    row = db.one("SELECT * FROM notes WHERE path=?", (rel,))
    if not row:
        raise HTTPException(404, "not found")
    # never render an encrypted note here: this route is unauthenticated, so
    # decrypting would leak plaintext by URL, and the ciphertext blob is useless
    from .. import secrets
    if secrets.is_encrypted(row["body"]):
        raise HTTPException(404, "not found")
    body = render.render(row["body"], _link_map(), img_src=_data_uri)
    doc = _page(row["title"], f"<article>{body}</article>", export=True)
    headers = {}
    if download:
        headers["Content-Disposition"] = f'attachment; filename="{vault.slugify(row["title"])}.html"'
    return HTMLResponse(doc, headers=headers)


def _u(path: str) -> str:
    return path[:-3] if path.endswith(".md") else path


_EXPORT_STYLE = """<style>
body{font-family:Georgia,'Iowan Old Style',serif;max-width:44rem;margin:2rem auto;
padding:0 1.4rem;font-size:1.05rem;line-height:1.65;color:#1a1a1a;background:#fff}
h1,h2,h3{line-height:1.25;font-weight:600}code{font-family:ui-monospace,monospace;
background:#f2f0ea;padding:1px 5px;border-radius:4px}pre{background:#f2f0ea;padding:.8rem;
border-radius:8px;overflow-x:auto}blockquote{border-left:3px solid #ccc;margin:0;
padding-left:1rem;color:#555}img{max-width:100%;height:auto;border-radius:6px}
.tag{color:#6a4bd8}a{color:#245}.unresolved{color:#999}
mark{background:#fdf3b0}del{color:#888}
.hl-kw{color:#6a4bd8;font-weight:600}.hl-str{color:#2f6f6a}.hl-com{color:#888;font-style:italic}.hl-num{color:#b26b3a}pre code{background:none;padding:0}
table{border-collapse:collapse}th,td{border:1px solid #ccc;padding:5px 10px}
.callout{border:1px solid #ddd;border-left:4px solid #6a4bd8;border-radius:8px;margin:1rem 0;padding:.5rem 1rem}
.callout-title{font-weight:600;margin-bottom:.2rem;color:#6a4bd8}
li input{margin-right:.4rem}li.done{color:#888;text-decoration:line-through}
hr{border:none;border-top:1px solid #ddd;margin:1.5rem 0}
@media print{body{margin:0;max-width:none}a{color:#000;text-decoration:none}}
</style>"""


def _page(title: str, body: str, export: bool = False) -> str:
    style = _EXPORT_STYLE if export else _STYLE
    return (f"<!doctype html><html><head><meta charset='utf-8'>"
            f"<meta name='viewport' content='width=device-width,initial-scale=1'>"
            f"<title>{html.escape(title)}</title>{style}</head><body>{body}</body></html>")
