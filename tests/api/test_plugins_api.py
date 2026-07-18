"""Plugin subsystem: discovery, trust defaults, enable/disable persistence,
asset serving + path confinement."""


def test_builtin_plugins_discovered_and_enabled_by_default(client):
    plugins = client.get("/api/plugins").json()
    names = {p["name"] for p in plugins}
    assert {"katex", "mermaid", "kanban", "pomodoro", "vault-stats",
            "journal-heatmap", "word-goal"} <= names
    assert all(p["enabled"] for p in plugins if p["source"] == "builtin")


def test_vault_plugin_disabled_by_default(client, vaultdir):
    pdir = vaultdir / "plugins" / "my-ext"
    pdir.mkdir(parents=True)
    (pdir / "plugin.json").write_text(
        '{"name": "my-ext", "version": "0.1.0", "description": "x", "client": "client.js"}')
    (pdir / "client.js").write_text("export function activate() {}")
    plugins = {p["name"]: p for p in client.get("/api/plugins").json()}
    assert plugins["my-ext"]["source"] == "vault"
    assert plugins["my-ext"]["enabled"] is False        # untrusted until opted in


def test_enable_disable_persists(client, vaultdir):
    pdir = vaultdir / "plugins" / "my-ext"
    pdir.mkdir(parents=True)
    (pdir / "plugin.json").write_text(
        '{"name": "my-ext", "version": "0.1.0", "description": "x", "client": "client.js"}')
    (pdir / "client.js").write_text("export function activate() {}")
    r = client.post("/api/plugins/my-ext/enable", json={"enabled": True})
    assert r.status_code == 200 and r.json()["enabled"] is True
    plugins = {p["name"]: p for p in client.get("/api/plugins").json()}
    assert plugins["my-ext"]["enabled"] is True
    client.post("/api/plugins/my-ext/enable", json={"enabled": False})
    plugins = {p["name"]: p for p in client.get("/api/plugins").json()}
    assert plugins["my-ext"]["enabled"] is False


def test_enable_unknown_plugin_404(client):
    assert client.post("/api/plugins/ghost/enable",
                       json={"enabled": True}).status_code == 404


def test_asset_serving_and_disabled_plugins_serve_nothing(client, vaultdir):
    pdir = vaultdir / "plugins" / "my-ext"
    pdir.mkdir(parents=True)
    (pdir / "plugin.json").write_text(
        '{"name": "my-ext", "version": "0.1.0", "description": "x", "client": "client.js"}')
    (pdir / "client.js").write_text("export function activate() {}")
    # disabled → assets 404 (code of a disabled plugin must never load)
    assert client.get("/plugins/my-ext/client.js").status_code == 404
    client.post("/api/plugins/my-ext/enable", json={"enabled": True})
    r = client.get("/plugins/my-ext/client.js")
    assert r.status_code == 200 and "activate" in r.text


def test_asset_path_traversal_blocked(client):
    """Traversal out of a plugin's directory must never resolve.

    Plain `../` in a URL is normalized away by HTTP clients before it reaches
    the route (so it can't express traversal), but an encoded %2f arrives at
    the handler verbatim — and the resolver itself must confine regardless of
    transport, so both layers are pinned here."""
    from server import plugins as plugin_mod
    for evil in ("../../server/config.py", "../katex/client.js",
                 "../../../../etc/passwd"):
        assert plugin_mod.asset_path("kanban", evil) is None
    assert client.get(
        "/plugins/kanban/..%2f..%2fserver%2fconfig.py").status_code == 404


def test_malformed_manifests_are_skipped(client, vaultdir):
    bad1 = vaultdir / "plugins" / "bad-json"
    bad1.mkdir(parents=True)
    (bad1 / "plugin.json").write_text("{not json")
    bad2 = vaultdir / "plugins" / "name-mismatch"
    bad2.mkdir(parents=True)
    (bad2 / "plugin.json").write_text(
        '{"name": "other", "version": "1.0", "client": "client.js"}')
    (bad2 / "client.js").write_text("x")
    names = {p["name"] for p in client.get("/api/plugins").json()}
    assert "bad-json" not in names and "name-mismatch" not in names and "other" not in names


def test_builtin_name_shadows_vault_clone(client, vaultdir):
    """A vault plugin cannot impersonate a builtin (builtin wins the name)."""
    pdir = vaultdir / "plugins" / "katex"
    pdir.mkdir(parents=True)
    (pdir / "plugin.json").write_text(
        '{"name": "katex", "version": "9.9.9", "description": "evil", "client": "client.js"}')
    (pdir / "client.js").write_text("export function activate() {}")
    plugins = [p for p in client.get("/api/plugins").json() if p["name"] == "katex"]
    assert len(plugins) == 1 and plugins[0]["source"] == "builtin"
    assert plugins[0]["version"] != "9.9.9"


def test_scaffold_creates_disabled_vault_plugin(client, vaultdir):
    r = client.post("/api/plugins/scaffold", json={"name": "my-idea"})
    assert r.status_code == 201
    made = r.json()
    assert made["enabled"] is False                     # scaffolding ≠ execution
    assert (vaultdir / "plugins" / "my-idea" / "plugin.json").is_file()
    assert "activate" in (vaultdir / "plugins" / "my-idea" / "client.js").read_text()
    listed = {p["name"]: p for p in client.get("/api/plugins").json()}
    assert listed["my-idea"]["source"] == "vault" and listed["my-idea"]["enabled"] is False


def test_scaffold_rejects_bad_names_and_duplicates(client, vaultdir):
    # NOTE: uppercase names are normalized (UPPER → upper), not rejected
    for bad in ("Bad Name", "../evil", "kanban"):   # kanban = builtin clash
        assert client.post("/api/plugins/scaffold",
                           json={"name": bad}).status_code in (400, 409)
    client.post("/api/plugins/scaffold", json={"name": "dupe"})
    assert client.post("/api/plugins/scaffold",
                       json={"name": "dupe"}).status_code == 400
