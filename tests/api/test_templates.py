"""Note templates: stored in templates/ (out of the note graph), applied with
variable expansion into a real note."""
import datetime


def test_template_is_not_indexed_as_a_note(client):
    before = len(client.get("/api/notes").json())
    client.post("/api/templates", json={"name": "Meeting", "body": "# {{title}}\n\n- attendees:"})
    # template exists...
    tpls = client.get("/api/templates").json()
    assert any(t["name"] == "Meeting" for t in tpls)
    # ...but did NOT become a note / enter search
    assert len(client.get("/api/notes").json()) == before
    assert client.get("/api/search?q=attendees").json() == []


def test_apply_template_expands_vars_and_creates_note(client):
    client.post("/api/templates", json={
        "name": "Standup",
        "body": "# {{title}}\n\ndate: {{date}}\ntime: {{time}}\n\n## Notes\n"})
    r = client.post("/api/templates/apply", json={"template": "templates/standup.md",
                                                  "title": "Standup Notes"})
    assert r.status_code == 201
    note = client.get("/api/notes/" + r.json()["path"]).json()
    today = datetime.date.today().isoformat()
    assert f"date: {today}" in note["body"]
    assert "{{" not in note["body"]                     # all vars expanded
    assert note["title"] == "Standup Notes"


def test_apply_missing_template_404(client):
    r = client.post("/api/templates/apply", json={"template": "templates/nope.md", "title": "x"})
    assert r.status_code == 404


def test_apply_rejects_non_template_path(client):
    # can't use apply to read arbitrary/reserved files
    client.post("/api/notes", json={"title": "Regular", "body": "secret"})
    r = client.post("/api/templates/apply", json={"template": "regular.md", "title": "x"})
    assert r.status_code == 400
    r2 = client.post("/api/templates/apply", json={"template": ".grimoire/secrets.enc", "title": "x"})
    assert r2.status_code == 400
