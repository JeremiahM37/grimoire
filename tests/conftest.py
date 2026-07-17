"""Shared fixtures. Each test gets a fresh temp vault — fully hermetic."""
import pytest


@pytest.fixture(autouse=True)
def _offline_ai(monkeypatch):
    """Strip AI backend env so unit/api tests are deterministic regardless of the
    developer's/deploy box's ambient env (which may point at a live Ollama)."""
    for var in ("MNEMO_OLLAMA_URL", "MNEMO_LLM", "MNEMO_LLM_MODEL", "MNEMO_WHISPER_URL",
                "MNEMO_BROKER_ALLOW_PRIVATE", "MNEMO_VAULT_IDLE_LOCK"):
        monkeypatch.delenv(var, raising=False)


@pytest.fixture()
def vaultdir(tmp_path, monkeypatch):
    """A fresh temp vault dir with the index initialized. Returns its Path."""
    from server import config, db, index, secrets
    vdir = tmp_path / "vault"
    vdir.mkdir()
    monkeypatch.setattr(config, "VAULT", vdir)
    config.mnemo_dir().mkdir(parents=True, exist_ok=True)
    db.init(config.db_path())
    secrets.reset_for_tests()   # drop any in-memory vault key from a prior test
    index.reindex()
    yield vdir
    secrets.reset_for_tests()
    db.close()


@pytest.fixture()
def client(vaultdir):
    from fastapi.testclient import TestClient
    from server.app import create_app
    with TestClient(create_app()) as c:
        yield c
    from server import db
    db.close()
