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
import json
import os
import signal
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path

OLLAMA_URL = os.environ.get("GRIMOIRE_OLLAMA_URL", "http://100.127.85.58:11434")


def wait_for(base: str, timeout: float = 90.0, proc=None, vault=None) -> None:
    """Wait for the server to answer — and, when `vault` is given, for it to be
    OUR server.

    A stray process on the port answers health checks perfectly well and then
    serves a different vault. That does not look like an error, it looks like
    results: a run of this harness once scored a leftover debug vault for two
    conversations before the answers gave it away. The vault check turns that
    into a loud failure.
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc is not None and proc.poll() is not None:
            raise RuntimeError(f"server exited with code {proc.returncode} "
                               "before answering (port already in use?)")
        try:
            with urllib.request.urlopen(base + "/api/health", timeout=5) as r:
                if vault is None:
                    return
                got = (json.loads(r.read().decode() or "{}") or {}).get("vault")
                if got and str(got) != str(vault):
                    raise RuntimeError(
                        f"{base} is serving {got!r}, not {str(vault)!r} — another "
                        "grimoire is on that port; stop it or use another port")
                return
        except urllib.error.HTTPError:
            return
        except Exception:  # noqa: BLE001 — not up yet
            time.sleep(0.25)
    raise TimeoutError(f"{base} did not come up within {timeout:g}s")


@contextlib.contextmanager
def launch(binary: str, vault: str | Path, port: int, embed: str = "auto",
           log: str | Path | None = None, env: dict | None = None):
    """Run `binary` against `vault` on `port` until the block exits.

    `env` overrides settings for one run — a benchmark asking a hundred
    questions in a row needs the AI rate limit off, which is right for a server
    answering a person and wrong here.
    """
    extra = env or {}
    env = dict(os.environ)
    env["GRIMOIRE_VAULT"] = str(vault)
    env["GRIMOIRE_PORT"] = str(port)
    env["GRIMOIRE_NO_WATCHER"] = "1"
    env.pop("GRIMOIRE_OLLAMA_URL", None)
    env["GRIMOIRE_LOCAL_EMBED"] = "off" if embed in ("off", "ollama") else "auto"
    if embed == "ollama":
        env["GRIMOIRE_OLLAMA_URL"] = OLLAMA_URL
        env["GRIMOIRE_EMBED_MODEL"] = "nomic-embed-text"

    env.update(extra)

    Path(vault).mkdir(parents=True, exist_ok=True)
    out = open(log, "w") if log else subprocess.DEVNULL  # noqa: SIM115
    proc = subprocess.Popen([str(binary)], env=env, stdout=out, stderr=out)
    base = f"http://127.0.0.1:{port}"
    try:
        wait_for(base, proc=proc, vault=Path(vault).resolve())
        yield base
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=20)
        except subprocess.TimeoutExpired:
            proc.kill()
        if out is not subprocess.DEVNULL:
            out.close()
