"""mnemo app factory. Run: python -m server (or uvicorn server.app:create_app --factory)."""
import contextlib
import os

from fastapi import FastAPI, Request
from fastapi.responses import FileResponse, JSONResponse
from fastapi.staticfiles import StaticFiles

from . import config, db, index
from .routers import (ask, daily, media, misc, notes, read, search,
                      secrets as secrets_router, settings as settings_router,
                      sync, templates)


def create_app() -> FastAPI:
    @contextlib.asynccontextmanager
    async def lifespan(app: FastAPI):
        config.mnemo_dir().mkdir(parents=True, exist_ok=True)
        db.init()
        index.reindex()   # rebuild the cache from the vault on boot
        watch = None
        if not os.environ.get("MNEMO_NO_WATCHER"):
            from .watcher import watcher as watch
            watch.start()   # pick up external edits (Obsidian/vim/sync) live
        yield
        if watch:
            watch.stop()
        db.close()

    app = FastAPI(title="mnemo", version="0.1.0", lifespan=lifespan)

    if config.AUTH_TOKEN:
        @app.middleware("http")
        async def auth(request: Request, call_next):
            if request.url.path.startswith("/api"):
                supplied = request.headers.get("authorization", "").removeprefix("Bearer ")
                if supplied != config.AUTH_TOKEN and \
                        request.query_params.get("token") != config.AUTH_TOKEN:
                    return JSONResponse({"detail": "unauthorized"}, status_code=401)
            return await call_next(request)

    for r in (notes, search, daily, misc, ask, secrets_router, media, sync, read,
              templates, settings_router):
        app.include_router(r.router)

    @app.get("/")
    def home():
        return FileResponse(config.WEB_DIR / "index.html")

    if config.WEB_DIR.exists():
        app.mount("/", StaticFiles(directory=config.WEB_DIR), name="web")
    return app
