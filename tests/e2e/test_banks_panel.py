"""The Memory banks panel against a real server: retain without a model
(`chunks` mode), browse what was stored, and recall it with a trace."""
from playwright.sync_api import expect

BANK = "e2e-banks"
DOCS = [
    ("trip", "Dana moved the database migration to Friday because the vendor was late."),
    ("prefs", "The user prefers tabs over spaces and reviews pull requests in the morning."),
    ("pets", "Alice adopted a grey cat named Pixel last spring."),
]


def seed(page, server):
    page.request.delete(f"{server}/api/banks/{BANK}")
    created = page.request.post(f"{server}/api/banks", data={
        "bank_id": BANK, "name": "E2E bank", "config": {"retain_extraction_mode": "chunks"}})
    assert created.status == 201, created.text()
    for doc, text in DOCS:
        r = page.request.post(f"{server}/api/banks/{BANK}/memories", data={
            "mode": "chunks", "items": [{"content": text, "document_id": doc, "tags": ["e2e"],
                                          "timestamp": "2024-05-02T10:00:00Z"}]})
        assert r.ok, r.text()


def open_bank(page, server):
    page.goto(server)
    page.wait_for_selector("body[data-ready]")
    page.click("#banks-open")
    expect(page.locator("#banks-modal")).to_be_visible()
    page.locator(f'#banks-list [data-bank="{BANK}"]').click()
    expect(page.locator("#banks-title")).to_have_text(BANK)


def tab(page, name):
    page.locator(f'.banks-tab[data-tab="{name}"]').click()
    expect(page.locator(f'[data-testid="banks-tab-{name}"]')).to_be_visible()


def test_bank_panel_recall_and_browse(page, server):
    seed(page, server)
    open_bank(page, server)

    tab(page, "memories")
    rows = page.locator("#banks-memories tbody tr")
    expect(rows).to_have_count(3)
    expect(page.locator("#banks-memories")).to_contain_text("grey cat named Pixel")

    tab(page, "playground")
    page.fill("#banks-query", "which pet did Alice adopt?")
    page.click("#banks-recall")
    results = page.locator("#banks-recall-results")
    expect(results.locator("tbody tr").first).to_contain_text("Pixel")
    expect(page.locator('[data-testid="trace-summary"]')).to_contain_text("Tokens used")
    expect(page.locator("#banks-ranks")).to_be_visible()

    # Reflect is not on this server yet: it says so instead of failing.
    page.click("#banks-reflect")
    expect(page.locator('[data-testid="unavailable"]')).to_contain_text("not available")

    tab(page, "documents")
    expect(page.locator("#banks-documents tbody tr")).to_have_count(3)
    page.locator('#banks-documents [data-doc="pets"] a').click()
    expect(page.locator("#banks-document .banks-chunk").first).to_contain_text("grey cat")

    tab(page, "entities")
    expect(page.locator('[data-testid="banks-tab-entities"]')).not_to_contain_text("Loading")

    for name in ("observations", "models", "operations"):
        tab(page, name)
        expect(page.locator('[data-testid="unavailable"]')).to_be_visible()


def test_create_bank_from_template_and_edit_profile(page, server):
    page.request.delete(f"{server}/api/banks/e2e-coding")
    page.goto(server)
    page.wait_for_selector("body[data-ready]")
    page.click("#banks-open")
    form = page.locator("#banks-create")
    form.locator('input[name="bank_id"]').fill("e2e-coding")
    form.locator('select[name="template"]').select_option("coding-agent")
    form.locator('button[type="submit"]').click()
    expect(page.locator("#banks-title")).to_have_text("e2e-coding")
    profile = page.locator("#banks-profile")
    expect(profile.locator('input[name="literalism"]')).to_have_value("5")
    page.fill("#banks-new-directive", "Never guess a version number")
    profile.get_by_role("button", name="Add").click()
    page.click("#banks-profile-save")
    expect(profile).to_contain_text("Saved")
    got = page.request.get(f"{server}/api/banks/e2e-coding").json()
    assert got["disposition"]["literalism"] == 5
    assert any(d["text"] == "Never guess a version number" for d in got["directives"])
    assert "technical decisions" in got["retain_mission"]


def test_banks_panel_fits_a_phone(browser, server):
    ctx = browser.new_context(viewport={"width": 390, "height": 844}, service_workers="block")
    ctx.add_init_script("localStorage.setItem('grimoire-editor-mode', 'classic')")
    page = ctx.new_page()
    try:
        seed(page, server)
        page.goto(server)
        page.wait_for_selector("body[data-ready]")
        page.click("#menu-open")
        page.click("#banks-open")
        page.locator(f'#banks-list [data-bank="{BANK}"]').click()
        for name in ("playground", "memories"):
            tab(page, name)
            if name == "playground":
                page.fill("#banks-query", "Pixel")
                page.click("#banks-recall")
                expect(page.locator("#banks-recall-results")).to_be_visible()
            overflow = page.evaluate(
                "() => [document.documentElement.scrollWidth, window.innerWidth,"
                " document.querySelector('#banks-modal .modal-box').getBoundingClientRect().right]")
            assert overflow[0] <= overflow[1], overflow
            assert overflow[2] <= overflow[1] + 1, overflow
    finally:
        ctx.close()
