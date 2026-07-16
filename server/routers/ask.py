"""Ask-your-notes (RAG) + inline AI actions."""
from fastapi import APIRouter
from pydantic import BaseModel

from .. import ai, index

router = APIRouter(prefix="/api")


class AskIn(BaseModel):
    q: str
    k: int = 6
    include_private: bool = False


@router.post("/ask")
def ask(a: AskIn):
    """Answer a question from the notes, with citations. Private notes excluded
    unless explicitly opted in."""
    if not a.q.strip():
        return {"answer": "", "citations": []}
    ctx = index.retrieve(a.q, k=a.k, include_private=a.include_private)
    ans = ai.answer(a.q, ctx)
    return {"answer": ans,
            "citations": [{"path": c["path"], "title": c["title"], "score": c["score"]}
                          for c in ctx]}


@router.get("/retrieve")
def retrieve(q: str, k: int = 6, include_private: bool = False):
    return index.retrieve(q, k=k, include_private=include_private)


class ActionIn(BaseModel):
    action: str          # summarize | expand | tags | title
    text: str


@router.post("/actions")
def actions(a: ActionIn):
    """Inline editor AI actions. Uses the LLM when configured, else a useful
    deterministic fallback so the feature works offline."""
    text = a.text.strip()
    if a.action == "tags":
        return {"result": ai_suggest_tags(text)}
    if a.action == "title":
        return {"result": _first_line_title(text)}
    prompt = {
        "summarize": f"Summarize these notes in 2-3 sentences:\n\n{text}",
        "expand": f"Expand these notes into a fuller draft, keeping the meaning:\n\n{text}",
    }.get(a.action)
    if not prompt:
        return {"result": "", "error": f"unknown action {a.action!r}"}
    out = ai.answer(prompt, [{"path": "_", "title": "selection", "chunk": text}])
    return {"result": out}


def ai_suggest_tags(text: str) -> list[str]:
    from collections import Counter
    import re
    words = re.findall(r"[a-z][a-z-]{3,}", text.lower())
    stop = {"this", "that", "with", "from", "have", "your", "into", "notes", "which",
            "these", "there", "their", "about", "would", "could", "should"}
    common = [w for w, _ in Counter(w for w in words if w not in stop).most_common(5)]
    return common


def _first_line_title(text: str) -> str:
    for line in text.splitlines():
        s = line.lstrip("# ").strip()
        if s:
            return s[:80]
    return "Untitled"
