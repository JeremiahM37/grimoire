"""Server-side markdown renderer (e-ink surface + HTML export)."""
from server import render


def test_headings_paragraphs_and_inline():
    h = render.render("# Title\n\nsome **bold** and *italic* and `code` here")
    assert '<h1 id="h-title">Title</h1>' in h
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


def test_code_block_is_escaped_and_not_wikilinked():
    h = render.render("```\n<b>x</b> [[not a link]]\n```")
    assert "&lt;b&gt;x&lt;/b&gt;" in h            # HTML escaped
    assert "<pre>" in h and "[[" in h             # inside a code block, brackets kept
    assert 'class="wikilink"' not in h            # NOT turned into a link


def test_tables_render_with_header_and_rows():
    md = "| Name | Age |\n|------|-----|\n| Bob | 30 |\n| Ann | 25 |"
    h = render.render(md)
    assert "<table>" in h and "<th>Name</th>" in h
    assert "<td>Bob</td>" in h and "<td>25</td>" in h
    assert h.count("<tr>") == 3          # header + 2 body rows


def test_table_needs_separator_else_plain_paragraph():
    # a lone pipe line without a |---| separator is NOT a table
    h = render.render("| just | text |\nno separator here")
    assert "<table>" not in h


def test_highlight_and_strikethrough():
    h = render.render("this is ==hot== and ~~cold~~")
    assert "<mark>hot</mark>" in h and "<del>cold</del>" in h


def test_callout_block():
    h = render.render("> [!warning] Heads up\n> be ==careful==\n>\n> more")
    assert 'class="callout callout-warning"' in h
    assert "Heads up" in h and "<mark>careful</mark>" in h
    assert h.count("<p>") >= 2   # two paragraphs in the body


def test_callout_default_title_from_type():
    h = render.render("> [!note]\n> content")
    assert 'callout-note' in h and "Note" in h


def test_code_syntax_highlighting():
    h = render.render("```js\nconst x = \"hi\"; // note\n```")
    assert 'class="lang-js"' in h
    assert '<span class="hl-kw">const</span>' in h
    assert '<span class="hl-str">&quot;hi&quot;</span>' in h
    assert '<span class="hl-com">// note</span>' in h


def test_highlighting_is_xss_safe():
    h = render.render("```\n<script>alert(1)</script>\n```")
    assert "&lt;script&gt;" in h and "<script>alert" not in h
