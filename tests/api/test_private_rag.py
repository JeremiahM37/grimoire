"""Negative / security: private notes must NEVER leak into retrieval or ask."""


def _seed_private(client):
    # a public note and a private one that both mention a distinctive token
    client.post("/api/notes", json={"title": "Public", "body":
        "The zephyrine protocol is documented publicly here."})
    client.post("/api/notes", json={"title": "Secret Diary",
        "frontmatter": {"private": True},
        "body": "The zephyrine protocol secret access code is 4815162342."})


def test_private_excluded_from_retrieve_by_default(client):
    _seed_private(client)
    res = client.get("/api/retrieve", params={"q": "zephyrine protocol"}).json()
    paths = [r["path"] for r in res]
    assert "secret-diary.md" not in paths
    assert "public.md" in paths


def test_private_never_in_ask_answer_or_citations(client):
    _seed_private(client)
    r = client.post("/api/ask", json={"q": "what is the zephyrine access code"}).json()
    assert "4815162342" not in r["answer"]                      # secret not surfaced
    assert all(c["path"] != "secret-diary.md" for c in r["citations"])


def test_private_included_only_when_explicitly_opted_in(client):
    _seed_private(client)
    res = client.get("/api/retrieve",
                     params={"q": "zephyrine protocol", "include_private": "true"}).json()
    assert any(r["path"] == "secret-diary.md" for r in res)     # opt-in works


def test_private_flag_persists_and_reindexes(client):
    _seed_private(client)
    client.post("/api/reindex")
    from server import db
    priv = db.one("SELECT COUNT(*) c FROM vectors WHERE private=1")["c"]
    assert priv >= 1
    # still excluded after reindex
    res = client.get("/api/retrieve", params={"q": "zephyrine protocol"}).json()
    assert "secret-diary.md" not in [r["path"] for r in res]
