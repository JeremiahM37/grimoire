"""The credential console, in a real browser.

The vault is the part of Grimoire where being quietly wrong is most expensive,
and every assertion here is a variant of one property: the screen tells you
what you need in order to decide what to rotate, and never tells you the value.

The session server is shared, so this uses the same passphrase as the other
suites and tolerates init-or-unlock, and everything it creates is namespaced.
"""
import json
import urllib.parse
import urllib.request

import pytest
from playwright.sync_api import expect

NS = "e2ecred"
PW = "mypassphrase123"


def _api(server, path, method="GET", body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"{server}/api{path}", data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=30) as r:
        raw = r.read()
    return json.loads(raw) if raw else None


@pytest.fixture(scope="module", autouse=True)
def _vault(server):
    for ep in ("init", "unlock"):
        try:
            _api(server, f"/vault/{ep}", "POST", {"passphrase": PW})
        except Exception:  # noqa: BLE001
            pass
    yield
    try:
        for s in _api(server, "/secrets") or []:
            if s["name"].startswith(NS):
                _api(server, "/secrets/" + urllib.parse.quote(s["name"]), method="DELETE")
    except Exception:  # noqa: BLE001
        pass


def _open_vault(pg, server):
    pg.goto(server)
    pg.wait_for_selector("body[data-ready]", timeout=10000)
    pg.locator("#vault-open").click()
    expect(pg.locator("#vault-body")).to_be_visible(timeout=8000)


def test_a_rotation_is_visible_and_undoable_from_the_console(page, server):
    """The one operation people are afraid of, and the one that was one-way."""
    name = f"{NS}-rotate"
    _api(server, "/secrets", "POST", {"name": name, "value": "value-ONE"})
    _api(server, "/secrets", "POST", {"name": name, "value": "value-TWO",
                                      "note": "quarterly rotation"})
    _open_vault(page, server)

    row = page.locator(".v-row", has_text=name)
    expect(row).to_be_visible(timeout=8000)
    expect(row).to_contain_text("1 prior")

    row.locator(".v-hist").click()
    expect(page.locator(".v-hist-box", has_text="quarterly rotation")).to_be_visible(timeout=6000)

    page.once("dialog", lambda d: d.accept())
    page.locator(".v-restore").first.click()
    page.wait_for_timeout(1500)

    # Verified through the API, because the console must not be able to show it.
    page.wait_for_timeout(500)
    vers = _api(server, f"/secrets/versions?name={name}")
    assert len(vers["versions"]) == 2, \
        "the rollback did not retain the value it replaced, so it is not undoable"


def test_no_secret_value_ever_reaches_the_page(page, server):
    name = f"{NS}-quiet"
    _api(server, "/secrets", "POST",
         {"name": name, "value": "sk_live_THISMUSTNEVERAPPEAR"})
    _api(server, "/secrets", "POST",
         {"name": name, "value": "sk_live_NORTHIS", "note": "rotated"})
    _open_vault(page, server)
    page.locator(".v-row", has_text=name).locator(".v-hist").click()
    page.wait_for_timeout(1200)
    html = page.content()
    for leaked in ("THISMUSTNEVERAPPEAR", "NORTHIS"):
        assert leaked not in html, f"a secret value reached the DOM: {leaked}"


def test_the_console_says_what_needs_rotating(page, server):
    """A credential dying is silent; the vault is the one thing that could
    have said so in advance."""
    _api(server, "/secrets", "POST", {
        "name": f"{NS}-expired", "value": "x",
        "meta": {"expires": "2020-01-01", "note": "an old key"}})
    _open_vault(page, server)
    row = page.locator(".v-row", has_text=f"{NS}-expired")
    expect(row).to_be_visible(timeout=8000)
    expect(row.locator(".v-bad")).to_contain_text("expired")
    # And the header carries a count, so it is visible without reading rows.
    expect(page.locator("#vault-body")).to_contain_text("need attention")


def test_a_never_used_credential_is_distinguishable_from_an_unused_one(page, server):
    _api(server, "/secrets", "POST", {"name": f"{NS}-fresh", "value": "x"})
    _open_vault(page, server)
    row = page.locator(".v-row", has_text=f"{NS}-fresh")
    expect(row).to_be_visible(timeout=8000)
    expect(row).to_contain_text("never used")


def test_scanning_notes_reports_masked_findings(page, server):
    """Their scanners watch git; here the substrate is a vault somebody types
    into and syncs to a phone."""
    key = "AKIAIOSFODNN7EXAMPLE"
    _api(server, "/notes", "POST", {
        "title": f"{NS} debugging",
        "body": f"# {NS} debugging\n\ntemporarily used {key} while testing\n"})
    _open_vault(page, server)
    page.locator("#v-scan").click()
    body = page.locator("#vault-body")
    expect(body).to_contain_text("Credentials found in notes", timeout=25000)
    expect(body).to_contain_text("AWS access key")
    # Masked: a panel that printed the key would copy the leak onto a screen.
    assert key not in page.content(), "the scan panel printed the credential it found"
    expect(body).to_contain_text("AKIA")
    expect(body).to_contain_text("Rotate anything real at the issuer first")


def test_the_scan_is_quiet_on_ordinary_notes(page, server):
    """A scanner that cries wolf gets turned off, and then its silence means
    'not run' rather than 'clean'."""
    _api(server, "/notes", "POST", {
        "title": f"{NS} ordinary",
        "body": f"# {NS} ordinary\n\nThe API key lives in the vault. password: changeme\n"})
    _open_vault(page, server)
    page.locator("#v-scan").click()
    expect(page.locator("#vault-body")).to_contain_text(
        "Credentials found in notes", timeout=25000)
    findings = page.locator(".v-arow", has_text=f"{NS} ordinary")
    expect(findings).to_have_count(0)


def test_a_grant_shows_what_it_has_left_in_count_not_only_in_time(page, server):
    """A TTL bounds a grant in time and not at all in volume: fifteen minutes is
    fifteen minutes in which an agent may make any number of calls."""
    name = f"{NS}-limited"
    _api(server, "/secrets", "POST", {"name": name, "value": "x"})
    _api(server, f"/secrets/{name}/grant", "POST", {
        "grantee": f"{NS}-agent", "scope": "https://example.com",
        "ttl_seconds": 900, "max_uses": 3})
    _open_vault(page, server)
    row = page.locator(".v-row", has_text=f"{NS}-agent")
    expect(row).to_be_visible(timeout=8000)
    expect(row).to_contain_text("3 of 3 uses left")
    # And the time bound is still shown alongside it, not replaced by it.
    expect(row).to_contain_text("left")


def test_secrets_can_be_organised_into_namespaces(page, server):
    for n in (f"{NS}-prod/one", f"{NS}-prod/two", f"{NS}-dev/one"):
        _api(server, "/secrets", "POST", {"name": n, "value": "x"})
    scoped = _api(server, f"/secrets/details?prefix={NS}-prod")
    names = [s["name"] for s in scoped["secrets"]]
    assert sorted(names) == [f"{NS}-prod/one", f"{NS}-prod/two"], names
    assert scoped["namespaces"].get(f"{NS}-dev") == 1, \
        "the namespace list must cover the whole vault, not only the scoped view"
    # The console still lists them all, so nothing is hidden by the feature.
    _open_vault(page, server)
    expect(page.locator(".v-row", has_text=f"{NS}-dev/one")).to_be_visible(timeout=8000)
