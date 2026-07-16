"""v0.2 API: ask-your-notes, retrieve, inline actions."""


def _seed(client):
    client.post("/api/notes", json={"title": "Sourdough", "body":
        "Sourdough bread needs a starter of flour and water fermented for days."})
    client.post("/api/notes", json={"title": "Espresso", "body":
        "Espresso is brewed by forcing hot water through finely ground coffee."})


def test_retrieve_ranks_relevant_note(client):
    _seed(client)
    res = client.get("/api/retrieve", params={"q": "coffee brewing"}).json()
    assert res and res[0]["path"] == "espresso.md"
    assert 0 < res[0]["score"] <= 1


def test_ask_returns_answer_and_citations(client):
    _seed(client)
    r = client.post("/api/ask", json={"q": "how is sourdough made"}).json()
    assert "starter" in r["answer"].lower()
    assert any(c["path"] == "sourdough.md" for c in r["citations"])


def test_ask_empty_query(client):
    assert client.post("/api/ask", json={"q": "   "}).json()["answer"] == ""


def test_inline_actions(client):
    body = "Meeting about the quarterly budget planning and hiring roadmap."
    tags = client.post("/api/actions", json={"action": "tags", "text": body}).json()["result"]
    assert isinstance(tags, list) and tags
    title = client.post("/api/actions", json={"action": "title",
        "text": "# Budget Notes\nmore"}).json()["result"]
    assert title == "Budget Notes"
    summ = client.post("/api/actions", json={"action": "summarize", "text": body}).json()
    assert summ["result"]   # extractive fallback returns something
    bad = client.post("/api/actions", json={"action": "nonsense", "text": "x"}).json()
    assert bad.get("error")


def test_reindex_populates_vectors(client):
    _seed(client)
    from server import db
    assert db.one("SELECT COUNT(*) c FROM vectors")["c"] >= 2
