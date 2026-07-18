"""Substrate trust surfaces in the browser: agent-memory provenance + badges,
retrieval inspection, and the memory palette entry."""
from playwright.sync_api import expect


def _remember(pg, text, topic, agent="probe-agent"):
    pg.evaluate(
        "([text, topic, agent]) => fetch('/api/memory', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({text, topic, agent})})", [text, topic, agent])


def test_memory_note_shows_badge_and_provenance(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    _remember(page, "the staging box is flaky on Mondays", "staging", "ops-agent")
    page.reload()
    page.wait_for_selector("body[data-ready]", timeout=10000)
    # badge in the (memory/ folder of the) note list
    folder = page.locator("details.folder", has_text="memory/")
    expect(folder).to_be_visible(timeout=8000)
    expect(folder.locator(".mem-badge")).to_be_visible()
    # open it → provenance banner with the writing agent + history link
    folder.locator(".note-row", has_text="Memory: staging").click()
    prov = page.locator("#provenance")
    expect(prov).to_be_visible(timeout=6000)
    expect(prov).to_contain_text("ops-agent")
    prov.locator("#prov-history").click()
    expect(page.locator("#history-modal")).to_be_visible()


def test_normal_note_has_no_provenance_banner(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    page.once("dialog", lambda d: d.accept("Ordinary"))
    page.click("#new-note")
    expect(page.locator("#title")).to_have_value("Ordinary", timeout=8000)
    expect(page.locator("#provenance")).to_be_hidden()


def test_retrieval_inspection_shows_agent_context(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    page.evaluate(
        "() => fetch('/api/notes', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({title:'Kubernetes Runbook',"
        "body:'restart the ingress with kubectl rollout restart'})})")
    page.wait_for_timeout(400)
    page.once("dialog", lambda d: d.accept("how do I restart the ingress"))
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "what would the agent see")
    page.keyboard.press("Enter")
    expect(page.locator("#inspect-modal")).to_be_visible(timeout=6000)
    chunk = page.locator(".inspect-chunk", has_text="Kubernetes Runbook")
    expect(chunk).to_be_visible(timeout=8000)
    expect(chunk.locator(".ic-score")).to_be_visible()
    # click-through opens the source note
    chunk.locator("a.wikilink").click()
    expect(page.locator("#title")).to_have_value("Kubernetes Runbook", timeout=6000)


def test_agent_memories_palette_entry(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    _remember(page, "remember me", "palette-probe")
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "agent memories")
    page.keyboard.press("Enter")
    expect(page.locator("#title")).to_have_value("Memory: palette-probe", timeout=8000)


def test_agent_briefing_surface(page, server):
    """The human console shows the same standing context get_briefing serves."""
    page.goto(server)
    page.wait_for_selector("body[data-ready]", timeout=10000)
    page.evaluate(
        "() => fetch('/api/notes', {method:'POST',"
        "headers:{'Content-Type':'application/json'},"
        "body: JSON.stringify({title:'Env Rules', body:'export APP_ENV=test #onboarding'})})")
    _remember(page, "vendor sandbox flaky, mock it", "ops-briefing")
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "agent briefing")
    page.keyboard.press("Enter")
    expect(page.locator("#inspect-title")).to_contain_text("briefing", timeout=6000)
    expect(page.locator("#inspect-body", )).to_contain_text("Onboarding", timeout=6000)
    expect(page.locator(".inspect-chunk", has_text="Env Rules")).to_be_visible()
    expect(page.locator(".inspect-chunk", has_text="ops-briefing")).to_be_visible()
