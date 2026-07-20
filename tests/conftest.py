"""Shared fixtures. Each test gets a fresh temp vault — fully hermetic."""
import pytest


@pytest.fixture(autouse=True)
def _offline_ai(monkeypatch):
    """Strip AI backend env so unit/api tests are deterministic regardless of the
    developer's/deploy box's ambient env (which may point at a live Ollama)."""
    for var in ("GRIMOIRE_OLLAMA_URL", "GRIMOIRE_LLM", "GRIMOIRE_LLM_MODEL", "GRIMOIRE_WHISPER_URL",
                "GRIMOIRE_BROKER_ALLOW_PRIVATE", "GRIMOIRE_VAULT_IDLE_LOCK"):
        monkeypatch.delenv(var, raising=False)
    # The module-level watcher singleton must NOT run in unit/api tests: every
    # TestClient app shares it, and a late debounced reindex from a previous
    # test's vault wrote stale rows into the next test's index (the historic
    # intermittent notes.path IntegrityError). The watcher has its own
    # dedicated integration test with a private VaultWatcher instance.
    monkeypatch.setenv("GRIMOIRE_NO_WATCHER", "1")
    # model2vec may be installed in the dev venv — tests need the hasher
    monkeypatch.setenv("GRIMOIRE_LOCAL_EMBED", "off")


@pytest.fixture()
def vaultdir(tmp_path, monkeypatch):
    """A fresh temp vault dir with the index initialized. Returns its Path."""
    from server import config, db, index, secrets
    vdir = tmp_path / "vault"
    vdir.mkdir()
    monkeypatch.setattr(config, "VAULT", vdir)
    config.grimoire_dir().mkdir(parents=True, exist_ok=True)
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
