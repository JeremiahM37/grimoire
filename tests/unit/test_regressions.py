"""Regression tripwires for bugs found during real-world testing.

Each test pins a specific past failure so it cannot silently return. Keep the
"regression:" note in each docstring — it documents the original incident.
"""
import sqlite3
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]


def test_dockerfile_ships_every_runtime_directory():
    """regression: the image once shipped without plugins/ — the container ran
    but every first-party plugin (katex, mermaid, kanban…) was silently absent.
    The Dockerfile must COPY every directory the server serves at runtime."""
    df = (ROOT / "deploy" / "Dockerfile").read_text()
    for required in ("COPY server", "COPY web", "COPY cli", "COPY plugins"):
        assert required in df, f"Dockerfile is missing '{required}'"


def test_dockerfile_installs_all_runtime_deps():
    """regression: argon2-cffi was once missing from the image — the app booted
    but the secret vault crashed on first use. Pin the runtime dep list."""
    df = (ROOT / "deploy" / "Dockerfile").read_text()
    for dep in ("fastapi", "uvicorn", "watchdog", "cryptography",
                "python-multipart", "argon2-cffi"):
        assert dep in df, f"Dockerfile is missing runtime dep '{dep}'"


def test_dockerfile_runs_as_non_root_with_healthcheck():
    """Production posture: non-root user + a healthcheck (added after the
    container audit). Losing either is a silent hardening regression."""
    df = (ROOT / "deploy" / "Dockerfile").read_text()
    assert "USER grimoire" in df
    assert "HEALTHCHECK" in df and "/api/health" in df


def test_db_reinit_closes_previous_connection(tmp_path):
    """regression: db.init() used to leak the previous sqlite connection when
    called twice (test fixture + app lifespan both init). Hundreds of leaked
    WAL handles caused intermittent interpreter segfaults and cross-test
    IntegrityErrors. Re-init must close the old handle."""
    from server import db
    db.init(tmp_path / "a.db")
    first = db._conn
    db.init(tmp_path / "b.db")
    try:
        with pytest.raises(sqlite3.ProgrammingError):
            first.execute("SELECT 1")          # closed handles refuse queries
    finally:
        db.close()


def test_watcher_singleton_is_disabled_in_this_suite(monkeypatch, vaultdir):
    """regression: the module-level watcher singleton, shared by every
    TestClient app, once replayed a previous test's vault into the next test's
    index (the historic notes.path IntegrityError). Unit/api tests must run
    with the watcher off; the watcher has its own integration test."""
    import os
    assert os.environ.get("GRIMOIRE_NO_WATCHER") == "1"
    from server.watcher import watcher
    assert getattr(watcher, "_observer", None) is None or \
        not getattr(watcher._observer, "is_alive", lambda: False)()


def test_note_body_comparison_ignores_trailing_newline(client):
    """regression: the serializer guarantees a trailing newline on disk, so a
    naive equality check treated every unchanged save as a content change and
    flooded version history with identical snapshots."""
    client.post("/api/notes", json={"title": "NL", "body": "same"})
    for _ in range(3):
        client.put("/api/notes/nl.md", json={"body": "same"})
    assert client.get("/api/notes/nl.md/history").json() == []


def test_history_routes_not_swallowed_by_greedy_note_route(client):
    """regression: FastAPI matches in registration order and
    /notes/{path:path} is greedy — the /history sub-routes were registered
    after it and 404'd. They must stay registered first."""
    client.post("/api/notes", json={"title": "Greedy", "body": "a"})
    client.put("/api/notes/greedy.md", json={"body": "b"})
    r = client.get("/api/notes/greedy.md/history")
    assert r.status_code == 200 and len(r.json()) == 1


def test_version_is_one_point_oh():
    """Ship as v1.0.0 — pyproject and the FastAPI app must agree."""
    py = (ROOT / "pyproject.toml").read_text()
    assert 'version = "1.0.0"' in py
    app_src = (ROOT / "server" / "app.py").read_text()
    assert 'version="1.0.0"' in app_src


def test_agent_setup_emits_discoverability_snippet(capsys):
    """regression (benchmark round 3): agents with mounted-but-unadvertised MCP
    tools made ZERO knowledge-base calls. Discoverability must be deployable:
    agent-setup prints the MCP config plus a context-file snippet."""
    from cli.grimoire import cmd_agent_setup
    cmd_agent_setup(["http://example-host:9111"])
    out = capsys.readouterr().out
    assert "mcpServers" in out and "http://example-host:9111" in out
    assert "get_briefing" in out and "CLAUDE.md" in out
