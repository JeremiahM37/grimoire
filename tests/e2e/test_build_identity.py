"""The console identifies the executable serving it, including local builds."""
from playwright.sync_api import expect


def test_running_build_is_visible(page, server):
    page.goto(server)
    health = page.request.get(server + "/api/health").json()
    build = health["build"]
    expect(page.locator("#running-build")).to_contain_text(health["version"])
    expect(page.locator("#running-build")).to_contain_text(build["revision"][:12] or "revision unknown")
    label = "local changes" if build["modified"] is True else "clean" if build["modified"] is False else "build status unknown"
    expect(page.locator("#running-build")).to_contain_text(label)
