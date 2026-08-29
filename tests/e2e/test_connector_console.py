"""The connector configuration screen, in a real browser.

Ten sources are advertised. What makes that claim true is not that the server
can pull from them — it is that an operator can configure one without editing a
file, and the console builds its form from /api/connectors/kinds precisely so
that supporting a source and being able to configure it cannot drift apart.

So these tests are driven by the server's own list rather than a list written
here. A kind added to the server is covered the moment it ships; a kind that
renders no usable form fails here instead of at a user.

The session vault is shared, so everything is namespaced and removed after.
"""
import json
import urllib.request

import pytest
from playwright.sync_api import expect

PREFIX = "e2e-conn"


def _api(server, path, method="GET", body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(
        f"{server}/api{path}", data=data, method=method,
        headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=15) as r:
        raw = r.read()
    return json.loads(raw) if raw else None


@pytest.fixture(scope="module", autouse=True)
def _cleanup(server):
    yield
    try:
        for c in _api(server, "/connectors") or []:
            if str(c.get("name", "")).startswith(PREFIX):
                _api(server, f"/connectors/{c['id']}", method="DELETE")
    except Exception:  # noqa: BLE001
        pass


@pytest.fixture(scope="module")
def kinds(server):
    ks = _api(server, "/connectors/kinds")
    assert ks, "the server advertises no connector kinds at all"
    return ks


def _open(pg, server):
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    pg.keyboard.press("Control+k")
    pg.fill("#palette-input", "Connectors")
    pg.keyboard.press("Enter")
    expect(pg.locator("#inspect-modal")).to_be_visible(timeout=5000)
    expect(pg.locator("#conn-list")).to_be_visible(timeout=5000)


def test_the_console_opens_and_offers_every_kind_the_server_supports(page, server, kinds):
    """A source the server can pull but the console cannot offer is not supported."""
    _open(page, server)
    offered = set(page.locator("#conn-kind option").evaluate_all(
        "els => els.map(e => e.value)"))
    missing = {k["kind"] for k in kinds} - offered
    assert not missing, (
        f"the server pulls from {sorted(missing)} but the console never offers them, "
        "so nobody can configure one without editing a file")


def test_every_kind_renders_a_form_you_could_actually_fill(page, server, kinds):
    """The form is generated from the server's field list, so a kind whose fields
    do not render is a source that ships as unconfigurable."""
    _open(page, server)
    for k in kinds:
        page.select_option("#conn-kind", k["kind"])
        page.locator("#conn-new").click()
        form = page.locator("#conn-form .conn-form")
        expect(form).to_be_visible(timeout=4000)
        # Every declared field needs an input bound to its own name, or whatever
        # the operator types lands in the wrong key — or nowhere.
        for f in k.get("fields") or []:
            field = page.locator(f"#cf-{f['name']}")
            expect(field).to_have_count(1)
            expect(field).to_be_visible()
        # Destination and schedule are how a pull stays predictable.
        expect(page.locator("#cf-prefix")).to_be_visible()
        expect(page.locator("#cf-interval")).to_be_visible()
        # Help text is not decoration here: it is where the credential comes
        # from, which is the step every integration guide leaves vague.
        assert (k.get("help") or "").strip(), f"{k['kind']} ships with no help text"
        expect(form).to_contain_text(k["help"][:40])


def test_a_credential_is_named_never_typed(page, server, kinds):
    """Grimoire's model is use-don't-read: the console takes the NAME of a vault
    secret. A form that accepted the token itself would put a live credential in
    a connector row, which is the thing the broker exists to avoid."""
    _open(page, server)
    needs_secret = [k for k in kinds if (k.get("secret_help") or "").strip()]
    assert needs_secret, "no kind declares a credential; this test would be vacuous"
    for k in needs_secret:
        page.select_option("#conn-kind", k["kind"])
        page.locator("#conn-new").click()
        secret = page.locator("#cf-secret")
        expect(secret).to_be_visible(timeout=4000)
        # A name, with the vault named as where the value lives.
        expect(page.locator("#conn-form")).to_contain_text("Vault credential name")
        expect(page.locator("#conn-form")).to_contain_text("Vault")


def test_saving_a_connector_lists_it_and_removing_it_takes_it_away(page, server, kinds):
    _open(page, server)
    k = next((x for x in kinds if x["kind"] == "rss"), kinds[0])
    page.select_option("#conn-kind", k["kind"])
    page.locator("#conn-new").click()
    expect(page.locator("#conn-form .conn-form")).to_be_visible(timeout=4000)
    page.fill("#cf-name", f"{PREFIX}-saved")
    for f in k.get("fields") or []:
        page.fill(f"#cf-{f['name']}", "https://example.invalid/feed.xml")
    page.fill("#cf-prefix", f"{PREFIX}/saved")
    page.fill("#cf-interval", "0")   # manual, so nothing reaches the network
    page.locator("#cf-save").click()

    row = page.locator(".conn-row", has_text=f"{PREFIX}-saved")
    expect(row).to_be_visible(timeout=6000)
    # The row has to say what it is and where it lands; a name alone does not
    # tell an operator which of two Slack connectors this one is.
    expect(row).to_contain_text(k["kind"])
    expect(row).to_contain_text(f"{PREFIX}/saved")
    # It has never run, and that must read as "never" rather than as a success.
    expect(row).to_contain_text("never")

    page.once("dialog", lambda d: d.accept())
    row.locator(".conn-del").click()
    expect(page.locator(".conn-row", has_text=f"{PREFIX}-saved")).to_have_count(0, timeout=6000)


def test_a_failing_sync_says_so_rather_than_looking_idle(page, server, kinds):
    """A connector that stopped pulling looks exactly like one with nothing new,
    which is how a silently broken source goes unnoticed for weeks."""
    k = next((x for x in kinds if x["kind"] == "rss"), None)
    if k is None:
        pytest.skip("no rss kind to point at an unreachable host")
    cfg = {f["name"]: "http://127.0.0.1:9/nothing-here" for f in (k.get("fields") or [])}
    made = _api(server, "/connectors", method="POST", body={
        "kind": k["kind"], "name": f"{PREFIX}-broken", "config": cfg,
        "prefix": f"{PREFIX}/broken", "interval": 0})
    _open(page, server)
    row = page.locator(".conn-row", has_text=f"{PREFIX}-broken")
    expect(row).to_be_visible(timeout=6000)

    row.locator(".conn-run").click()
    # Port 9 discards everything, so the pull cannot succeed. The row must carry
    # the failure, not just a toast that has already gone.
    expect(row.locator(".conn-err")).to_be_visible(timeout=20000)
    expect(row.locator(".conn-err")).to_contain_text("failed")
    assert "undefined" not in row.inner_text()

    if made and made.get("id"):
        _api(server, f"/connectors/{made['id']}", method="DELETE")


def test_an_empty_console_explains_itself(page, server):
    """With nothing configured the screen must teach, not sit blank."""
    _open(page, server)
    body = page.locator("#inspect-body")
    expect(body).to_contain_text("into your vault as ordinary notes", timeout=5000)
    expect(body).not_to_contain_text("undefined")
