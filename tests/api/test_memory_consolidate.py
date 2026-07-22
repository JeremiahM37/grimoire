"""Memory consolidation: compact an agent's memory note while keeping it
markdown, provenance-preserving, and rollback-able."""


def test_dedup_fallback_without_llm(client):
    """No LLM → consolidation removes exact-duplicate entries (safe floor)."""
    for _ in range(3):
        client.post("/api/memory", json={"text": "staging needs --force-recreate after a vpn change",
                                          "topic": "deploy", "agent": "claude-code"})
    body = client.get("/api/notes/memory%2Fdeploy.md").json()["body"]
    assert body.count("- **") == 3                      # three appended (dup) entries
    r = client.post("/api/memory/consolidate", json={"topic": "deploy"}).json()
    assert r["notes_changed"] == 1
    body2 = client.get("/api/notes/memory%2Fdeploy.md").json()["body"]
    assert body2.count("- **") == 1                     # collapsed to one


def test_consolidate_snapshots_for_rollback(client):
    client.post("/api/memory", json={"text": "fact A", "topic": "ops", "agent": "a"})
    client.post("/api/memory", json={"text": "fact A", "topic": "ops", "agent": "a"})  # dup
    client.post("/api/memory/consolidate", json={"topic": "ops"})
    versions = client.get("/api/notes/memory%2Fops.md/history").json()
    assert versions                                     # pre-consolidation snapshot exists


def test_consolidate_all_when_unspecified(client, monkeypatch):
    from server import ai
    client.post("/api/memory", json={"text": "x", "topic": "one", "agent": "a"})
    client.post("/api/memory", json={"text": "y", "topic": "two", "agent": "a"})
    # force a real rewrite so both count as changed
    monkeypatch.setattr(ai, "consolidate_memory", lambda b: b + "\n- **z** — consolidated\n")
    r = client.post("/api/memory/consolidate", json={}).json()
    assert r["notes_changed"] == 2


def test_consolidate_uses_llm_when_present(monkeypatch):
    from server import ai
    monkeypatch.setenv("GRIMOIRE_LLM", "openai")
    monkeypatch.setenv("GRIMOIRE_LLM_BASE_URL", "http://x/v1")
    monkeypatch.setattr(ai, "_complete",
                        lambda p, b="": "# Memory: deploy\n\n- **merged** — one clean entry\n")
    out = ai.consolidate_memory("# Memory: deploy\n\n- **a** — dup\n- **a** — dup\n- **b** — other\n")
    assert "one clean entry" in out
