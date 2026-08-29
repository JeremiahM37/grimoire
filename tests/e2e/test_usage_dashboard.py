"""The AI usage dashboard, in a real browser.

The rollups and the pricing are covered by Go tests. What is not covered
anywhere else is whether the screen a person actually looks at tells them the
truth about money — and this is a screen where being quietly wrong is
expensive:

  - the scope caption must be ON the page. The number is NOT an agent's total
    AI spend, and a dollar figure shown without that sentence is a claim the
    software cannot support.
  - an unpriced model must read as unknown, never as $0.00. A zero rendered as
    a total makes an unmetered provider look free, which is the expensive
    direction to be wrong in.
  - a total containing an unpriced call must say "at least".

The session vault is shared, so everything here is namespaced and the module
cleans up after itself.
"""
import sqlite3
import urllib.request
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest
from conftest import PHONE  # conftest dir is on sys.path
from playwright.sync_api import expect


@pytest.fixture(scope="module", autouse=True)
def _cleanup(server):
    yield
    req = urllib.request.Request(
        f"{server}/api/notes/memory/e2e-usage.md", method="DELETE")
    try:
        urllib.request.urlopen(req, timeout=10).close()
    except Exception:  # noqa: BLE001
        pass


def _ready(pg, server):
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)


def _open_usage(pg):
    pg.keyboard.press("Control+k")
    pg.fill("#palette-input", "AI usage")
    pg.keyboard.press("Enter")
    expect(pg.locator("#inspect-modal")).to_be_visible(timeout=5000)


def test_the_dashboard_opens_from_the_palette(page, server):
    _ready(page, server)
    _open_usage(page)
    expect(page.locator("#inspect-title")).to_contain_text("AI usage")
    # It must render something rather than sit on "Loading…" forever, which is
    # what a broken fetch looks like to a user.
    expect(page.locator("#inspect-body")).not_to_contain_text("Loading…", timeout=5000)


def test_the_scope_caveat_is_on_the_page(page, server):
    """The number is not an agent's total spend, and the page has to say so."""
    _ready(page, server)
    _open_usage(page)
    body = page.locator("#inspect-body")
    expect(body).to_contain_text("What this is", timeout=5000)
    # The specific claim that must never be implied: this is Grimoire's own
    # calls, not the agent's conversation with its provider.
    expect(body).to_contain_text("Grimoire made itself")
    expect(body).to_contain_text("not pass through this server")


def test_an_empty_ledger_explains_itself(page, server):
    """A fresh install has no usage; that must read as an explanation, not a fault."""
    _ready(page, server)
    _open_usage(page)
    body = page.locator("#inspect-body")
    expect(body).to_contain_text("Prices last checked", timeout=5000)
    # No error, no stack, no blank panel.
    expect(body).not_to_contain_text("undefined")
    expect(body).not_to_contain_text("NaN")


def _seed(calls):
    """Write model-call rows straight into the ledger the dashboard reads.

    Directly rather than through an endpoint, because the alternative is a
    test-only route on a production server — and the only honest way to
    generate these through the app is a real paid API call. WAL plus the
    busy timeout make a second writer safe.
    """
    import conftest
    db = Path(conftest.VAULT) / ".grimoire" / "index.db"
    con = sqlite3.connect(db, timeout=10)
    now = datetime.now(UTC)
    try:
        for i, c in enumerate(calls):
            cost, known = _price(c)
            con.execute(
                "INSERT INTO model_calls(at,provider,model,surface,agent,"
                "input_tokens,output_tokens,latency_ms,cost,cost_known,error) "
                "VALUES(?,?,?,?,?,?,?,?,?,?,'')",
                ((now - timedelta(minutes=i)).isoformat(),
                 c["provider"], c["model"], c.get("surface", ""), c.get("agent", ""),
                 c["input_tokens"], c["output_tokens"], 120, cost, known))
        con.commit()
    finally:
        con.close()


# The prices the Go table uses, for the two models these tests assert on. Kept
# here deliberately: a test that recomputed the cost from the code under test
# would pass whatever that code did.
_PRICES = {
    ("anthropic", "claude-sonnet-5"): (3.00, 15.00),
    ("ollama", "qwen3.5:4b"): (0.0, 0.0),
}


def _price(c):
    p = _PRICES.get((c["provider"], c["model"]))
    if p is None:
        return 0.0, 0          # unpriced: cost_known = 0
    return (c["input_tokens"] / 1e6 * p[0]
            + c["output_tokens"] / 1e6 * p[1]), 1


def test_costs_and_rollups_render(page, server):
    _ready(page, server)
    _seed([
        {"provider": "anthropic", "model": "claude-sonnet-5", "surface": "ask",
         "agent": "e2e-usage-agent", "input_tokens": 1000000, "output_tokens": 100000},
        {"provider": "ollama", "model": "qwen3.5:4b", "surface": "intent",
         "agent": "e2e-usage-agent", "input_tokens": 500, "output_tokens": 50},
    ])
    _open_usage(page)
    body = page.locator("#inspect-body")
    expect(body).to_contain_text("By provider", timeout=5000)
    expect(body).to_contain_text("anthropic")
    expect(body).to_contain_text("ollama")
    # A million input tokens of sonnet is $3, plus 100k output at $15/M = $1.50.
    expect(body).to_contain_text("$4.50")
    # Surfaces are what make a bill actionable — one number is not.
    expect(body).to_contain_text("By surface")
    expect(body).to_contain_text("ask")
    expect(body).to_contain_text("intent")


def test_an_unpriced_model_is_not_shown_as_free(page, server):
    """A zero total makes an unmetered provider look free. It must read unknown."""
    _ready(page, server)
    _seed([
        {"provider": "openrouter", "model": "anthropic/claude-sonnet-5",
         "surface": "ask", "input_tokens": 900000, "output_tokens": 900000},
    ])
    _open_usage(page)
    body = page.locator("#inspect-body")
    expect(body).to_contain_text("no price on file", timeout=5000)
    # And the headline total must be hedged rather than stated flat.
    expect(body).to_contain_text("at least")
    # The specific failure this guards: a routed model rendering as $0.00 in the
    # provider table as though it had been free.
    expect(body.locator("text=not priced").first).to_be_visible()


def test_agents_are_listed_with_what_they_did(page, server):
    _ready(page, server)
    page.evaluate(
        "() => fetch('/api/memory', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({text:'the e2e usage host is prod-usage',"
        "topic:'e2e-usage', agent:'e2e-usage-agent', task:'usage check'})})")
    page.wait_for_timeout(400)
    _open_usage(page)
    body = page.locator("#inspect-body")
    expect(body).to_contain_text("Agents in this knowledge base", timeout=5000)
    expect(body).to_contain_text("e2e-usage-agent")


def test_the_window_picker_changes_the_query(page, server):
    _ready(page, server)
    _open_usage(page)
    body = page.locator("#inspect-body")
    expect(body).to_contain_text("Prices last checked", timeout=5000)
    page.locator(".usage-win", has_text="24 hours").click()
    # The clicked window becomes the selected one, and the panel re-renders
    # rather than blanking.
    expect(page.locator(".usage-win.on")).to_contain_text("24 hours", timeout=5000)
    expect(body).to_contain_text("Prices last checked")


@pytest.mark.parametrize("page", [PHONE], indirect=True, ids=["phone"])
def test_it_reads_on_a_phone(page, server):
    """The console is a PWA people open on a phone; a table that overflows there
    is a table nobody can read."""
    _ready(page, server)
    _open_usage(page)
    expect(page.locator("#inspect-body")).to_contain_text(
        "Prices last checked", timeout=5000)
    overflow = page.evaluate(
        "() => document.documentElement.scrollWidth - document.documentElement.clientWidth")
    assert overflow <= 2, f"the page scrolls sideways by {overflow}px on a phone"
