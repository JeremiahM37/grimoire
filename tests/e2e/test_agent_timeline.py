"""The joined agent timeline, in a real browser.

The backend join is covered by Go tests. What is not covered anywhere else is
whether a person can actually read the sequence: that the three kinds arrive in
one list, that a refusal is visibly a refusal, and that a locked vault says its
third is missing instead of quietly showing two thirds as if that were all of
it. A timeline that silently drops a leg is worse than no timeline, because it
reads as "the agent did nothing".

The session vault is shared, so every note here is namespaced and the module
cleans up after itself.
"""
import urllib.request

import pytest
from playwright.sync_api import expect


@pytest.fixture(scope="module", autouse=True)
def _cleanup(server):
    yield
    for path in ("memory/e2e-timeline.md", "e2e-timeline-runbook.md"):
        req = urllib.request.Request(f"{server}/api/notes/{path}", method="DELETE")
        try:
            urllib.request.urlopen(req, timeout=10).close()
        except Exception:  # noqa: BLE001
            pass


def _ready(pg, server):
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)


def _palette(pg, query):
    pg.keyboard.press("Control+k")
    pg.fill("#palette-input", query)
    pg.keyboard.press("Enter")


def _remember(pg, text):
    pg.evaluate(
        "(text) => fetch('/api/memory', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({text, topic:'e2e-timeline', agent:'e2e-timeline-agent',"
        "task:'timeline check'})})", text)
    pg.wait_for_timeout(300)


def test_the_timeline_shows_what_an_agent_remembered(page, server):
    _ready(page, server)
    _remember(page, "the e2e timeline host is prod-timeline")

    _palette(page, "everything your agents did")
    body = page.locator("#inspect-body")
    expect(body).to_be_visible()
    # The actor and the fact both have to be there: a timeline that says
    # "something happened" is not a timeline.
    expect(body).to_contain_text("e2e-timeline-agent", timeout=10000)
    expect(body).to_contain_text("prod-timeline")


def test_a_locked_vault_says_its_third_is_missing(page, server):
    """The failure this guards against is a silent short list."""
    _ready(page, server)
    _remember(page, "the e2e timeline gateway restarts nightly")

    # The e2e server runs with a locked (or uninitialised) vault, which is
    # exactly the state a reader must be told about rather than left to infer.
    locked = page.evaluate(
        "async () => (await (await fetch('/api/timeline?limit=5')).json())"
        ".credentials_hidden")

    _palette(page, "everything your agents did")
    body = page.locator("#inspect-body")
    expect(body).to_be_visible()
    if locked:
        expect(body).to_contain_text("hidden while the vault is locked",
                                     timeout=10000)
    else:
        # If a later change unlocks the fixture vault, the assertion inverts
        # rather than silently passing on a branch it never took.
        expect(body).not_to_contain_text("hidden while the vault is locked")


def test_the_kind_filter_does_not_leak_other_kinds(page, server):
    _ready(page, server)
    _remember(page, "the e2e timeline filter fact")
    kinds = page.evaluate(
        "async () => { const d = await (await fetch("
        "'/api/timeline?kind=memory&limit=50')).json();"
        " return [...new Set(d.events.map(e => e.kind))]; }")
    assert kinds in ([], ["memory"]), f"kind filter leaked: {kinds}"
