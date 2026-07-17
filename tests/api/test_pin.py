"""Pinned notes float to the top of the list."""


def test_pin_floats_note_to_top(client, vaultdir):
    client.post("/api/notes", json={"title": "Alpha Pin", "body": "a"})
    client.post("/api/notes", json={"title": "Beta Pin", "body": "b"})
    client.post("/api/notes", json={"title": "Gamma Pin", "body": "c"})
    assert client.post("/api/notes/alpha-pin.md/pin").json()["pinned"] is True
    lst = client.get("/api/notes").json()
    assert lst[0]["title"] == "Alpha Pin" and lst[0]["pinned"] is True
    assert all(not n["pinned"] for n in lst[1:])
    # persisted in frontmatter on disk
    assert "pinned: true" in (vaultdir / "alpha-pin.md").read_text()
    # unpin clears it
    assert client.post("/api/notes/alpha-pin.md/pin").json()["pinned"] is False
    assert all(not n["pinned"] for n in client.get("/api/notes").json())
    assert "pinned" not in (vaultdir / "alpha-pin.md").read_text()
