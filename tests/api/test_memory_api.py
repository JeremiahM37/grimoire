"""Agent memory: provenance, topic accretion, recall, rollback via history,
and the injection/path posture."""


def test_remember_creates_topic_note_with_provenance(client):
    r = client.post("/api/memory", json={
        "text": "The deploy script needs sudo on the media host.",
        "topic": "deploy quirks", "agent": "claude-code", "task": "task-42"})
    assert r.status_code == 201
    out = r.json()
    assert out["path"] == "memory/deploy-quirks.md" and out["created"] is True
    note = client.get("/api/notes/memory/deploy-quirks.md").json()
    assert note["frontmatter"]["memory"] is True
    assert note["frontmatter"]["agent"] == "claude-code"
    assert note["frontmatter"]["task"] == "task-42"
    assert "claude-code" in note["body"] and "needs sudo" in note["body"]


def test_memories_accrete_on_the_same_topic(client):
    client.post("/api/memory", json={"text": "first fact", "topic": "project x"})
    r = client.post("/api/memory", json={"text": "second fact", "topic": "project x",
                                         "agent": "researcher"})
    assert r.json()["created"] is False
    body = client.get("/api/notes/memory/project-x.md").json()["body"]
    assert "first fact" in body and "second fact" in body
    assert body.index("first fact") < body.index("second fact")   # chronological


def test_agent_appends_are_rollbackable(client):
    client.post("/api/memory", json={"text": "good memory", "topic": "roll"})
    client.post("/api/memory", json={"text": "hallucinated nonsense", "topic": "roll"})
    versions = client.get("/api/notes/memory/roll.md/history").json()
    assert versions            # the pre-append body was snapshotted
    client.post(f"/api/notes/memory/roll.md/history/{versions[0]['id']}/restore")
    body = client.get("/api/notes/memory/roll.md").json()["body"]
    assert "good memory" in body and "hallucinated nonsense" not in body


def test_append_updates_both_agent_and_task_provenance(client):
    """regression: appends updated fm.agent but kept the ORIGINAL fm.task, so
    the provenance banner mixed one agent's name with another agent's task."""
    client.post("/api/memory", json={"text": "a", "topic": "prov",
                                     "agent": "first-agent", "task": "t-1"})
    client.post("/api/memory", json={"text": "b", "topic": "prov",
                                     "agent": "second-agent", "task": "t-2"})
    fm = client.get("/api/notes/memory/prov.md").json()["frontmatter"]
    assert fm["agent"] == "second-agent" and fm["task"] == "t-2"


def test_recall_by_query_and_recency(client):
    client.post("/api/memory", json={"text": "the database password rotates monthly",
                                     "topic": "ops"})
    client.post("/api/memory", json={"text": "team prefers tabs", "topic": "style"})
    hits = client.get("/api/memory", params={"q": "database"}).json()
    assert [h["path"] for h in hits] == ["memory/ops.md"]
    recent = client.get("/api/memory").json()
    assert {h["path"] for h in recent} == {"memory/ops.md", "memory/style.md"}


def test_recall_only_sees_the_memory_namespace(client):
    client.post("/api/notes", json={"title": "Normal Note", "body": "database things"})
    client.post("/api/memory", json={"text": "unrelated", "topic": "other"})
    hits = client.get("/api/memory", params={"q": "database"}).json()
    assert all(h["path"].startswith("memory/") for h in hits)
    assert hits == []          # the normal note must not leak into recall


def test_recall_matches_non_adjacent_terms(client):
    """regression (found live in production): recall phrase-quoted the whole
    query, so 'deployed context server' missed a memory that said 'deployed as
    the context server'. Recall must AND individual terms, not exact-phrase."""
    client.post("/api/memory", json={
        "text": "grimoire deployed as the context server on AIServer",
        "topic": "prod-recall"})
    hits = client.get("/api/memory", params={"q": "deployed context server"}).json()
    assert [h["path"] for h in hits] == ["memory/prod-recall.md"]


def test_recall_query_is_injection_safe(client):
    client.post("/api/memory", json={"text": "safe entry", "topic": "inj"})
    r = client.get("/api/memory", params={"q": 'x" OR path:"'})
    assert r.status_code == 200 and r.json() == []


def test_bad_agent_name_and_topic_traversal_rejected(client):
    assert client.post("/api/memory", json={
        "text": "x", "agent": "a\nb"}).status_code == 400
    # a hostile topic slugs down to a safe filename inside memory/
    r = client.post("/api/memory", json={"text": "x", "topic": "../../etc/passwd"})
    assert r.status_code == 201
    assert r.json()["path"].startswith("memory/")
    assert ".." not in r.json()["path"]


def test_topicless_memory_lands_on_a_daily_note(client):
    r = client.post("/api/memory", json={"text": "loose thought"})
    assert r.status_code == 201
    import re
    assert re.fullmatch(r"memory/\d{4}-\d{2}-\d{2}\.md", r.json()["path"])
