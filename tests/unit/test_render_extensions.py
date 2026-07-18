"""Renderer extensions: heading anchors, [[note#heading]] links, footnotes,
![[note]] transclusion (depth/cycle safety), and ```query blocks."""
from server import render
from server.render import RenderContext


def test_heading_gets_stable_anchor_id():
    h = render.render("## My Great Heading")
    assert '<h2 id="h-my-great-heading">' in h


def test_wikilink_with_heading_anchor():
    h = render.render("[[Known#My Section]]", {"known": "known.md"})
    assert 'href="/read/known#h-my-section"' in h


def test_footnotes_render_refs_and_list():
    h = render.render("A claim.[^1]\n\n[^1]: The source.")
    assert '<sup class="fn-ref" id="fnref-1">' in h
    assert '<li id="fn-1">The source.' in h
    # the definition line is not rendered as a paragraph too
    assert h.count("The source.") == 1


def test_transclusion_renders_embedded_note():
    bodies = {"inner.md": "# Inner\n\nembedded text"}
    ctx = RenderContext(link_map={"inner": "inner.md"}, note_body=bodies.get)
    h = render.render("before\n\n![[Inner]]\n\nafter", ctx=ctx)
    assert 'class="embed"' in h and "embedded text" in h


def test_transclusion_cycle_is_guarded():
    bodies = {"a.md": "![[B]]", "b.md": "![[A]]"}
    ctx = RenderContext(link_map={"a": "a.md", "b": "b.md"}, note_body=bodies.get)
    h = render.render("![[A]]", ctx=ctx)
    assert "embed depth limit" in h            # terminated, no recursion blowup


def test_transclusion_unavailable_note_degrades_gracefully():
    ctx = RenderContext(link_map={"x": "x.md"}, note_body=lambda rel: None)
    assert "unavailable" in render.render("![[X]]", ctx=ctx)
    assert "not found" in render.render("![[Ghost]]", ctx=ctx)


def test_image_embed_still_inline_not_transcluded():
    ctx = RenderContext(note_body=lambda rel: "SHOULD NOT APPEAR")
    h = render.render("![[pic.png]]", ctx=ctx)
    assert "<img" in h and "SHOULD NOT APPEAR" not in h


def test_query_block_renders_via_runner():
    ctx = RenderContext(run_query=lambda block: {
        "render": "list", "columns": ["title"], "errors": [],
        "rows": [{"path": "a.md", "title": "Alpha"}], "count": 1})
    h = render.render("```query\ntag: x\n```", ctx=ctx)
    assert 'class="query"' in h and ">Alpha</a>" in h


def test_query_block_errors_are_shown_not_fatal():
    ctx = RenderContext(run_query=lambda block: {
        "render": "list", "columns": [], "errors": ["unknown key 'zap'"],
        "rows": [], "count": 0})
    h = render.render("```query\nzap: 1\n```", ctx=ctx)
    assert "query error" in h and "zap" in h


def test_query_block_without_runner_falls_back_to_code():
    # A context with no runner (e.g. bare render) shows the block as code
    h = render.render("```query\ntag: x\n```")
    assert "<pre>" in h and 'class="query"' not in h


def test_query_table_render_escapes_and_links():
    ctx = RenderContext(run_query=lambda block: {
        "render": "table", "columns": ["title", "tags"], "errors": [],
        "rows": [{"path": "x.md", "title": "<b>X</b>", "tags": ["t1"]}], "count": 1})
    h = render.render("```query\nrender: table\n```", ctx=ctx)
    assert "&lt;b&gt;X&lt;/b&gt;" in h          # titles are escaped
    assert '<span class="tag">#t1</span>' in h
