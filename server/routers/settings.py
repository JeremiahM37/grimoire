"""Read/update user settings (AI backend/model, service URLs). embed_model is
intentionally NOT editable here — changing it would invalidate stored vectors."""
from fastapi import APIRouter
from pydantic import BaseModel

from .. import ai, settings

router = APIRouter(prefix="/api")


def _state() -> dict:
    return {"settings": settings.all_effective(),
            "answer_backend": ai._answer_backend() or "extractive"}


@router.get("/settings")
def get_settings():
    return _state()


class SettingsPatch(BaseModel):
    llm: str | None = None            # '', 'ollama', 'claude'
    llm_model: str | None = None
    ollama_url: str | None = None
    whisper_url: str | None = None


@router.put("/settings")
def put_settings(p: SettingsPatch):
    patch = {k: v for k, v in p.model_dump(exclude_unset=True).items()}
    settings.update(patch)
    return _state()
