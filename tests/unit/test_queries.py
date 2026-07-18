"""Live-query engine: parsing, validation, execution, and its security posture
(parameterization, private exclusion, caps)."""
from server import index, queries, vault


def _seed(vaultdir):
    vault.write("alpha.md", "# Alpha\n\nbody #project", {"title": "Alpha"})
    vault.write("beta.md", "# Beta\n\nlinks to [[Alpha]] #project #work", {"title": "Beta"})
    vault.write("work/gamma.md", "# Gamma\n\nquarterly report", {"title": "Gamma"})
    vault.write("hidden.md", "# Hidden\n\nsecret stuff #project",
                {"title": "Hidden", "private": True})
    for p in ("alpha.md", "beta.md", "work/gamma.md", "hidden.md"):
        index.upsert(p)


# ------------------------------------------------------------------ parsing

def test_parse_full_block():
    spec = queries.parse("tag: #project\npath: work/\nsort: title asc\n"
                         "limit: 5\nrender: table\ncolumns: title, tags")
    assert spec.tag == "project" and spec.path == "work/"
    assert spec.sort == "title" and spec.sort_desc is False
    assert spec.limit == 5 and spec.render == "table"
    assert spec.columns == ["title", "tags"] and not spec.errors


def test_parse_collects_errors_instead_of_raising():
    spec = queries.parse("sort: evil; DROP TABLE notes\nrender: nope\n"
                         "limit: banana\nbogus: x")
    assert len(spec.errors) == 4
    # invalid values never replace the safe defaults
    assert spec.sort == "updated" and spec.render == "list"
    assert spec.limit == queries.DEFAULT_LIMIT


def test_limit_is_capped():
    assert queries.parse("limit: 99999").limit == queries.MAX_LIMIT


# ---------------------------------------------------------------- execution

def test_tag_filter(vaultdir):
    _seed(vaultdir)
    rows = queries.execute(queries.parse("tag: project"))
    assert {r["path"] for r in rows} == {"alpha.md", "beta.md"}   # hidden excluded


def test_private_excluded_by_default_included_on_opt_in(vaultdir):
    _seed(vaultdir)
    public = queries.run("tag: project")["rows"]
    assert "hidden.md" not in {r["path"] for r in public}
    private = queries.run("tag: project", include_private=True)["rows"]
    assert "hidden.md" in {r["path"] for r in private}


def test_path_prefix_is_literal_not_like_pattern(vaultdir):
    _seed(vaultdir)
    assert {r["path"] for r in queries.execute(queries.parse("path: work/"))} \
        == {"work/gamma.md"}
    # LIKE metacharacters in the prefix must not act as wildcards
    assert queries.execute(queries.parse("path: %")) == []


def test_linked_to(vaultdir):
    _seed(vaultdir)
    rows = queries.execute(queries.parse("linked-to: [[Alpha]]"))
    assert {r["path"] for r in rows} == {"beta.md"}


def test_text_fts_and_injection_safe(vaultdir):
    _seed(vaultdir)
    rows = queries.execute(queries.parse("text: quarterly"))
    assert {r["path"] for r in rows} == {"work/gamma.md"}
    # FTS operators in user text are treated as literal text, not syntax
    assert queries.execute(queries.parse('text: quarterly" OR path:"')) == []


def test_run_shape_and_errors_short_circuit(vaultdir):
    _seed(vaultdir)
    ok = queries.run("tag: project\nrender: count")
    assert ok["render"] == "count" and ok["count"] == 2 and not ok["errors"]
    bad = queries.run("sort: nope")
    assert bad["errors"] and bad["rows"] == []
