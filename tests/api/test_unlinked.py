"""Unlinked mentions: find plain-text mentions that aren't wiki-links yet."""


def test_unlinked_mentions_found_and_linkable(client):
    client.post("/api/notes", json={"title": "Project Phoenix", "body": "the big effort"})
    client.post("/api/notes", json={"title": "Meeting", "body": "we discussed Project Phoenix at length"})
    client.post("/api/notes", json={"title": "Linked", "body": "see [[Project Phoenix]] already"})
    u = client.get("/api/notes/project-phoenix.md/unlinked").json()
    paths = {x["path"] for x in u}
    assert "meeting.md" in paths          # plain mention → unlinked
    assert "linked.md" not in paths       # already links → excluded
    hit = next(x for x in u if x["path"] == "meeting.md")
    assert "Project Phoenix" in hit["context"]
    # one-click link wraps the mention in the source note
    r = client.post("/api/notes/project-phoenix.md/link",
                    json={"source": "meeting.md", "name": "Project Phoenix"})
    assert r.status_code == 200
    assert "[[Project Phoenix]]" in client.get("/api/notes/meeting.md").json()["body"]
    # now it's a real backlink, no longer unlinked
    assert client.get("/api/notes/project-phoenix.md/unlinked").json() == []
    assert any(b["path"] == "meeting.md"
               for b in client.get("/api/notes/project-phoenix.md").json()["backlinks"])


def test_unlinked_ignores_short_titles_and_encrypted(client):
    client.post("/api/notes", json={"title": "AI", "body": "x"})     # too short (<3)
    client.post("/api/notes", json={"title": "Talk", "body": "all about AI today"})
    assert client.get("/api/notes/ai.md/unlinked").json() == []
