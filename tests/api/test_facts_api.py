"""Structured facts: `key:: value` inline fields projected from markdown for
deterministic agent lookup, plus write-back into the note."""


def test_facts_extracted_and_queryable(client):
    client.post("/api/notes", json={"title": "Service Alpha", "body": (
        "# Service Alpha\n\n"
        "port:: 8443\n"
        "owner:: platform-team\n\n"
        "Some prose about the service.\n")})
    # exact-key lookup
    hits = client.get("/api/facts", params={"key": "port"}).json()
    assert len(hits) == 1 and hits[0]["value"] == "8443"
    assert hits[0]["note"] == "service-alpha.md"
    # all facts in a note
    byn = {f["key"]: f["value"] for f in
           client.get("/api/facts", params={"note": "service-alpha.md"}).json()}
    assert byn == {"port": "8443", "owner": "platform-team"}


def test_facts_ignore_code_fences_and_prose(client):
    client.post("/api/notes", json={"title": "Doc", "body": (
        "real:: yes\n\n"
        "```\nnot_a_fact:: should be ignored inside code\n```\n\n"
        "Just a sentence with :: double colons but not a field.\n")})
    keys = {f["key"] for f in client.get("/api/facts", params={"note": "doc.md"}).json()}
    assert keys == {"real"}


def test_facts_update_on_edit_and_delete_with_note(client):
    client.post("/api/notes", json={"title": "Cfg", "body": "timeout:: 30\n"})
    assert client.get("/api/facts", params={"key": "timeout"}).json()[0]["value"] == "30"
    client.put("/api/notes/cfg.md", json={"body": "timeout:: 60\n"})
    assert client.get("/api/facts", params={"key": "timeout"}).json()[0]["value"] == "60"
    client.delete("/api/notes/cfg.md")
    assert client.get("/api/facts", params={"key": "timeout"}).json() == []


def test_set_fact_writes_back_into_markdown(client):
    client.post("/api/notes", json={"title": "Runbook", "body": "# Runbook\n\nsome text\n"})
    r = client.post("/api/facts", json={"note": "runbook.md", "key": "SLA", "value": "99.9%"})
    assert r.status_code == 201
    # it lives in the markdown body now, and is queryable (key lowercased)
    body = client.get("/api/notes/runbook.md").json()["body"]
    assert "SLA:: 99.9%" in body
    assert client.get("/api/facts", params={"key": "sla"}).json()[0]["value"] == "99.9%"
    # setting the same key updates in place, doesn't duplicate
    client.post("/api/facts", json={"note": "runbook.md", "key": "SLA", "value": "99.95%"})
    hits = client.get("/api/facts", params={"key": "sla"}).json()
    assert len(hits) == 1 and hits[0]["value"] == "99.95%"


def test_private_note_facts_excluded_by_default(client):
    client.post("/api/notes", json={"title": "Secret Cfg", "body": "apikey_hint:: rotate-monthly\n"})
    client.put("/api/notes/secret-cfg.md",
               json={"body": "apikey_hint:: rotate-monthly\n", "frontmatter": {"private": True}})
    assert client.get("/api/facts", params={"key": "apikey_hint"}).json() == []
    incl = client.get("/api/facts", params={"key": "apikey_hint", "include_private": True}).json()
    assert incl and incl[0]["value"] == "rotate-monthly"
