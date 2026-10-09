"""Fixtures shared by every browser suite.

One Playwright per test process: two suites that each start their own
(`sync_playwright()` in a session fixture) cannot run in one pytest
invocation, because the second start finds the first one's event loop
running and refuses."""

import pytest


@pytest.fixture(scope="session")
def browser():
    playwright = pytest.importorskip("playwright.sync_api")
    with playwright.sync_playwright() as api:
        launched = api.chromium.launch()
        yield launched
        launched.close()
