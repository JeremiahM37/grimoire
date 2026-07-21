"""AI layer: embeddings, retrieval, and answer synthesis — pluggable and offline-safe.

Design principle: grimoire works **fully offline with zero external deps** by default,
and gets *smarter* (not merely functional) when you point it at a local Ollama or
Claude. So:

- Embeddings default to a deterministic **hashing embedder** (bag-of-tokens hashed
  into a fixed-dim L2-normalized vector). Cosine over these reflects real token
  overlap — good enough for retrieval, instant, private, and deterministic for
  tests. Set GRIMOIRE_OLLAMA_URL + GRIMOIRE_EMBED_MODEL for semantic embeddings.
- Answers default to an **extractive** synthesizer (stitches the most relevant
  note chunks with citations). Set GRIMOIRE_LLM=ollama|claude for generative answers.

Every note chunk is embedded on index; private notes are excluded from the vector
store unless a query explicitly opts in.
"""
import hashlib
import json
import math
import os
import re
import struct
import urllib.request

EMBED_DIM = 256

# ---- config (settings.json → env → default, read live) ----------------------
# imported lazily to avoid a cycle (settings imports config only)

def _cfg(key: str) -> str:
    from . import settings
    return settings.get(key) or ""

def _ollama_url() -> str:
    return _cfg("ollama_url").rstrip("/")

def _embed_model() -> str:
    return _cfg("embed_model") or "nomic-embed-text"

def _llm() -> str:
    return _cfg("llm").lower()   # '', 'ollama', 'claude', 'openai'

def _llm_model() -> str:
    return _cfg("llm_model") or "qwen3.5:4b"

def _llm_base_url() -> str:
    return _cfg("llm_base_url").rstrip("/")

def _llm_api_key() -> str:
    """Key for the OpenAI-compatible backend. Explicit setting/env wins; else
    dogfood the credential vault (an unlocked secret named 'llm-api-key'), so
    the key is stored the same audited way agents' secrets are. Local servers
    (vLLM, LM Studio, Ollama's OpenAI shim) usually need no key at all."""
    key = _cfg("llm_api_key")
    if key:
        return key
    try:
        from . import secrets
        return secrets._get_value("llm-api-key") or ""
    except Exception:
        return ""


# ---- chunking ---------------------------------------------------------------

def _split_long_para(p: str, target: int) -> list[str]:
    """A paragraph without blank lines (a transcript, a log, hard-wrapped
    prose) must not become one giant chunk — split it on line boundaries,
    falling back to sentence boundaries for a single enormous line."""
    if len(p) <= target * 1.5:
        return [p]
    units = p.splitlines()
    if len(units) == 1:
        units = re.split(r"(?<=[.!?])\s+", p)
    out, cur = [], ""
    for u in units:
        if len(cur) + len(u) + 1 > target and cur:
            out.append(cur)
            cur = u
        else:
            cur = (cur + "\n" + u) if cur else u
    if cur:
        out.append(cur)
    return out


def chunk_text(text: str, target: int = 800) -> list[str]:
    """Split on blank lines (long paragraphs split further on lines/sentences),
    then greedily pack pieces to ~target chars."""
    paras = [q for p in re.split(r"\n\s*\n", text) if p.strip()
             for q in _split_long_para(p.strip(), target)]
    chunks, cur = [], ""
    for p in paras:
        if len(cur) + len(p) + 2 > target and cur:
            chunks.append(cur.strip())
            cur = p
        else:
            cur = (cur + "\n\n" + p) if cur else p
    if cur.strip():
        chunks.append(cur.strip())
    return chunks or ([text.strip()] if text.strip() else [])


# ---- embeddings -------------------------------------------------------------

_TOKEN_RE = re.compile(r"[a-z0-9]+")


def _hash_embed(text: str) -> list[float]:
    vec = [0.0] * EMBED_DIM
    for tok in _TOKEN_RE.findall(text.lower()):
        h = int(hashlib.md5(tok.encode()).hexdigest(), 16)
        idx = h % EMBED_DIM
        sign = 1.0 if (h >> 8) & 1 else -1.0
        vec[idx] += sign
    norm = math.sqrt(sum(v * v for v in vec)) or 1.0
    return [v / norm for v in vec]


# optional local semantic embeddings: `pip install model2vec` and grimoire
# picks them up automatically (static embeddings — numpy-only, ~30MB model,
# instant encode). Ollama still wins when configured; hashing remains the
# zero-dependency floor. Disable with GRIMOIRE_LOCAL_EMBED=off.
_m2v_model = None
_m2v_failed = False


def _local_enabled() -> bool:
    return str(_cfg("local_embed") or "auto").lower() not in ("off", "0", "false", "no")


def _local_model_name() -> str:
    return _cfg("local_embed_model") or "minishlab/potion-base-8M"


def _m2v():
    global _m2v_model, _m2v_failed
    if _m2v_model is not None or _m2v_failed:
        return _m2v_model
    try:
        from model2vec import StaticModel
        _m2v_model = StaticModel.from_pretrained(_local_model_name())
    except Exception:
        _m2v_failed = True   # not installed / model not cached offline — fine
    return _m2v_model


def embed(texts: list[str]) -> list[list[float]]:
    """Embed a batch: Ollama when configured, else the local model2vec model
    when installed, else the deterministic hasher."""
    url = _ollama_url()
    if url:
        try:
            return [_ollama_embed(url, t) for t in texts]
        except Exception:
            pass   # graceful offline fallback — never break indexing on AI outage
    if _local_enabled():
        m = _m2v()
        if m is not None:
            try:
                return [[float(x) for x in v] for v in m.encode(texts)]
            except Exception:
                pass
    return [_hash_embed(t) for t in texts]


def embed_signature() -> str:
    """Identifies the active embedding backend. Vectors from different
    backends are not comparable — the index re-embeds when this changes."""
    if _ollama_url():
        return f"ollama:{_embed_model()}"
    if _local_enabled() and _m2v() is not None:
        return f"model2vec:{_local_model_name()}"
    return "hash:v1"


def _ollama_embed(url: str, text: str) -> list[float]:
    req = urllib.request.Request(
        f"{url}/api/embeddings", method="POST",
        headers={"Content-Type": "application/json"},
        data=json.dumps({"model": _embed_model(), "prompt": text}).encode())
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.load(r)["embedding"]


def pack(vec: list[float]) -> bytes:
    return struct.pack(f"<{len(vec)}f", *vec)


def unpack(blob: bytes) -> list[float]:
    return list(struct.unpack(f"<{len(blob) // 4}f", blob))


def cosine(a: list[float], b: list[float]) -> float:
    if len(a) != len(b):
        return 0.0
    dot = sum(x * y for x, y in zip(a, b, strict=False))
    na = math.sqrt(sum(x * x for x in a)) or 1.0
    nb = math.sqrt(sum(y * y for y in b)) or 1.0
    return dot / (na * nb)


# ---- answer synthesis -------------------------------------------------------

def _answer_backend() -> str:
    """Which LLM (if any) synthesizes answers. Explicit GRIMOIRE_LLM wins; otherwise
    if a local Ollama is reachable we AUTO-enable generative answers (the homelab
    deployment sets only GRIMOIRE_OLLAMA_URL). Falls back to extractive when neither
    is present — keeping the offline default and hermetic tests deterministic."""
    b = _llm()
    if b in ("ollama", "claude", "openai"):
        return b
    if _ollama_url():
        return "ollama"
    return ""


def answer(question: str, contexts: list[dict]) -> str:
    """contexts: [{path, title, chunk}]. Generative when an LLM is available, else extractive."""
    if not contexts:
        return "I couldn't find anything in your notes about that."
    backend = _answer_backend()
    if backend:
        try:
            return _llm_answer(question, contexts, backend)
        except Exception:
            pass   # fall through to extractive — never fail the request on AI outage
    return _extractive_answer(question, contexts)


def _extractive_answer(question: str, contexts: list[dict]) -> str:
    lead = f"From your notes on “{question.strip()}”:\n\n"
    parts = []
    for c in contexts[:4]:
        snippet = c["chunk"].strip()
        if len(snippet) > 400:
            snippet = snippet[:400].rsplit(" ", 1)[0] + " …"
        parts.append(f"- {snippet}  ([[{_stem(c['path'])}|{c['title']}]])")
    return lead + "\n".join(parts)


def _llm_answer(question: str, contexts: list[dict], backend: str) -> str:
    ctx = "\n\n".join(f"[{i+1}] ({c['title']})\n{c['chunk']}"
                      for i, c in enumerate(contexts[:6]))
    prompt = (
        "Answer the question using ONLY the notes below. Cite sources as [n]. If the "
        "notes don't contain the answer, say so.\n\n"
        f"NOTES:\n{ctx}\n\nQUESTION: {question}\n\nANSWER:")
    if backend == "ollama":
        url = _ollama_url()
        req = urllib.request.Request(
            f"{url}/api/generate", method="POST",
            headers={"Content-Type": "application/json"},
            data=json.dumps({"model": _llm_model(), "prompt": prompt,
                             "stream": False, "think": False}).encode())
        with urllib.request.urlopen(req, timeout=120) as r:
            return json.load(r).get("response", "").strip() or _extractive_answer(question, contexts)
    if backend == "openai":
        # any OpenAI-compatible /chat/completions endpoint
        base = _llm_base_url() or "https://api.openai.com/v1"
        headers = {"Content-Type": "application/json"}
        key = _llm_api_key()
        if key:
            headers["Authorization"] = f"Bearer {key}"
        req = urllib.request.Request(
            f"{base}/chat/completions", method="POST", headers=headers,
            data=json.dumps({"model": _llm_model(),
                             "messages": [{"role": "user", "content": prompt}],
                             "stream": False, "temperature": 0.2}).encode())
        with urllib.request.urlopen(req, timeout=120) as r:
            data = json.load(r)
        text = (data["choices"][0]["message"]["content"] or "").strip()
        return text or _extractive_answer(question, contexts)
    # claude via ANTHROPIC_API_KEY (or vault secret in v0.3+)
    key = os.environ.get("ANTHROPIC_API_KEY", "")
    if not key:
        return _extractive_answer(question, contexts)
    req = urllib.request.Request(
        "https://api.anthropic.com/v1/messages", method="POST",
        headers={"content-type": "application/json", "x-api-key": key,
                 "anthropic-version": "2023-06-01"},
        data=json.dumps({"model": os.environ.get("GRIMOIRE_LLM_MODEL", "claude-sonnet-5"),
                         "max_tokens": 1024,
                         "messages": [{"role": "user", "content": prompt}]}).encode())
    with urllib.request.urlopen(req, timeout=120) as r:
        data = json.load(r)
    return "".join(b.get("text", "") for b in data.get("content", [])).strip()


def _stem(path: str) -> str:
    return path.rsplit("/", 1)[-1][:-3]


# ---- transcription (audio memos) --------------------------------------------

def transcribe(audio: bytes, filename: str = "memo.webm") -> str:
    """Transcribe an audio memo. Uses a local whisper HTTP service when configured
    (GRIMOIRE_WHISPER_URL, OpenAI-compatible /v1/audio/transcriptions), else returns a
    placeholder so the memo is still saved with its audio attachment. Tests
    monkeypatch this."""
    url = _cfg("whisper_url").rstrip("/")
    if not url:
        return "[audio memo — transcription unavailable; set GRIMOIRE_WHISPER_URL]"
    try:
        boundary = "----grimoire" + os.urandom(8).hex()
        parts = [
            f'--{boundary}\r\nContent-Disposition: form-data; name="model"\r\n\r\n'
            f'{os.environ.get("GRIMOIRE_WHISPER_MODEL", "whisper-1")}\r\n',
            f'--{boundary}\r\nContent-Disposition: form-data; name="file"; '
            f'filename="{filename}"\r\nContent-Type: application/octet-stream\r\n\r\n',
        ]
        body = parts[0].encode() + parts[1].encode() + audio + f"\r\n--{boundary}--\r\n".encode()
        req = urllib.request.Request(
            f"{url}/v1/audio/transcriptions", method="POST", data=body,
            headers={"Content-Type": f"multipart/form-data; boundary={boundary}"})
        with urllib.request.urlopen(req, timeout=180) as r:
            return json.load(r).get("text", "").strip() or "[empty transcription]"
    except Exception as e:  # noqa: BLE001
        return f"[transcription failed: {e}]"
