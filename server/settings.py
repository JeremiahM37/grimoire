"""User settings — a small JSON store in .grimoire/settings.json. These override
environment defaults so the AI backend/model can be changed from the UI without
editing the systemd unit. Only non-secret operational settings live here.

Precedence for a value: settings.json → environment → built-in default.
"""
import json
import os
from typing import Any

from . import config

# settings that may be set from the UI, with their env fallback + default
FIELDS = {
    "llm": ("GRIMOIRE_LLM", ""),                     # '', 'ollama', 'claude' ('' = auto)
    "llm_model": ("GRIMOIRE_LLM_MODEL", "qwen3.5:4b"),
    "ollama_url": ("GRIMOIRE_OLLAMA_URL", ""),
    "embed_model": ("GRIMOIRE_EMBED_MODEL", "nomic-embed-text"),
    "local_embed": ("GRIMOIRE_LOCAL_EMBED", "auto"),   # auto|off — model2vec if installed
    "local_embed_model": ("GRIMOIRE_LOCAL_EMBED_MODEL", "minishlab/potion-base-8M"),
    "whisper_url": ("GRIMOIRE_WHISPER_URL", ""),
}


def _path():
    return config.grimoire_dir() / "settings.json"


def _load() -> dict:
    p = _path()
    if not p.exists():
        return {}
    try:
        return json.loads(p.read_text(encoding="utf-8"))
    except Exception:
        return {}


def get(key: str) -> Any:
    """Effective value: settings.json wins, then env, then built-in default."""
    env_key, default = FIELDS.get(key, (None, None))
    stored = _load().get(key)
    if stored not in (None, ""):
        return stored
    if env_key and os.environ.get(env_key):
        return os.environ[env_key]
    return default


def all_effective() -> dict:
    return {k: get(k) for k in FIELDS}


def update(patch: dict) -> dict:
    """Merge a patch into settings.json (only known FIELDS). Empty string clears
    a field back to the env/default. Returns the new effective settings."""
    data = _load()
    for k, v in patch.items():
        if k not in FIELDS:
            continue
        if v in (None, ""):
            data.pop(k, None)
        else:
            data[k] = v
    config.grimoire_dir().mkdir(parents=True, exist_ok=True)
    _path().write_text(json.dumps(data, indent=2), encoding="utf-8")
    return all_effective()


def reset_for_tests() -> None:
    p = _path()
    if p.exists():
        p.unlink()
