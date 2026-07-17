"""CRDT sync endpoints."""
from server import crdt


def test_get_doc_returns_body_crdt_and_fm(client):
    client.post("/api/notes", json={"title": "Doc Note", "body": "hello crdt world"})
    r = client.get("/api/crdt/doc/doc-note.md")
    assert r.status_code == 200
    d = r.json()
    assert d["path"] == "doc-note.md" and d["fm"]["title"] == "Doc Note"
    # the returned doc materializes back to the note's stored body
    note_body = client.get("/api/notes/doc-note.md").json()["body"]
    assert crdt.Doc.from_json(d["doc"], "x").text() == note_body
    assert "hello crdt world" in note_body


def test_get_doc_404_for_missing(client):
    assert client.get("/api/crdt/doc/nope.md").status_code == 404


def test_encrypted_note_not_crdt_mergeable(client):
    client.post("/api/vault/init", json={"passphrase": "crdtpass12345"})
    client.post("/api/notes", json={"title": "Enc", "body": "secret text"})
    client.post("/api/notes/enc.md/encrypt")
    assert client.get("/api/crdt/doc/enc.md").status_code == 409          # can't merge ciphertext


def test_merge_applies_derived_peer_doc(client):
    client.post("/api/notes", json={"title": "Merge Target", "body": "base text"})
    # a peer that started from OUR doc (shared ids — as real sync guarantees) and edited
    ours = client.get("/api/crdt/doc/merge-target.md").json()["doc"]
    peer = crdt.Doc.from_json(ours, "peer")
    peer.local_edit(peer.text().rstrip("\n") + " and more")
    r = client.post("/api/crdt/merge", json={"path": "merge-target.md", "doc": peer.to_json(),
                                             "fm": {"title": "Merge Target", "updated": "2099"}})
    assert r.status_code == 200 and r.json()["conflict"] is False
    body = client.get("/api/notes/merge-target.md").json()["body"]
    assert "and more" in body        # cleanly merged, no interleaving


def test_independent_docs_conflict_instead_of_interleaving(client):
    client.post("/api/notes", json={"title": "Indep", "body": "alpha beta gamma"})
    # a peer doc created INDEPENDENTLY (disjoint ids), newer frontmatter
    peer = crdt.Doc.from_text("delta epsilon zeta", "peer")
    r = client.post("/api/crdt/merge", json={"path": "indep.md", "doc": peer.to_json(),
                                             "fm": {"title": "Indep", "updated": "2099-01-01"}})
    assert r.json()["conflict"] is True
    body = client.get("/api/notes/indep.md").json()["body"]
    assert "delta epsilon zeta" in body and "alpha" not in body   # clean replace, not garbled
    # our version was preserved as a conflict copy
    paths = [n["path"] for n in client.get("/api/notes").json()]
    assert any("conflict" in p for p in paths)
