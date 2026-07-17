"""v0.3 secret vault: init/unlock/lock, CRUD, grants, broker, audit."""


def _init_unlock(client, pw="supersecret1"):
    client.post("/api/vault/init", json={"passphrase": pw})
    # init leaves it unlocked; explicit unlock also works
    return pw


def test_status_and_init(client):
    assert client.get("/api/vault/status").json() == {
        "initialized": False, "unlocked": False, "count": None}
    _init_unlock(client)
    s = client.get("/api/vault/status").json()
    assert s["initialized"] and s["unlocked"]


def test_add_list_delete_secret(client):
    _init_unlock(client)
    client.post("/api/secrets", json={"name": "github", "value": "ghp_xxx",
                                      "meta": {"kind": "token"}})
    lst = client.get("/api/secrets").json()
    assert lst[0]["name"] == "github" and lst[0]["meta"]["kind"] == "token"
    # the VALUE is never returned anywhere
    assert "ghp_xxx" not in client.get("/api/secrets").text
    client.delete("/api/secrets/github")
    assert client.get("/api/secrets").json() == []


def test_lock_unlock_cycle(client):
    pw = _init_unlock(client)
    client.post("/api/secrets", json={"name": "k", "value": "v"})
    client.post("/api/vault/lock")
    assert client.get("/api/secrets").status_code == 423   # locked
    assert client.post("/api/vault/unlock", json={"passphrase": "nope"}).status_code == 401
    assert client.post("/api/vault/unlock", json={"passphrase": pw}).status_code == 200
    assert client.get("/api/secrets").json()[0]["name"] == "k"


def test_grant_and_broker(client, monkeypatch):
    _init_unlock(client)
    client.post("/api/secrets", json={"name": "svc", "value": "topsecret-key"})
    g = client.post("/api/secrets/svc/grant",
                    json={"grantee": "agent-1", "scope": "http://8.8.8.8/",
                          "ttl_seconds": 60}).json()["grant"]

    # broker injects the secret into the request; capture what goes out
    sent = {}

    class FakeResp:
        status = 200
        def read(self, n): return b'{"ok":true}'
        def __enter__(self): return self
        def __exit__(self, *a): return False

    def fake_urlopen(req, timeout=30):
        sent["header"] = req.headers.get("X-key")
        sent["url"] = req.full_url
        return FakeResp()
    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    r = client.post("/api/secrets/broker", json={
        "grant": g, "method": "GET", "url": "http://8.8.8.8/api", "header": "X-key"})
    assert r.status_code == 200 and r.json()["status"] == 200
    assert sent["header"] == "topsecret-key"          # secret was injected...
    assert "topsecret-key" not in r.text              # ...but never returned to caller


def test_audit_records_use(client):
    _init_unlock(client)
    client.post("/api/secrets", json={"name": "a", "value": "v"})
    actions = [row["action"] for row in client.get("/api/audit").json()]
    assert "set" in actions
