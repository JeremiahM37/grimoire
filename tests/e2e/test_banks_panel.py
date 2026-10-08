"""The Memory banks panel against a real server with no language model.

Retain without a model (`chunks` mode), browse what was stored, recall it with
a trace, reflect on the extractive path, manage directives and mental models,
and see the "needs a language model" answers handled as such."""
import time

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


def open_bank(page, server, bank=BANK):
    page.goto(server)
    page.wait_for_selector("body[data-ready]")
    page.click("#banks-open")
    expect(page.locator("#banks-modal")).to_be_visible()
    page.locator(f'#banks-list [data-bank="{bank}"]').click()
    expect(page.locator("#banks-title")).to_have_text(bank)


def tab(page, name):
    page.locator(f'.banks-tab[data-tab="{name}"]').click()
    expect(page.locator(f'[data-testid="banks-tab-{name}"]')).to_be_visible()


def wait_ops(page, server, bank, timeout=20):
    """Wait until the bank has no queued or running operation."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        ops = page.request.get(f"{server}/api/banks/{bank}/operations?limit=200").json()["operations"]
        if all(o["status"] in ("completed", "failed", "cancelled") for o in ops):
            return ops
        time.sleep(0.2)
    raise AssertionError(f"operations still running: {ops}")


def test_bank_panel_recall_reflect_and_browse(page, server):
    seed(page, server)
    open_bank(page, server)
    expect(page.locator("#banks-stats")).to_contain_text("3 facts")
    expect(page.locator("#banks-stats")).to_contain_text("no language model")

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

    # With no model, reflect quotes what recall finds and cites it.
    page.click("#banks-reflect")
    reflect = page.locator("#banks-reflect-results")
    expect(reflect).to_have_attribute("data-mode", "extractive")
    expect(page.locator('[data-testid="reflect-answer"]')).to_contain_text("Pixel")
    cites = page.locator("#banks-citations [data-cite]")
    expect(cites.first).to_be_visible()
    cited = cites.first.get_attribute("data-cite")
    facts = page.request.get(f"{server}/api/banks/{BANK}/memories").json()["items"]
    assert cited in {f["id"] for f in facts}
    expect(page.locator("#banks-reflect-trace")).to_have_count(0)  # a trace only when asked for

    tab(page, "documents")
    expect(page.locator("#banks-documents tbody tr")).to_have_count(3)
    page.locator('#banks-documents [data-doc="pets"] a').click()
    expect(page.locator("#banks-document .banks-chunk").first).to_contain_text("grey cat")

    tab(page, "entities")
    expect(page.locator('[data-testid="banks-tab-entities"]')).not_to_contain_text("Loading")

    # Consolidation needs a model: the tab says so in words, not as a raw 409.
    tab(page, "observations")
    expect(page.locator("#banks-observations")).to_contain_text("No observations yet")
    page.click("#banks-consolidate")
    expect(page.locator(".banks-model-required")).to_contain_text("needs a language model")

    tab(page, "operations")
    expect(page.locator('[data-testid="banks-tab-operations"]')).not_to_contain_text("Loading")
    expect(page.locator('[data-testid="unavailable"]')).to_have_count(0)


def test_retain_in_background_shows_an_operation(page, server):
    seed(page, server)
    open_bank(page, server)
    tab(page, "documents")
    form = page.locator("#banks-retain")
    form.locator('textarea[name="content"]').fill("Bruno repaired the boiler on Tuesday.")
    form.locator('input[name="document_id"]').fill("boiler")
    form.locator('select[name="mode"]').select_option("chunks")
    form.locator('input[name="async"]').check()
    form.locator('button[type="submit"]').click()
    expect(page.locator("#banks-retain-status")).to_contain_text("Queued: operation")
    wait_ops(page, server, BANK)
    tab(page, "operations")
    row = page.locator('#banks-operations tr[data-kind="retain"]').first
    expect(row).to_have_attribute("data-status", "completed")
    row.locator("a").click()
    expect(page.locator("#banks-operation")).to_contain_text("boiler")
    facts = page.request.get(f"{server}/api/banks/{BANK}/memories?document_id=boiler").json()
    assert facts["total"] == 1


def test_create_bank_from_template_and_edit_profile(page, server):
    page.request.delete(f"{server}/api/banks/e2e-coding")
    page.goto(server)
    page.wait_for_selector("body[data-ready]")
    page.click("#banks-open")
    form = page.locator("#banks-create")
    form.locator('input[name="bank_id"]').fill("e2e-coding")
    form.locator('select[name="template"]').select_option("coding-agent")
    preview = page.locator('[data-testid="template-preview"]')
    expect(preview).to_contain_text("Project context")
    expect(preview).to_contain_text("Cite decisions")
    form.locator('textarea[name="mission"]').fill("Remember how e2e-coding is built.")
    form.locator('button[type="submit"]').click()
    expect(page.locator("#banks-title")).to_have_text("e2e-coding")
    profile = page.locator("#banks-profile")
    expect(profile.locator('input[name="literalism"]')).to_have_value("5")
    expect(profile.locator('textarea[name="mission"]')).to_have_value("Remember how e2e-coding is built.")
    profile.locator('input[name="observations_mission"]').fill("Track build commands.")
    page.click("#banks-profile-save")
    expect(profile).to_contain_text("Saved")
    got = page.request.get(f"{server}/api/banks/e2e-coding").json()
    assert got["disposition"]["literalism"] == 5
    assert "technical decisions" in got["retain_mission"]
    assert got["config"]["observations_mission"] == "Track build commands."
    # The template's directive survived the profile save (directives are not
    # part of the profile form any more).
    assert any(d.get("name") == "Cite decisions" for d in got["directives"])

    # The import created the template's models, unanswered (no model here).
    tab(page, "models")
    tree = page.locator("#banks-model-tree")
    for mid in ("project-context", "developer-preferences", "review-patterns"):
        expect(tree.locator(f'[data-model="{mid}"]')).to_be_visible()
    tree.locator('[data-model="project-context"]').click()
    expect(page.locator('[data-testid="model-meta"]')).to_contain_text("never refreshed")


def test_directives_crud(page, server):
    seed(page, server)
    open_bank(page, server)
    tab(page, "directives")
    form = page.locator("#banks-directive-create")
    form.locator('input[name="text"]').fill("Never guess a version number.")
    form.locator('input[name="name"]').fill("No guessing")
    form.locator('input[name="tags"]').fill("release")
    form.locator('input[name="priority"]').fill("2")
    form.locator('button[type="submit"]').click()
    table = page.locator("#banks-directives")
    expect(table).to_contain_text("No guessing")
    items = page.request.get(f"{server}/api/banks/{BANK}/directives?active_only=false").json()["items"]
    (d,) = [x for x in items if x.get("name") == "No guessing"]
    assert d["tags"] == ["release"] and d["priority"] == 2

    row = table.locator(f'tr[data-directive="{d["id"]}"]')
    row.get_by_role("button", name="Edit").click()
    row.get_by_label("Directive text").fill("Never guess a version number; look it up.")
    row.get_by_role("button", name="Save").click()
    expect(row).to_contain_text("look it up")
    row.get_by_label("Active").click()  # controlled: it flips once the server has answered
    expect(row.get_by_label("Active")).not_to_be_checked()
    got = page.request.get(f"{server}/api/banks/{BANK}/directives?active_only=false").json()["items"]
    (d2,) = [x for x in got if x["id"] == d["id"]]
    assert d2["text"] == "Never guess a version number; look it up." and d2.get("inactive") is True
    assert not [x for x in page.request.get(f"{server}/api/banks/{BANK}/directives").json()["items"] if x["id"] == d["id"]]

    page.once("dialog", lambda dialog: dialog.accept())
    row.get_by_role("button", name="Delete").click()
    expect(table.locator(f'tr[data-directive="{d["id"]}"]')).to_have_count(0)


def test_mental_model_without_a_language_model(page, server):
    seed(page, server)
    open_bank(page, server)
    tab(page, "models")
    form = page.locator("#banks-model-create")
    form.locator('input[name="name"]').fill("Pets")
    form.locator('textarea[name="question"]').fill("Which pets do people have?")
    form.locator('input[name="id"]').fill("pets")
    form.locator('input[name="folder"]').fill("household")
    form.locator('button[type="submit"]').click()
    expect(page.locator("#banks-models-note")).to_contain_text("No language model is configured")
    model = page.locator("#banks-model")
    expect(model).to_be_visible()
    model_id = model.get_attribute("data-model")
    assert model_id == "household/pets"

    # Refresh needs a model: model_required is shown as words, not as a failure code.
    page.click("#banks-model-refresh")
    expect(model.locator(".banks-model-required")).to_contain_text("needs a language model")

    # A person writes the answer instead; it is theirs.
    page.click("#banks-model-edit")
    page.locator('#banks-model-editor textarea[name="body"]').fill("Alice has a grey cat called Pixel.")
    page.locator("#banks-model-editor button[type=submit]").click()
    expect(page.locator('[data-testid="model-body"]')).to_have_text("Alice has a grey cat called Pixel.")
    got = page.request.get(f"{server}/api/banks/{BANK}/mental-models/{model_id.replace('/', '%2F')}").json()
    assert got["authority"] == "human" and got["body"].strip() == "Alice has a grey cat called Pixel."

    # Moving it to another folder changes its id; the panel follows it.
    page.locator('.banks-move input[name="folder"]').fill("animals")
    page.locator(".banks-move button[type=submit]").click()
    expect(page.locator("#banks-model")).to_have_attribute("data-model", "animals/pets")
    expect(page.locator('#banks-model-tree [data-model="animals/pets"]')).to_be_visible()
    tree = page.request.get(f"{server}/api/banks/{BANK}/mental-models-tree").json()["roots"]
    assert any(n["kind"] == "folder" and n["name"] == "animals" for n in tree)


def test_banks_panel_fits_a_phone(browser, server):
    ctx = browser.new_context(viewport={"width": 390, "height": 844}, service_workers="block")
    ctx.add_init_script("localStorage.setItem('grimoire-editor-mode', 'classic')")
    page = ctx.new_page()
    try:
        seed(page, server)
        page.goto(server)
        page.wait_for_selector("body[data-ready]")
        page.click("#tabbar >> text=More")
        page.click("#more-sheet >> text=Banks")
        page.locator(f'#banks-list [data-bank="{BANK}"]').click()
        for name in ("playground", "memories", "models", "directives", "operations"):
            tab(page, name)
            if name == "playground":
                page.fill("#banks-query", "Pixel")
                page.click("#banks-recall")
                expect(page.locator("#banks-recall-results")).to_be_visible()
            overflow = page.evaluate(
                "() => [document.documentElement.scrollWidth, window.innerWidth,"
                " document.querySelector('#banks-modal .modal-box').getBoundingClientRect().right]")
            assert overflow[0] <= overflow[1], (name, overflow)
            assert overflow[2] <= overflow[1] + 1, (name, overflow)
    finally:
        ctx.close()
