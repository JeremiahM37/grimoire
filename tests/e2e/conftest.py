"""Shared e2e fixtures: one live server on a temp vault + browser contexts.

`page` pins the CLASSIC editor (tests drive the raw textarea); `live_page` runs
the CM6 live-preview editor (the default mode for real users).
"""
import os
import socket
import subprocess
import time
import urllib.request
from pathlib import Path

import pytest
from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parents[2]
PORT = int(os.environ.get("GRIMOIRE_E2E_PORT", "9121"))
BASE = f"http://127.0.0.1:{PORT}"
PHONE = {"width": 390, "height": 844}
DESKTOP = {"width": 1280, "height": 860}

# Set by the server fixture; the vault the live server is running on.
VAULT = None


def _free(port):
    # A test must never kill an unrelated listener to claim its port.
    with socket.socket() as listener:
        listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        try:
            listener.bind(("127.0.0.1", port))
        except OSError as error:
            raise RuntimeError(f"test port {port} is occupied; choose GRIMOIRE_E2E_PORT") from error


def _start_server(vault, port, extra_env=None):
    """Start the binary on a vault and port; returns the process once healthy."""
    env = {**os.environ, "GRIMOIRE_VAULT": str(vault), "GRIMOIRE_PORT": str(port)}
    # keep e2e hermetic/offline regardless of ambient env
    for var in ("GRIMOIRE_OLLAMA_URL", "GRIMOIRE_LLM", "GRIMOIRE_LLM_MODEL", "GRIMOIRE_WHISPER_URL",
                "GRIMOIRE_LLM_BASE_URL", "GRIMOIRE_LLM_API_KEY", "GRIMOIRE_LLM_REASONING_EFFORT", "GRIMOIRE_LLM_EXTRA_BODY"):
        env.pop(var, None)
    # the API indexes on every write; the watcher would only add redundant reindex
    # churn over the shared, ever-growing e2e vault (and can starve the server)
    env["GRIMOIRE_NO_WATCHER"] = "1"
    env.update(extra_env or {})
    _free(port)
    # IMPORTANT: discard server output. A PIPE that nobody drains fills the ~64KB
    # OS buffer after enough uvicorn access-log lines, blocking the server on
    # write — it silently stops serving late in a large run.
    # These tests drive a browser and the HTTP API — neither knows what is
    # answering — so GRIMOIRE_E2E_SERVER can point them at any build. That is
    # what let this suite act as the acceptance gate for the Go port.
    binary = os.environ.get("GRIMOIRE_E2E_SERVER") or str(ROOT / "go" / "grimoire")
    if not Path(binary).exists():
        pytest.skip(f"no grimoire binary at {binary} — build it with "
                    "`cd go && go build -o grimoire ./cmd/grimoire`",
                    allow_module_level=True)
    env.setdefault("GRIMOIRE_WEB_DIR", str(ROOT / "web"))
    proc = subprocess.Popen([binary], cwd=ROOT, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = f"http://127.0.0.1:{port}"
    for _ in range(100):
        if proc.poll() is not None:
            raise RuntimeError(f"server exited before becoming healthy: {proc.returncode}")
        try:
            with urllib.request.urlopen(base + "/api/health", timeout=1) as response:
                if response.status == 200:
                    break
        except OSError:
            pass
        time.sleep(0.1)
    else:
        proc.kill(); raise RuntimeError("server did not start")
    return proc


def _stop_server(proc):
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
        proc.wait(timeout=5)


@pytest.fixture(scope="session")
def server(tmp_path_factory):
    vault = tmp_path_factory.mktemp("e2e-vault")
    proc = _start_server(vault, PORT)
    # The vault path is published so a test can seed tables the UI reads
    # without the server growing a test-only endpoint. WAL plus the busy
    # timeout make a second writer safe.
    global VAULT
    VAULT = vault
    yield BASE
    _stop_server(proc)


@pytest.fixture(scope="session")
def llm_stub():
    """An OpenAI-compatible stand-in that answers a bank's model calls (llm_stub.py)."""
    from llm_stub import StubLLM
    stub = StubLLM().start()
    yield stub
    stub.stop()


@pytest.fixture(scope="session")
def llm_server(tmp_path_factory, llm_stub):
    """A second server, on its own vault and port, whose language model is the stub.

    Yields (base_url, vault_path). Kept apart from `server` so every other
    test still runs with no model configured."""
    vault = tmp_path_factory.mktemp("e2e-llm-vault")
    port = int(os.environ.get("GRIMOIRE_E2E_LLM_PORT", str(PORT + 1)))
    proc = _start_server(vault, port, {
        "GRIMOIRE_LLM": "openai", "GRIMOIRE_LLM_BASE_URL": llm_stub.base_url,
        "GRIMOIRE_LLM_MODEL": "stub-model", "GRIMOIRE_LLM_API_KEY": "stub-key"})
    yield f"http://127.0.0.1:{port}", vault
    _stop_server(proc)


@pytest.fixture(scope="session")
def browser():
    with sync_playwright() as p:
        b = p.chromium.launch()
        yield b
        b.close()


@pytest.fixture()
def page(browser, server, request):
    """A page pinned to the CLASSIC editor — this suite drives the textarea
    directly. Live-editor behavior has its own fixture below (live_page)."""
    # Routed fixture responses must not be bypassed by a controlling worker.
    # Offline/worker tests explicitly create a worker-enabled context.
    ctx = browser.new_context(viewport=getattr(request, "param", DESKTOP), service_workers="block")
    ctx.add_init_script("localStorage.setItem('grimoire-editor-mode', 'classic')")
    pg = ctx.new_page()
    yield pg
    ctx.close()


@pytest.fixture()
def live_page(browser, server):
    """A page running the CM6 live-preview editor (the default mode)."""
    ctx = browser.new_context(viewport=DESKTOP, service_workers="block")
    ctx.add_init_script("localStorage.setItem('grimoire-editor-mode', 'live')")
    pg = ctx.new_page()
    yield pg
    ctx.close()

# The app sets body[data-ready] at the END of boot. A reload restarts boot, and
# boot restores the previously-open note -- so a click issued between the two
# either misses the row that has not rendered yet, or lands and is then
# overwritten when the restore finishes. That is what
# test_alias_wikilink_navigates hit on CI: #title held "Trash E2E", a note from
# an entirely different module, and the same commit passed on rerun.
#
# Waiting on readiness is the fix. A longer expect() timeout is not: the value
# was already wrong and stayed wrong, so more patience would only have made the
# failure slower.
def reload_ready(page, timeout=10000):
    """Reload and wait for boot to finish before returning."""
    page.reload()
    page.wait_for_selector("body[data-ready]", timeout=timeout)


def answer_panel(page, value=None):
    panel = page.locator(".form-panel").last
    panel.wait_for(state="visible")
    if value is not None:
        panel.locator("input,textarea,select").first.fill(value)
    element = panel.element_handle()
    panel.locator('button[type="submit"]').click()
    element.wait_for_element_state("hidden")
