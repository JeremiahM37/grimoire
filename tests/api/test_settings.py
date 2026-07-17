"""User settings store — overrides env for AI backend/model, honored live."""


def test_defaults_are_offline_in_tests(client):
    st = client.get("/api/settings").json()
    assert st["answer_backend"] == "extractive"          # no LLM reachable in tests
    assert st["settings"]["embed_model"] == "nomic-embed-text"


def test_setting_ollama_url_enables_generative_backend(client):
    r = client.put("/api/settings", json={"ollama_url": "http://127.0.0.1:11434"})
    body = r.json()
    assert body["settings"]["ollama_url"] == "http://127.0.0.1:11434"
    assert body["answer_backend"] == "ollama"            # auto-enabled by the URL
    # persisted across requests
    assert client.get("/api/settings").json()["answer_backend"] == "ollama"


def test_clearing_a_setting_reverts_to_default(client):
    client.put("/api/settings", json={"llm_model": "custom:7b"})
    assert client.get("/api/settings").json()["settings"]["llm_model"] == "custom:7b"
    client.put("/api/settings", json={"llm_model": ""})   # empty clears it
    assert client.get("/api/settings").json()["settings"]["llm_model"] == "qwen3.5:4b"


def test_invalid_llm_backend_rejected(client):
    assert client.put("/api/settings", json={"llm": "bogus"}).status_code == 400
    for ok in ("", "ollama", "claude"):
        assert client.put("/api/settings", json={"llm": ok}).status_code == 200


def test_embed_model_not_editable_via_settings(client):
    # embed_model isn't in the patch schema — sending it is ignored (would corrupt vectors)
    client.put("/api/settings", json={"embed_model": "something-else"})
    assert client.get("/api/settings").json()["settings"]["embed_model"] == "nomic-embed-text"
