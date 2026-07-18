"""Shared e2e fixtures: one live server on a temp vault + browser contexts.

`page` pins the CLASSIC editor (tests drive the raw textarea); `live_page` runs
the CM6 live-preview editor (the default mode for real users).
"""
import os
import socket
import subprocess
import sys
import time
from pathlib import Path

import pytest
from playwright.sync_api import sync_playwright

ROOT = Path(__file__).resolve().parents[2]
PORT = 9121
BASE = f"http://127.0.0.1:{PORT}"
PHONE = {"width": 390, "height": 844}
DESKTOP = {"width": 1280, "height": 860}


def _free(port):
    try:
        out = subprocess.run(["ss", "-tlnp"], capture_output=True, text=True).stdout
        for line in out.splitlines():
            if f":{port} " in line and "pid=" in line:
                subprocess.run(["kill", "-9", line.split("pid=")[1].split(",")[0]],
                               capture_output=True)
    except Exception:
        pass


@pytest.fixture(scope="session")
def server(tmp_path_factory):
    vault = tmp_path_factory.mktemp("e2e-vault")
    env = {**os.environ, "GRIMOIRE_VAULT": str(vault), "GRIMOIRE_PORT": str(PORT)}
    # keep e2e hermetic/offline regardless of ambient env
    for var in ("GRIMOIRE_OLLAMA_URL", "GRIMOIRE_LLM", "GRIMOIRE_LLM_MODEL", "GRIMOIRE_WHISPER_URL"):
        env.pop(var, None)
    # the API indexes on every write; the watcher would only add redundant reindex
    # churn over the shared, ever-growing e2e vault (and can starve the server)
    env["GRIMOIRE_NO_WATCHER"] = "1"
    _free(PORT)
    # IMPORTANT: discard server output. A PIPE that nobody drains fills the ~64KB
    # OS buffer after enough uvicorn access-log lines, blocking the server on
    # write — it silently stops serving late in a large run.
    proc = subprocess.Popen([sys.executable, "-m", "server"], cwd=ROOT, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(100):
        with socket.socket() as s:
            if s.connect_ex(("127.0.0.1", PORT)) == 0:
                break
        time.sleep(0.1)
    else:
        proc.kill(); raise RuntimeError("server did not start")
    yield BASE
    proc.terminate()
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:
        proc.kill()
    _free(PORT)


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
    ctx = browser.new_context(viewport=getattr(request, "param", DESKTOP))
    ctx.add_init_script("localStorage.setItem('grimoire-editor-mode', 'classic')")
    pg = ctx.new_page()
    yield pg
    ctx.close()


@pytest.fixture()
def live_page(browser, server):
    """A page running the CM6 live-preview editor (the default mode)."""
    ctx = browser.new_context(viewport=DESKTOP)
    ctx.add_init_script("localStorage.setItem('grimoire-editor-mode', 'live')")
    pg = ctx.new_page()
    yield pg
    ctx.close()
