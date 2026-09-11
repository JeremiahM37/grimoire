"""Real-server acceptance for the React knowledge explorer and knowledge APIs."""

from __future__ import annotations

import json
import re
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

import pytest
from playwright.sync_api import expect


def http_json(base: str, method: str, path: str, payload=None):
    body = None if payload is None else json.dumps(payload).encode()
    request = urllib.request.Request(
        base + path,
        data=body,
        method=method,
        headers={"Content-Type": "application/json"} if body is not None else {},
    )
    try:
        with urllib.request.urlopen(request, timeout=15) as response:
            raw = response.read()
            return response.status, json.loads(raw or b"{}")
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace")
        raise AssertionError(f"{method} {path} returned HTTP {exc.code}: {detail}") from exc


def multipart_import(
    base: str,
    filename: str,
    content: bytes,
    content_type: str = "text/markdown",
    *,
    path: str | None = None,
):
    boundary = "----grimoire-knowledge-acceptance"
    fields = b""
    if path is not None:
        fields = (
            f"--{boundary}\r\n"
            'Content-Disposition: form-data; name="path"\r\n\r\n'
            f"{path}\r\n"
        ).encode()
    body = fields + (
        f"--{boundary}\r\n"
        f'Content-Disposition: form-data; name="file"; filename="{filename}"\r\n'
        f"Content-Type: {content_type}\r\n\r\n"
    ).encode() + content + f"\r\n--{boundary}--\r\n".encode()
    request = urllib.request.Request(
        base + "/api/documents/import", data=body, method="POST",
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
    )
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            return response.status, json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace")
        try:
            payload = json.loads(detail)
        except json.JSONDecodeError:
            payload = {"detail": detail}
        return exc.code, payload


def assert_contract(obj: dict, *, required: tuple[str, ...], label: str):
    missing = [key for key in required if key not in obj]
    assert not missing, f"{label} missing contract fields {missing}: {obj}"


def test_graph_query_source_evidence_dates_and_trust(knowledge_server):
    base, _, _ = knowledge_server
    status, graph = http_json(
        base, "GET", "/api/knowledge/graph?seed=Launch%20plan&depth=2&limit=50"
    )
    assert status == 200
    assert_contract(graph, required=("revision", "nodes", "edges", "truncated"), label="graph")
    assert len(graph["nodes"]) <= 50
    assert any(node.get("label") == "Launch plan" for node in graph["nodes"])
    assert any(edge.get("evidence") for edge in graph["edges"]), "relationship lacks source evidence"
    assert not any(node.get("label") == "Security review" for node in graph["nodes"]), (
        "private document leaked into the unauthenticated graph"
    )
    evidence = next(edge["evidence"][0] for edge in graph["edges"] if edge.get("evidence"))
    assert evidence.get("path") and evidence.get("text")

    status, query = http_json(
        base,
        "POST",
        "/api/knowledge/query",
        {
            "question": "When is the Atlas launch review and who owns the canary?",
            "limit": 10,
            "depth": 2,
            "after": "2026-01-01",
            "before": "2026-01-31",
            "expand": True,
        },
    )
    assert status == 200
    assert_contract(query, required=("answer", "citations", "graph", "revision"), label="query")
    assert query["citations"], "query returned no citations"
    assert any("2026-01-20" in (citation.get("text") or "") for citation in query["citations"])
    assert all(citation.get("path") and citation.get("trust") for citation in query["citations"])
    assert not any(citation.get("title") == "Security review" for citation in query["citations"])

    source_path = evidence["path"]
    status, source = http_json(
        base, "GET", "/api/knowledge/source?" + urllib.parse.urlencode({"path": source_path})
    )
    assert status == 200
    assert_contract(source, required=("path", "title", "text", "origin", "trust"), label="source")
    assert source["path"] == source_path
    assert source["text"]

    private_request = urllib.request.Request(
        base + "/api/knowledge/source?" + urllib.parse.urlencode({"path": "security-review.md"})
    )
    with pytest.raises(urllib.error.HTTPError) as private_error:
        urllib.request.urlopen(private_request, timeout=10)
    assert private_error.value.code in {403, 404}

    # The source boundary must not turn a path traversal into a read primitive.
    traversal = urllib.parse.urlencode({"path": "../../etc/passwd"})
    request = urllib.request.Request(base + "/api/knowledge/source?" + traversal)
    with pytest.raises(urllib.error.HTTPError) as error:
        urllib.request.urlopen(request, timeout=10)
    assert error.value.code in {400, 403, 404}


def test_document_import_refresh_and_update_is_incremental(knowledge_server):
    base, vault, _ = knowledge_server
    original = b"# Imported Atlas\n\nThe first revision says the owner is platform.\n"
    status, imported = multipart_import(base, "atlas-import.md", original)
    assert status in {200, 201}
    assert_contract(imported, required=("path", "source_path", "title", "format"), label="import")
    assert imported["format"] in {"md", "markdown"}
    assert imported["source_path"]

    status, refreshed = http_json(base, "POST", "/api/documents/refresh", {"path": imported["path"]})
    assert status == 200
    assert refreshed["path"] == imported["path"]

    status, documents = http_json(base, "GET", "/api/documents")
    assert status == 200
    rows = documents.get("documents")
    assert isinstance(rows, list)
    row = next(item for item in rows if item.get("title") == imported["title"])
    assert row.get("status") in {"ready", "indexed", "ok", "complete"}

    # Repeating a filename without an explicit generated-note path must never
    # silently replace the existing import. Implementations may reject it or
    # allocate a distinct destination, but neither is an overwrite.
    duplicate_status, duplicate = multipart_import(base, "atlas-import.md", b"# Unauthorized replacement\n")
    if duplicate_status in {400, 409, 422}:
        assert "overwrite" in json.dumps(duplicate).lower() or "exist" in json.dumps(duplicate).lower()
    else:
        assert duplicate_status in {200, 201}
        assert duplicate.get("path") != imported["path"]

    updated = original.replace(b"first revision says the owner is platform", b"second revision says the owner is data")
    status, refreshed = multipart_import(base, "atlas-import.md", updated, path=imported["path"])
    assert status in {200, 201}
    assert refreshed["path"] == imported["path"]
    status, source = http_json(
        base, "GET", "/api/knowledge/source?" + urllib.parse.urlencode({"path": imported["path"]})
    )
    assert status == 200
    assert "second revision" in source["text"]
    assert "first revision" not in source["text"]

    # A replacement is a real vault artifact and not an arbitrary host-path read.
    assert (vault / imported["path"]).exists()
    assert "second revision" in (vault / imported["path"]).read_text()


def _open_knowledge(page, base):
    page.goto(base + "/")
    page.wait_for_load_state("domcontentloaded")
    if page.locator("#menu-open").is_visible():
        page.locator("#menu-open").click()
    page.locator("#knowledge-open").click()
    expect(page.locator("#knowledge-modal")).to_be_visible(timeout=10000)


def canvas_digest(page):
    return page.locator(".knowledge-canvas").evaluate(
        """canvas => {
            const data = canvas.getContext('2d').getImageData(0, 0, canvas.width, canvas.height).data;
            let hash = 2166136261;
            for (let i = 0; i < data.length; i += 1) hash = Math.imul(hash ^ data[i], 16777619);
            return hash >>> 0;
        }"""
    )


@pytest.mark.parametrize("page", [{"width": 1440, "height": 960}, {"width": 390, "height": 844}], indirect=True, ids=["desktop", "mobile"])
def test_react_knowledge_explorer_browses_evidence_and_filters(page, knowledge_server):
    base, _, _ = knowledge_server
    _open_knowledge(page, base)
    page.wait_for_timeout(500)
    modal = page.locator("#knowledge-modal")
    body_text = modal.inner_text()
    assert "evidence" in body_text.lower() or "knowledge" in body_text.lower()
    assert modal.evaluate("element => element.scrollWidth <= element.clientWidth + 1"), "knowledge modal has horizontal overflow"

    # Query from the real browser, then inspect the rendered citations/highlight.
    question = page.get_by_label("Ask the corpus")
    expect(question).to_be_visible()
    question.fill("When is the Atlas launch review?")
    page.get_by_role("button", name="Ask with citations").click()
    expect(page.locator(".knowledge-answer")).to_contain_text("2026-01-20", timeout=15000)
    expect(page.locator(".citation").first).to_be_visible()
    assert page.get_by_text(re.compile("Launch plan", re.I)).count()

    # Selecting a citation highlights the connected graph evidence and exposes
    # the cited document in the inspector.
    before_selection = canvas_digest(page)
    page.locator(".citation").filter(has_text="Launch plan").first.click()
    expect(page.locator(".knowledge-inspector")).to_contain_text("Launch plan", timeout=8000)
    page.wait_for_timeout(100)
    assert canvas_digest(page) != before_selection, "citation selection did not redraw highlighted graph"
    page.get_by_role("button", name="Expand source").click()
    expect(page.locator(".source-expanded blockquote")).to_contain_text("rollback window", timeout=8000)

    # Relationship/date controls must affect real requests and rendered state,
    # not only local labels.
    date_control = page.locator("input[type=date]")
    expect(date_control).to_have_count(2)
    date_control.nth(0).fill("2026-02-01")
    date_control.nth(1).fill("2026-02-28")
    with page.expect_request(re.compile(r"/api/knowledge/query")) as dated_request:
        page.get_by_role("button", name="Ask with citations").click()
    expect(page.locator(".knowledge-answer")).not_to_contain_text("2026-01-20", timeout=10000)
    dated_body = json.loads(dated_request.value.post_data or "{}")
    assert dated_body["after"] == "2026-02-01"
    assert dated_body["before"] == "2026-02-28"

    # Selecting a node is an actual graph highlight and populates source
    # evidence in the inspector.
    page.get_by_role("button", name=re.compile("Refresh map", re.I)).click()
    canvas = page.locator(".knowledge-canvas")
    expect(canvas).to_be_visible()
    expect(canvas).to_have_attribute("aria-label", re.compile("Interactive 2d knowledge graph"))
    accessible_items = page.locator(".knowledge-accessible-list")
    expect(accessible_items).to_be_visible()
    first_node = accessible_items.get_by_role("button").first
    expect(first_node).to_be_visible()
    first_node.click()
    expect(page.locator(".knowledge-inspector")).not_to_contain_text("Select a node or relationship")

    # Relationship filtering must issue a filtered graph request and leave a
    # graph state whose visible edge labels match the selected relation.
    relation = page.get_by_label("Relationship")
    expect(relation).to_be_visible()
    expect(relation.locator("option")).to_have_count(2, timeout=10000)
    relation_value = relation.locator("option").nth(1).inner_text()
    assert relation_value
    with page.expect_response(re.compile(r"/api/knowledge/graph\?.*relation=")) as relation_response:
        relation.select_option(label=relation_value)
    assert f"relation={urllib.parse.quote(relation_value)}" in relation_response.value.url
    filtered_graph = relation_response.value.json()
    assert filtered_graph["edges"]
    assert all(edge["relation"] == relation_value for edge in filtered_graph["edges"])

    # The view toggle must change the actual map presentation, not just its
    # button label, and must be reversible.
    page.get_by_role("button", name="3D view").click()
    expect(page.get_by_role("button", name="2D view")).to_be_visible()
    canvas = page.locator(".knowledge-canvas")
    expect(canvas).to_be_visible()
    canvas.scroll_into_view_if_needed()
    box = canvas.bounding_box()
    assert box and box["width"] > 200 and box["height"] > 200
    viewport = page.viewport_size
    assert viewport
    start = (box["x"] + box["width"] * 0.82, box["y"] + box["height"] * 0.68)
    end = (box["x"] + box["width"] * 0.34, box["y"] + box["height"] * 0.38)
    assert all(0 <= coordinate < viewport[axis] for coordinate, axis in ((start[0], "width"), (start[1], "height"), (end[0], "width"), (end[1], "height")))
    before_orbit = canvas_digest(page)
    page.mouse.move(*start)
    page.mouse.down()
    page.mouse.move(*end, steps=8)
    page.mouse.up()
    page.wait_for_timeout(100)
    assert canvas_digest(page) != before_orbit, "3D orbit did not change projected graph rendering"
    page.get_by_role("button", name="2D view").click()
    modal.evaluate("element => { element.scrollTop = 0; element.scrollLeft = 0; }")
    screenshot_name = "desktop" if page.viewport_size["width"] > 700 else "mobile"
    page.screenshot(path=f"/tmp/grimoire-knowledge-{screenshot_name}.png", full_page=True)

    # Import through the actual browser file chooser, then refresh the same
    # document row. This exercises the UI-to-multipart path, not only HTTP.
    with page.expect_file_chooser() as chooser_info:
        page.get_by_role("button", name="Import document").click()
    chooser_info.value.set_files(str(Path(__file__).parent / "fixtures" / "public-handbook.md"))
    expect(page.locator(".document-row").filter(has_text="Public handbook")).to_be_visible(timeout=15000)
    page.locator(".document-row").filter(has_text="Public handbook").get_by_role("button", name="Refresh").click()
    page.screenshot(path=f"/tmp/grimoire-knowledge-{screenshot_name}-documents.png", full_page=True)
