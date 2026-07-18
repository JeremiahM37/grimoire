"""Canvas API: create/list/get/put/delete, JSON Canvas validation, path
confinement, and the size cap."""


def _mk(client, name="Board"):
    return client.post("/api/canvas", json={"name": name}).json()


def test_create_list_get_roundtrip(client):
    made = _mk(client)
    assert made["path"] == "canvases/board.canvas"
    assert [c["name"] for c in client.get("/api/canvas").json()] == ["board"]
    doc = client.get(f"/api/canvas/{made['path']}").json()
    assert doc["nodes"] == [] and doc["edges"] == []


def test_put_and_reload_nodes_edges(client):
    made = _mk(client)
    nodes = [{"id": "a", "type": "text", "text": "hello", "x": 0, "y": 0,
              "width": 200, "height": 80},
             {"id": "b", "type": "file", "file": "note.md", "x": 300, "y": 100,
              "width": 220, "height": 60}]
    edges = [{"id": "e1", "fromNode": "a", "toNode": "b"}]
    r = client.put(f"/api/canvas/{made['path']}", json={"nodes": nodes, "edges": edges})
    assert r.status_code == 200
    doc = client.get(f"/api/canvas/{made['path']}").json()
    assert doc["nodes"][0]["text"] == "hello" and doc["edges"][0]["id"] == "e1"


def test_edge_referencing_missing_node_rejected(client):
    made = _mk(client)
    r = client.put(f"/api/canvas/{made['path']}", json={
        "nodes": [{"id": "a", "x": 0, "y": 0}],
        "edges": [{"id": "e", "fromNode": "a", "toNode": "ghost"}]})
    assert r.status_code == 400


def test_node_without_string_id_rejected(client):
    made = _mk(client)
    r = client.put(f"/api/canvas/{made['path']}", json={
        "nodes": [{"id": 42, "x": 0, "y": 0}], "edges": []})
    assert r.status_code == 400


def test_size_cap(client):
    made = _mk(client)
    huge = [{"id": f"n{i}", "text": "x" * 4000, "x": 0, "y": 0} for i in range(400)]
    r = client.put(f"/api/canvas/{made['path']}", json={"nodes": huge, "edges": []})
    assert r.status_code == 413


def test_duplicate_create_conflicts(client):
    _mk(client)
    assert client.post("/api/canvas", json={"name": "Board"}).status_code == 409


def test_delete(client):
    made = _mk(client)
    assert client.delete(f"/api/canvas/{made['path']}").status_code == 204
    assert client.get(f"/api/canvas/{made['path']}").status_code == 404


def test_path_confinement(client):
    import pytest

    from server.routers.canvas import _canvas_path
    from server.vault import VaultError
    with pytest.raises(VaultError):
        _canvas_path("../../etc/evil")
