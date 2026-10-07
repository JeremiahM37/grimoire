"""The full memory-bank path with a language model, against a real server.

The model is the stub in llm_stub.py (an OpenAI-compatible server answering
with canned, input-driven JSON), so this runs offline and deterministically:
retain → extraction → consolidation into observations → a mental model
written by refresh → a person edits it → the next refresh files a proposal
the person accepts or rejects → a person's observation survives
consolidation → reflect cites what it used."""
import time
import urllib.parse

from playwright.sync_api import expect
from test_banks_panel import tab

BANK = "e2e-reason"
SESSIONS = [
    ("s1", "Dana leads the billing team at Northwind. Dana prefers written proposals over meetings."),
    ("s2", "Omar joined the billing team in March. Omar is learning the invoicing service from Dana."),
]


def wait_ops(page, base, bank, timeout=30):
    deadline = time.time() + timeout
    ops = []
    while time.time() < deadline:
        ops = page.request.get(f"{base}/api/banks/{bank}/operations?limit=200").json()["operations"]
        if ops and all(o["status"] in ("completed", "failed", "cancelled") for o in ops):
            return ops
        time.sleep(0.2)
    raise AssertionError(f"operations still running: {ops}")


def model_url(base, bank, mid):
    return f"{base}/api/banks/{bank}/mental-models/{urllib.parse.quote(mid, safe='')}"


def open_bank(page, base):
    page.goto(base)
    page.wait_for_selector("body[data-ready]")
    page.click("#banks-open")
    page.locator(f'#banks-list [data-bank="{BANK}"]').click()
    expect(page.locator("#banks-title")).to_have_text(BANK)


def test_full_path_with_a_model(page, llm_server, llm_stub):
    base, _vault = llm_server
    page.request.delete(f"{base}/api/banks/{BANK}")
    r = page.request.post(f"{base}/api/banks", data={"bank_id": BANK, "name": "Reasoning",
                                                     "mission": "Know the billing team.", "config": {"consolidation": "auto"}})
    assert r.status == 201, r.text()

    # ---- retain: the model extracts facts; consolidation follows on its own.
    for doc, text in SESSIONS:
        r = page.request.post(f"{base}/api/banks/{BANK}/memories", data={
            "items": [{"content": text, "document_id": doc, "timestamp": "2024-05-02T10:00:00Z"}]})
        assert r.ok, r.text()
    # The memories list holds observations too, once consolidation has run.
    facts = [f for f in page.request.get(f"{base}/api/banks/{BANK}/memories").json()["items"] if f["type"] != "observation"]
    assert len(facts) == 4, facts
    assert "extract" in llm_stub.kinds()
    ops = wait_ops(page, base, BANK)
    assert any(o["kind"] == "consolidation" and o["status"] == "completed" for o in ops), ops
    obs = page.request.get(f"{base}/api/banks/{BANK}/observations").json()
    assert obs["total"] >= 2, obs
    fact_ids = {f["id"] for f in facts}
    assert all(set(o["source_fact_ids"]) <= fact_ids and o["source_fact_ids"] for o in obs["items"])

    open_bank(page, base)
    tab(page, "observations")
    cards = page.locator("#banks-observations [data-observation]")
    expect(cards.first).to_be_visible()
    expect(page.locator("#banks-observations")).to_contain_text("Dana")
    cards.first.locator("summary").click()
    expect(cards.first.locator(".banks-evidence code").first).to_be_visible()

    # ---- a mental model, written by a refresh.
    tab(page, "models")
    form = page.locator("#banks-model-create")
    form.locator('input[name="name"]').fill("Billing team")
    form.locator('textarea[name="question"]').fill("Who is on the billing team and how do they work?")
    form.locator('input[name="id"]').fill("billing")
    form.locator('select[name="refresh"]').select_option("manual")
    form.locator('button[type="submit"]').click()
    # The panel follows the refresh operation and shows the answer when it lands.
    expect(page.locator("#banks-models-note")).to_contain_text("its first answer is written")
    expect(page.locator('[data-testid="model-body"]')).to_contain_text("Answer")
    m = page.request.get(model_url(base, BANK, "billing")).json()
    assert m["body"].startswith("Answer"), m
    assert m["authority"] == "agent" and m["version"] >= 1 and not m["is_stale"] and m["based_on"]

    # ---- a person edits the answer; the next refresh files a proposal.
    page.click("#banks-model-edit")
    mine = "The billing team: Dana (lead) and Omar (new in March)."
    page.locator('#banks-model-editor textarea[name="body"]').fill(mine)
    page.locator("#banks-model-editor button[type=submit]").click()
    expect(page.locator('[data-testid="model-body"]')).to_have_text(mine)
    assert page.request.get(model_url(base, BANK, "billing")).json()["authority"] == "human"

    page.click("#banks-model-refresh")
    expect(page.locator("#banks-model-note")).to_contain_text("filed as a proposal")
    proposal = page.locator('[data-testid="proposal"]')
    expect(proposal).to_be_visible()
    expect(page.locator('[data-testid="proposal-current"]')).to_have_text(mine)
    expect(page.locator('[data-testid="proposal-content"]')).to_contain_text("Answer")
    expect(page.locator('#banks-model-tree [data-model="billing"] .banks-badge.proposal')).to_be_visible()

    # Reject: the person's text stays.
    page.click("#banks-proposal-reject")
    expect(proposal).to_have_count(0)
    got = page.request.get(model_url(base, BANK, "billing")).json()
    assert got["body"].strip() == mine and not got.get("pending_proposal")

    # Refresh again and accept: the proposal becomes the answer and the model owns it again.
    page.click("#banks-model-refresh")
    expect(page.locator("#banks-model-note")).to_contain_text("filed as a proposal")
    expect(proposal).to_be_visible()
    proposed = page.locator('[data-testid="proposal-content"]').inner_text()
    page.click("#banks-proposal-accept")
    expect(proposal).to_have_count(0)
    got = page.request.get(model_url(base, BANK, "billing")).json()
    assert got["body"].strip() == proposed.strip() and got["authority"] == "agent"

    # ---- a person corrects an observation; consolidation leaves it alone.
    note_path = f"banks/{BANK}/observations.md"
    note = page.request.get(f"{base}/api/notes/{note_path}").json()
    target = next(o for o in page.request.get(f"{base}/api/banks/{BANK}/observations").json()["items"] if "Omar" in o["text"])
    corrected = "Omar joined the billing team in April 2024, not March."
    assert target["text"] in note["body"]
    r = page.request.put(f"{base}/api/notes/{note_path}", data={"body": note["body"].replace(target["text"], corrected, 1)})
    assert r.ok, r.text()
    human = page.request.get(f"{base}/api/banks/{BANK}/observations/{target['id']}").json()["observation"]
    assert human["authority"] == "human" and human["text"] == corrected

    # A new fact about the same thing: the model proposes revising the person's
    # observation, and the engine files that as a challenge instead.
    r = page.request.post(f"{base}/api/banks/{BANK}/memories", data={
        "items": [{"content": "Omar joined the billing team and now owns the invoicing service.", "document_id": "s3",
                   "timestamp": "2024-06-01T10:00:00Z"}]})
    assert r.ok, r.text()
    wait_ops(page, base, BANK)
    after = page.request.get(f"{base}/api/banks/{BANK}/observations/{target['id']}").json()["observation"]
    assert after["text"] == corrected and after["authority"] == "human", after
    challenges = [o for o in page.request.get(f"{base}/api/banks/{BANK}/observations").json()["items"]
                  if o.get("challenges") == target["id"]]
    assert challenges, "the model's revision should be filed as a challenge"
    tab(page, "observations")
    card = page.locator(f'#banks-observations [data-observation="{target["id"]}"]')
    expect(card).to_contain_text(corrected)
    expect(card.locator(".banks-badge.human")).to_be_visible()
    expect(page.locator(f'#banks-observations [data-observation="{challenges[0]["id"]}"] .banks-badge.disputed')).to_contain_text("challenges")

    # ---- reflect with a model: an answer that cites ids the bank holds.
    tab(page, "playground")
    page.fill("#banks-query", "Who leads the billing team?")
    page.locator('#banks-playground input[name="tool_calls"]').check()
    page.click("#banks-reflect")
    result = page.locator("#banks-reflect-results")
    expect(result).to_have_attribute("data-mode", "llm")
    expect(page.locator('[data-testid="reflect-answer"]')).to_contain_text("Answer")
    cites = page.locator("#banks-citations [data-cite]")
    expect(cites.first).to_be_visible()
    known = {f["id"] for f in page.request.get(f"{base}/api/banks/{BANK}/memories?limit=500").json()["items"]}
    known |= {o["id"] for o in page.request.get(f"{base}/api/banks/{BANK}/observations").json()["items"]}
    known |= {m["id"] for m in page.request.get(f"{base}/api/banks/{BANK}/mental-models").json()["items"]}
    shown = [cites.nth(i).get_attribute("data-cite") for i in range(cites.count())]
    assert shown and set(shown) <= known, (shown, known)
    expect(page.locator("#banks-reflect-trace")).to_be_visible()
    assert "reflect" in llm_stub.kinds()

    # The operations tab lists the work the model did.
    tab(page, "operations")
    for kind in ("consolidation", "refresh_mental_model"):
        expect(page.locator(f'#banks-operations tr[data-kind="{kind}"]').first).to_have_attribute("data-status", "completed")
