"""Launch a grimoire server for a benchmark run.

The harnesses used to build the vault and retrieve in-process, which tied them
to one implementation. They now drive a real server over HTTP, so this is the
piece that makes a run self-contained: give it the binary and an embedder
condition and it starts, waits, and cleans up.

The embedder conditions are the ones the reports compare:

    off     the zero-dependency hashing embedder (the as-shipped floor)
    auto    the local model2vec model (the pip-extra / built-in condition)
    ollama  semantic embeddings from a local Ollama

`GRIMOIRE_NO_WATCHER` is always set: the harness rewrites the whole vault
between questions, and a watcher would re-index underneath the run.
"""
from __future__ import annotations

import contextlib
import os
import signal
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path

OLLAMA_URL = os.environ.get("GRIMOIRE_OLLAMA_URL", "http://100.127.85.58:11434")


def wait_for(base: str, timeout: float = 90.0, proc=None) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc is not None and proc.poll() is not None:
            raise RuntimeError(f"server exited with code {proc.returncode} "
                               "before answering (port already in use?)")
        try:
            with urllib.request.urlopen(base + "/api/health", timeout=5):
                return
        except urllib.error.HTTPError:
            return
        except Exception:  # noqa: BLE001 — not up yet
            time.sleep(0.25)
    raise TimeoutError(f"{base} did not come up within {timeout:g}s")


@contextlib.contextmanager
def launch(binary: str, vault: str | Path, port: int, embed: str = "auto",
           log: str | Path | None = None):
    """Run `binary` against `vault` on `port` until the block exits."""
    env = dict(os.environ)
    env["GRIMOIRE_VAULT"] = str(vault)
    env["GRIMOIRE_PORT"] = str(port)
    env["GRIMOIRE_NO_WATCHER"] = "1"
    env.pop("GRIMOIRE_OLLAMA_URL", None)
    env["GRIMOIRE_LOCAL_EMBED"] = "off" if embed in ("off", "ollama") else "auto"
    if embed == "ollama":
        env["GRIMOIRE_OLLAMA_URL"] = OLLAMA_URL
        env["GRIMOIRE_EMBED_MODEL"] = "nomic-embed-text"

    Path(vault).mkdir(parents=True, exist_ok=True)
    out = open(log, "w") if log else subprocess.DEVNULL  # noqa: SIM115
    proc = subprocess.Popen([str(binary)], env=env, stdout=out, stderr=out)
    base = f"http://127.0.0.1:{port}"
    try:
        wait_for(base, proc=proc)
        yield base
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=20)
        except subprocess.TimeoutExpired:
            proc.kill()
        if out is not subprocess.DEVNULL:
            out.close()
