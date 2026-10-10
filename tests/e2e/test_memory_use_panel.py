"""The Memory use panel against seeded trace data: overview, per-memory card,
the note menu entry, and the empty and admin-hint states."""
import conftest
import memory_use_seed
import pytest
from memory_use_seed import RULE, seed
from playwright.sync_api import expect


@pytest.fixture(scope="module")
def seeded(server):
    seed(server, conftest.VAULT)
    return server


def open_overview(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]")
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "memory use")
    page.locator("#palette-list .pal-item", has_text="Memory use").first.click()
    expect(page.locator("#memuse-modal")).to_be_visible()


def test_overview_numbers_and_lists(page, seeded):
    open_overview(page, seeded)
    expect(page.locator("#memuse-exposures")).to_contain_text("39")
    expect(page.locator("#memuse-uptake")).to_contain_text("%")
    expect(page.locator("#memuse-holdout")).to_contain_text("off")
    expect(page.locator('[data-testid="memuse-headline"] .memuse-bar')).to_be_visible()
    expect(page.locator("#memuse-ignored")).to_contain_text("fact ")
    expect(page.locator("#memuse-violated")).to_contain_text("run-tests-before-commit")
    expect(page.locator("#memuse-helpful")).to_contain_text("never-force-push")
    rules = page.locator('[data-testid="memuse-rules"]')
    expect(rules).to_contain_text("Never force-push to main")
    expect(rules).to_contain_text("97%")
    expect(rules).to_contain_text("asks first")


def test_card_funnel_evidence_benefit_and_actions(page, seeded):
    open_overview(page, seeded)
    page.locator(f'#memuse-helpful [data-target="{RULE}"]').click()
    expect(page.locator("#memuse-title")).to_have_text("Agent Memory/never-force-push.md")
    funnel = page.locator('[data-testid="memuse-funnel"] li')
    expect(funnel).to_have_count(5)
    expect(funnel.nth(0)).to_contain_text("n=16")
    expect(funnel.nth(1)).to_contain_text("n=")
    ev = page.locator('[data-testid="memuse-evidence"]')
    for kind in ("tag", "fp", "check", "changed"):
        expect(ev.locator(f'[data-evidence="{kind}"]')).to_be_visible()
    expect(page.locator('[data-testid="memuse-benefit-label"]')).to_have_text("associated")
    expect(page.locator('[data-testid="memuse-benefit"]')).to_contain_text("95% CI")
    expect(page.locator('[data-testid="memuse-reminders"]')).to_contain_text("ran unchanged")
    expect(page.locator('[data-testid="memuse-causal"]')).to_contain_text("insufficient data")
    actions = page.locator('[data-testid="memuse-actions"] li')
    expect(actions.first).to_be_visible()
    assert actions.count() == 10
    # Codes only: nothing in the list looks like a command or a path.
    assert "/" not in page.locator('[data-testid="memuse-actions"]').inner_text().replace("-", "")
    page.click("#memuse-back")
    expect(page.locator('[data-testid="memuse-overview-view"]')).to_be_visible()


def test_unlinked_memory_says_insufficient_data(page, seeded):
    page.goto(seeded)
    page.wait_for_selector("body[data-ready]")
    page.keyboard.press("Control+k")
    page.fill("#palette-input", "memory use")
    page.locator("#palette-list .pal-item", has_text="Memory use").first.click()
    page.locator(f'#memuse-ignored [data-target="{memory_use_seed.FACT}"]').click()
    expect(page.locator('[data-testid="memuse-benefit-label"]')).to_have_text("insufficient data")
    expect(page.locator('[data-testid="memuse-actions"], .vault-note', has_text="No linked actions yet")).to_be_visible()


def test_card_from_the_note_menu_and_empty_state(page, seeded):
    page.goto(seeded)
    page.wait_for_selector("body[data-ready]")
    page.request.post(f"{seeded}/api/notes", data={"path": "plain.md", "body": "# Plain\n\nnothing"})
    page.goto(seeded)
    page.wait_for_selector("body[data-ready]")
    page.locator('#note-list [data-path="plain.md"], .note-row:has-text("Plain")').first.click()
    page.click("#note-more")
    page.click("#memuse-btn")
    expect(page.locator('[data-testid="memuse-card-empty"]')).to_be_visible()
    expect(page.locator("#memuse-title")).to_have_text("plain.md")


def test_phone_layout_fits(browser, seeded):
    ctx = browser.new_context(viewport={"width": 390, "height": 844}, service_workers="block")
    page = ctx.new_page()
    open_overview(page, seeded)
    width = page.evaluate("document.querySelector('#memuse-modal .banks-body').scrollWidth - document.querySelector('#memuse-modal .banks-body').clientWidth")
    assert width <= 1
    page.locator(f'#memuse-helpful [data-target="{RULE}"]').click()
    expect(page.locator('[data-testid="memuse-funnel"]')).to_be_visible()
    ctx.close()


