"""Server-side markdown renderer (e-ink surface + HTML export)."""
from server import render


def test_headings_paragraphs_and_inline():
    h = render.render("# Title\n\nsome **bold** and *italic* and `code` here")
    assert "<h1>Title</h1>" in h
    assert "<strong>bold</strong>" in h and "<em>italic</em>" in h and "<code>code</code>" in h


def test_lists_and_tasks():
    h = render.render("- a\n- b\n\n1. one\n2. two\n\n- [ ] todo\n- [x] done")
    assert "<ul>" in h and "<ol>" in h
    assert h.count("<li") >= 6
    assert "checked disabled" in h and "class='done'" in h


def test_wikilinks_resolve_and_dangle():
    h = render.render("see [[Known]] and [[Ghost]]", {"known": "known.md"})
    assert '<a class="wikilink" href="/read/known">Known</a>' in h
    assert '<span class="unresolved">Ghost</span>' in h


def test_images_use_provided_src():
    h = render.render("![[pic.png]]", img_src=lambda rel: "DATA:" + rel)
    assert '<img src="DATA:pic.png"' in h


def test_escapes_html_no_script_execution():
    h = render.render("<script>alert(1)</script>")
    assert "<script>" not in h and "&lt;script&gt;" in h


def test_code_block_is_escaped_verbatim():
    h = render.render("```\n<b>x</b> [[not a link]]\n```")
    assert "&lt;b&gt;x&lt;/b&gt;" in h
    assert "[[not a link]]" in h and "<pre>" in h
