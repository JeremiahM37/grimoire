"""The console identifies the executable serving it, including local builds."""
from playwright.sync_api import expect


def test_running_build_is_visible(page, server):
    page.goto(server)
    health = page.request.get(server + "/api/health").json()
    build = health["build"]
    expect(page.locator("#running-build")).to_contain_text(health["version"])
    if build["modified"] is True:
        expect(page.locator("#running-build")).to_contain_text("local changes")
    # the footer shows only the version; "revision unknown" / "build status unknown" are not shown
    expect(page.locator("#running-build")).not_to_contain_text("unknown")
