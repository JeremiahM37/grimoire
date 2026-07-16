from server import markdown as md


def test_frontmatter_scalars_and_lists():
    fm, body = md.parse_frontmatter(
        "---\ntitle: Hello World\ntags: [a, b, c]\nprivate: true\n---\nbody here")
    assert fm["title"] == "Hello World"
    assert fm["tags"] == ["a", "b", "c"]
    assert fm["private"] is True
    assert body.strip() == "body here"


def test_frontmatter_block_list():
    fm, _ = md.parse_frontmatter("---\ntags:\n  - one\n  - two\n---\nx")
    assert fm["tags"] == ["one", "two"]


def test_no_frontmatter():
    fm, body = md.parse_frontmatter("# Just a note\n")
    assert fm == {} and body == "# Just a note\n"


def test_wikilinks_alias_and_dedupe():
    links = md.extract_links("See [[Alpha]] and [[Beta|the beta]] and [[Alpha]] again")
    assert [l["target"] for l in links] == ["Alpha", "Beta"]
    assert links[1]["alias"] == "the beta"


def test_wikilinks_ignore_code_and_anchors():
    links = md.extract_links("`[[not a link]]` but [[Real#heading]] counts")
    assert [l["target"] for l in links] == ["Real"]


def test_tags_extraction_ignores_headings_and_code():
    tags = md.extract_tags("## Heading not a tag\ntext #real and #nested/tag `#incode`")
    assert "real" in tags and "nested/tag" in tags
    assert "Heading" not in tags and "incode" not in tags


def test_title_precedence():
    assert md.derive_title({"title": "FM"}, "# H1\n", "stem") == "FM"
    assert md.derive_title({}, "# H1 title\n", "stem") == "H1 title"
    assert md.derive_title({}, "no heading", "the-stem") == "the-stem"
