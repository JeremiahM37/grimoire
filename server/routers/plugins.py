"""Plugin API: list, enable/disable, and path-confined asset serving."""
import mimetypes

from fastapi import APIRouter, HTTPException
from fastapi.responses import FileResponse
from pydantic import BaseModel

from .. import plugins

router = APIRouter()


@router.get("/api/plugins")
def list_plugins():
    return [{**p, "client_url": f"/plugins/{p['name']}/{p['client']}",
             "styles_url": f"/plugins/{p['name']}/{p['styles']}" if p["styles"] else None}
            for p in plugins.discover()]


class ScaffoldIn(BaseModel):
    name: str


@router.post("/api/plugins/scaffold", status_code=201)
def scaffold_plugin(body: ScaffoldIn):
    """Create a hello-world vault plugin skeleton (disabled until enabled)."""
    try:
        return plugins.scaffold(body.name.strip().lower())
    except ValueError as e:
        raise HTTPException(400, str(e)) from None
    except FileExistsError:
        raise HTTPException(409, "plugin directory already exists") from None


class EnableIn(BaseModel):
    enabled: bool


@router.post("/api/plugins/{name}/enable")
def enable_plugin(name: str, body: EnableIn):
    p = plugins.set_enabled(name, body.enabled)
    if p is None:
        raise HTTPException(404, "no such plugin")
    return p


@router.get("/plugins/{name}/{rel:path}")
def plugin_asset(name: str, rel: str):
    p = plugins.asset_path(name, rel)
    if p is None:
        raise HTTPException(404, "not found")
    mime = mimetypes.guess_type(str(p))[0] or "application/octet-stream"
    return FileResponse(p, media_type=mime)
