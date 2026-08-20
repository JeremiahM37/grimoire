"""Shared fixtures.

The adapter tests drive a REAL Grimoire binary rather than a stub. An adapter
that satisfies an interface I imagined is worth nothing, and both LangGraph's
store protocol and CrewAI's vector-in backend have enough surface that only the
real thing settles it.
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request

import pytest

BINARY = os.environ.get("GRIMOIRE_BINARY") or os.path.join(
    os.path.dirname(__file__), "..", "..", "..", "go", "grimoire"
)


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


@pytest.fixture(scope="session")
def live_server():
    """A Grimoire server on a throwaway vault, for the length of the session."""
    binary = os.path.abspath(BINARY)
    if not os.path.exists(binary):
        pytest.skip("build the server first: cd go && go build -o grimoire ./cmd/grimoire")
    # A binary older than the source it was built from produces failures that
    # look like adapter bugs and are not — an afternoon went into one. Fail on
    # it explicitly instead.
    source = os.path.join(os.path.dirname(binary), "internal")
    newest = max(
        (os.path.getmtime(os.path.join(root, name))
         for root, _, files in os.walk(source) for name in files
         if name.endswith(".go")),
        default=0,
    )
    if newest > os.path.getmtime(binary):
        pytest.fail("the server binary is older than go/internal — rebuild it: "
                    "cd go && go build -o grimoire ./cmd/grimoire")

    vault = tempfile.mkdtemp(prefix="grimoire-clients-")
    port = free_port()
    proc = subprocess.Popen(
        [binary],
        env={**os.environ, "GRIMOIRE_VAULT": vault, "GRIMOIRE_PORT": str(port)},
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    url = f"http://127.0.0.1:{port}"
    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            urllib.request.urlopen(f"{url}/api/health", timeout=1).read()
            break
        except (urllib.error.URLError, ConnectionError, OSError):
            if proc.poll() is not None:
                pytest.fail("server exited during startup")
            time.sleep(0.2)
    else:
        proc.kill()
        pytest.fail("server did not become ready")
    try:
        yield url
    finally:
        proc.terminate()
        proc.wait(timeout=10)
        shutil.rmtree(vault, ignore_errors=True)
