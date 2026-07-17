"""Cross-note task aggregation."""


def test_tasks_aggregates_open_and_done(client):
    client.post("/api/notes", json={"title": "Proj A", "body": "# A\n- [ ] alpha task\n- [x] done one"})
    client.post("/api/notes", json={"title": "Proj B", "body": "notes\n- [ ] beta task\ntext"})
    open_tasks = client.get("/api/tasks").json()
    texts = {t["text"] for t in open_tasks}
    assert texts == {"alpha task", "beta task"}          # done excluded by default
    assert all(t["done"] is False for t in open_tasks)
    # includes line + note metadata for navigation
    a = next(t for t in open_tasks if t["text"] == "alpha task")
    assert a["path"] == "proj-a.md" and a["line"] == 1
    # include_done surfaces completed ones too, open-first
    allt = client.get("/api/tasks?include_done=true").json()
    assert any(t["text"] == "done one" and t["done"] for t in allt)
    assert allt[0]["done"] is False


def test_encrypted_note_tasks_excluded(client):
    client.post("/api/vault/init", json={"passphrase": "taskspass12345"})
    client.post("/api/notes", json={"title": "Sealed", "body": "- [ ] secret task"})
    client.post("/api/notes/sealed.md/encrypt")
    assert all(t["text"] != "secret task" for t in client.get("/api/tasks").json())
