"""Independent real-server fixtures for knowledge acceptance.

This deliberately does not import tests/e2e/conftest.py: that fixture owns a
fixed listener and cleanup policy which is unsafe when another worker has a live
server. Every test session below gets a free loopback port and a private vault.
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
FIXTURES = Path(__file__).parent / "fixtures"


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def _wait_for_server(base_url: str, proc: subprocess.Popen[str]) -> None:
    import urllib.error
    import urllib.request

    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"server exited during startup with status {proc.returncode}")
        try:
            with urllib.request.urlopen(f"{base_url}/api/health", timeout=1) as response:
                if response.status == 200:
                    return
        except (OSError, urllib.error.URLError):
            time.sleep(0.1)
    proc.terminate()
    raise RuntimeError("isolated Grimoire server did not become healthy within 30 seconds")


@pytest.fixture(scope="session")
def knowledge_server(tmp_path_factory: pytest.TempPathFactory):
    vault = tmp_path_factory.mktemp("knowledge-vault")
    for fixture in FIXTURES.glob("*.md"):
        shutil.copy2(fixture, vault / fixture.name)

    port = _free_port()
    binary = vault / "grimoire"
    build = subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/grimoire"],
        cwd=ROOT / "go",
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    if build.returncode:
        pytest.fail(f"could not build real server:\n{build.stdout}\n{build.stderr}")

    env = dict(os.environ)
    env.update(
        {
            "GRIMOIRE_VAULT": str(vault),
            "GRIMOIRE_HOST": "127.0.0.1",
            "GRIMOIRE_PORT": str(port),
            "GRIMOIRE_NO_WATCHER": "1",
            "GRIMOIRE_WEB_DIR": str(ROOT / "web"),
            # Acceptance must be offline and deterministic; the server's hasher
            # embedder is a real fallback, not a mocked knowledge endpoint.
            "GRIMOIRE_LOCAL_EMBED": "off",
        }
    )
    for name in ("GRIMOIRE_OLLAMA_URL", "GRIMOIRE_LLM", "GRIMOIRE_LLM_MODEL", "GRIMOIRE_WHISPER_URL"):
        env.pop(name, None)

    log = vault / "server.log"
    with log.open("w") as handle:
        proc = subprocess.Popen(
            [str(binary)], cwd=ROOT, env=env, stdout=handle, stderr=subprocess.STDOUT,
            text=True,
        )
    base_url = f"http://127.0.0.1:{port}"
    try:
        _wait_for_server(base_url, proc)
        yield base_url, vault, log
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait(timeout=5)


@pytest.fixture(scope="session")
def browser():
    playwright = pytest.importorskip("playwright.sync_api")
    with playwright.sync_playwright() as api:
        browser = api.chromium.launch()
        yield browser
        browser.close()


@pytest.fixture()
def page(browser, knowledge_server, request):
    viewport = getattr(request, "param", {"width": 1440, "height": 960})
    context = browser.new_context(viewport=viewport)
    page = context.new_page()
    yield page
    context.close()
