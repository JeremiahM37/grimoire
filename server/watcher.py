"""Vault watcher — the "edit anywhere" guarantee.

Watches the vault directory for `.md` changes made OUTSIDE mnemo (Obsidian, vim,
git pull, Syncthing, another device's sync) and reconciles the index. Debounced
so a burst of saves collapses to one reindex pass. The `.mnemo/` dir is ignored.
"""
import logging
import threading
import time
from pathlib import Path

from watchdog.events import FileSystemEventHandler
from watchdog.observers import Observer

from . import config, index, vault

log = logging.getLogger("mnemo.watcher")


class _Handler(FileSystemEventHandler):
    def __init__(self, on_change):
        self._on_change = on_change

    def _relevant(self, path: str) -> bool:
        p = Path(path)
        return p.suffix == ".md" and ".mnemo" not in p.parts and not p.name.endswith(".tmp")

    def on_any_event(self, event):
        if event.is_directory:
            return
        for path in (getattr(event, "dest_path", "") or "", event.src_path):
            if path and self._relevant(path):
                self._on_change(path)


class VaultWatcher:
    """Debounced observer. Coalesces file events into single-note upserts/removes;
    an external edit shows up in search/links within ~`debounce` seconds."""

    def __init__(self, debounce: float = 0.6):
        self.debounce = debounce
        self._obs: Observer | None = None
        self._pending: dict[str, float] = {}
        self._lock = threading.Lock()
        self._timer: threading.Timer | None = None

    def _queue(self, path: str) -> None:
        with self._lock:
            self._pending[path] = time.time()
            if self._timer is None:
                self._timer = threading.Timer(self.debounce, self._flush)
                self._timer.daemon = True
                self._timer.start()

    def _flush(self) -> None:
        with self._lock:
            paths = list(self._pending)
            self._pending.clear()
            self._timer = None
        for abspath in paths:
            try:
                p = Path(abspath)
                rel = vault.rel_of(p) if p.exists() else \
                    str(p.resolve()).replace(str(config.VAULT.resolve()) + "/", "")
                if p.exists():
                    index.upsert(rel)
                else:
                    index.remove(rel)
            except Exception:
                log.debug("watcher reconcile skipped %s", abspath)

    def start(self) -> None:
        config.VAULT.mkdir(parents=True, exist_ok=True)
        self._obs = Observer()
        self._obs.schedule(_Handler(self._queue), str(config.VAULT), recursive=True)
        self._obs.daemon = True
        self._obs.start()
        log.info("vault watcher started on %s", config.VAULT)

    def stop(self) -> None:
        if self._obs:
            self._obs.stop()
            self._obs.join(timeout=2)
            self._obs = None


watcher = VaultWatcher()
