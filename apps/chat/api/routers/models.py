"""Model discovery endpoint."""

import re
from typing import Optional

from fastapi import APIRouter, Query

from config import settings
from models.schemas import ModelInfo, ModelListResponse
from services import gateway

router = APIRouter(prefix="/api", tags=["models"])

# ── Capability inference by model ID pattern ─────────────

_IMAGE_PATTERNS = [
    re.compile(r"dall-e", re.I),
    re.compile(r"gpt-image", re.I),
    re.compile(r"chatgpt-image", re.I),
    re.compile(r"flux", re.I),
    re.compile(r"stable-diffusion|sd[-_]?xl|sdxl", re.I),
    re.compile(r"midjourney", re.I),
    re.compile(r"imagen", re.I),
]

_VIDEO_PATTERNS = [
    re.compile(r"sora", re.I),
    re.compile(r"hunyuan[-_]?video", re.I),
    re.compile(r"runway", re.I),
    re.compile(r"kling", re.I),
    re.compile(r"seedance", re.I),
]

_AUDIO_PATTERNS = [
    re.compile(r"whisper", re.I),
    re.compile(r"tts", re.I),
]

_EMBEDDING_PATTERNS = [
    re.compile(r"embedding", re.I),
]


def _infer_capabilities(model_id: str) -> list[str]:
    caps: list[str] = []
    for p in _IMAGE_PATTERNS:
        if p.search(model_id):
            caps.append("image")
            break
    for p in _VIDEO_PATTERNS:
        if p.search(model_id):
            caps.append("video")
            break
    for p in _AUDIO_PATTERNS:
        if p.search(model_id):
            caps.append("audio")
            break
    for p in _EMBEDDING_PATTERNS:
        if p.search(model_id):
            caps.append("embedding")
            break
    # Default: if no special capability matched, it's a text/chat model
    if not caps:
        caps.append("text")
    return caps


def _parse_allowlist(raw: str) -> set[str]:
    """Parse a comma-separated string into a set of lowercase model IDs."""
    if not raw.strip():
        return set()
    return {s.strip().lower() for s in raw.split(",") if s.strip()}


def _get_allowlist(capability: str | None) -> set[str]:
    """Return the effective allowlist for a given capability.

    Priority: per-capability allowlist > global allowlist > empty (show all).
    """
    per_cap = {
        "text": settings.text_models_allowlist,
        "image": settings.image_models_allowlist,
        "audio": settings.audio_models_allowlist,
        "video": settings.video_models_allowlist,
    }
    if capability and per_cap.get(capability, "").strip():
        return _parse_allowlist(per_cap[capability])
    if settings.models_allowlist.strip():
        return _parse_allowlist(settings.models_allowlist)
    return set()


@router.get("/models", response_model=ModelListResponse,
             summary="List available models",
             description="Proxies GET /v1/models from the configured OpenAI-compatible backend. "
                         "Use ?capability=image to filter by capability.")
async def list_models(capability: Optional[str] = Query(None)):
    """Fetch available models from the gateway (vLLM, AIBrix, OpenAI, etc.).

    Optional query param ``capability`` filters results (text, image, video, audio).
    Models can be filtered via env vars:
    - ``MODELS_ALLOWLIST``: global allowlist (comma-separated model IDs)
    - ``TEXT_MODELS_ALLOWLIST``, ``IMAGE_MODELS_ALLOWLIST``, etc.: per-capability
    """
    raw_models = await gateway.list_models()
    allowlist = _get_allowlist(capability)

    models = []
    for m in raw_models:
        model_id = m.get("id", "")
        caps = _infer_capabilities(model_id)
        if capability and capability not in caps:
            continue
        if allowlist and model_id.lower() not in allowlist:
            continue
        models.append(
            ModelInfo(
                id=model_id,
                name=model_id,
                capabilities=caps,
                owned_by=m.get("owned_by"),
            )
        )

    return ModelListResponse(models=models)
