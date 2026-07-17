"""mnemo configuration — env-driven, sane homelab defaults."""
import os
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

# The vault: a directory of plain .md files you own. Source of truth.
VAULT = Path(os.environ.get("MNEMO_VAULT", Path.home() / "mnemo-vault")).expanduser()

# Where the rebuildable index + config + secrets live (inside the vault).
def mnemo_dir() -> Path:
    return VAULT / ".mnemo"

def db_path() -> Path:
    return mnemo_dir() / "index.db"

PORT = int(os.environ.get("MNEMO_PORT", "9111"))
HOST = os.environ.get("MNEMO_HOST", "0.0.0.0")

# Subdirectory (relative to vault) for daily notes.
DAILY_DIR = os.environ.get("MNEMO_DAILY_DIR", "journal")
# Subdirectory for captures (browser clips, audio, quick notes).
INBOX_DIR = os.environ.get("MNEMO_INBOX_DIR", "inbox")

WEB_DIR = ROOT / "web"

# Optional single bearer token for the API/PWA ("none" auth when empty).
AUTH_TOKEN = os.environ.get("MNEMO_AUTH_TOKEN", "")

# Background auto-sync with a peer mnemo (empty = off).
SYNC_PEER = os.environ.get("MNEMO_SYNC_PEER", "")
SYNC_TOKEN = os.environ.get("MNEMO_SYNC_TOKEN", "")      # peer's auth token, if any
SYNC_INTERVAL = int(os.environ.get("MNEMO_SYNC_INTERVAL", "0"))   # seconds; 0 = no timer
