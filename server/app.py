"""grimoire app factory. Run: python -m server (or uvicorn server.app:create_app --factory)."""
import contextlib
import hmac
import os

from fastapi import FastAPI, Request
from fastapi.responses import FileResponse, JSONResponse
from fastapi.staticfiles import StaticFiles

from . import config, db, index
from .routers import ask, daily, media, misc, notes, read, search, sync, templates
from .routers import canvas as canvas_router
from .routers import crdt as crdt_router
from .routers import memory as memory_router
from .routers import plugins as plugins_router
from .routers import secrets as secrets_router
from .routers import settings as settings_router


def create_app() -> FastAPI:
    @contextlib.asynccontextmanager
    async def lifespan(app: FastAPI):
        config.grimoire_dir().mkdir(parents=True, exist_ok=True)
        db.init()
        index.reindex()   # rebuild the cache from the vault on boot
        index.ensure_embed_signature()   # re-embed if the backend changed
        watch = None
        if not os.environ.get("GRIMOIRE_NO_WATCHER"):
            from .watcher import watcher as watch
            watch.start()   # pick up external edits (other editors, sync) live
        sync_task = None
        if config.SYNC_PEER and config.SYNC_INTERVAL > 0:
            import asyncio

            from . import syncclient

            async def _sync_loop():
                while True:
                    await asyncio.sleep(config.SYNC_INTERVAL)
                    try:
                        await asyncio.get_running_loop().run_in_executor(
                            None, syncclient.sync_with_peer, config.SYNC_PEER, "server", config.SYNC_TOKEN)
                    except Exception:  # noqa: BLE001
                        pass   # transient peer outage — try again next tick
            sync_task = asyncio.create_task(_sync_loop())
        yield
        if sync_task:
            sync_task.cancel()
        if watch:
            watch.stop()
        db.close()

    app = FastAPI(title="Grimoire", version="1.0.0", lifespan=lifespan)

    # Security headers on every response. Strict CSP (no inline/external scripts)
    # is defense-in-depth against XSS; the renderers already escape HTML. No
    # frame-ancestors restriction so the app can still be embedded behind the
    # homelab reverse proxy — front that with Authelia/Tailscale as configured.
    CSP = ("default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "
           "img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; "
           "media-src 'self' blob:; object-src 'none'; base-uri 'self'; form-action 'self'")

    @app.middleware("http")
    async def security_headers(request: Request, call_next):
        resp = await call_next(request)
        resp.headers["X-Content-Type-Options"] = "nosniff"
        resp.headers["Referrer-Policy"] = "no-referrer"
        resp.headers["X-Frame-Options"] = os.environ.get("GRIMOIRE_FRAME_OPTIONS", "SAMEORIGIN")
        resp.headers.setdefault("Content-Security-Policy", CSP)
        resp.headers["Cross-Origin-Opener-Policy"] = "same-origin"
        return resp

    if config.AUTH_TOKEN:
        @app.middleware("http")
        async def auth(request: Request, call_next):
            if request.url.path.startswith("/api"):
                supplied = request.headers.get("authorization", "").removeprefix("Bearer ")
                qtoken = request.query_params.get("token", "")
                # constant-time comparison — no early-exit timing leak
                ok = (hmac.compare_digest(supplied, config.AUTH_TOKEN)
                      or hmac.compare_digest(qtoken, config.AUTH_TOKEN))
                if not ok:
                    return JSONResponse({"detail": "unauthorized"}, status_code=401)
            return await call_next(request)

    for r in (notes, search, daily, misc, ask, secrets_router, media, sync, read,
              templates, settings_router, crdt_router, plugins_router, canvas_router,
              memory_router):
        app.include_router(r.router)

    @app.get("/")
    def home():
        return FileResponse(config.WEB_DIR / "index.html")

    if config.WEB_DIR.exists():
        app.mount("/", StaticFiles(directory=config.WEB_DIR), name="web")
    return app
