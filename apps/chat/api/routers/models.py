"""Model discovery endpoint."""

import re
import time

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
    re.compile(r"realtime", re.I),
    re.compile(r"audio", re.I),
    re.compile(r"transcribe", re.I),
    re.compile(r"speech", re.I),
]

_EMBEDDING_PATTERNS = [
    re.compile(r"embedding", re.I),
]

# Models served by /v1/moderations or the legacy /v1/completions endpoint —
# not usable from /v1/chat/completions, so they must not appear as "text".
_OTHER_PATTERNS = [
    re.compile(r"moderation", re.I),
    re.compile(r"-instruct", re.I),
    re.compile(r"^(babbage|davinci|curie|ada)(-|$)", re.I),
    re.compile(r"^o1-pro", re.I),  # responses-only, not chat/completions
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
    for p in _OTHER_PATTERNS:
        if p.search(model_id):
            caps.append("other")
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


@router.get(
    "/models",
    response_model=ModelListResponse,
    summary="List available models",
    description="Proxies GET /v1/models from the configured OpenAI-compatible backend. "
    "Use ?capability=image to filter by capability.",
)
async def list_models(capability: str | None = Query(None)):
    """Fetch available models from the gateway (vLLM, AIBrix, OpenAI, etc.).

    Optional query param ``capability`` filters results (text, image, video, audio).
    Models can be filtered via env vars:
    - ``MODELS_ALLOWLIST``: global allowlist (comma-separated model IDs)
    - ``TEXT_MODELS_ALLOWLIST``, ``IMAGE_MODELS_ALLOWLIST``, etc.: per-capability
    """
    raw_models = await gateway.list_models()
    allowlist = _get_allowlist(capability)

    # Recency cutoff: hide models older than MODELS_MAX_AGE_DAYS, but only when the
    # provider reports a `created` timestamp (self-hosted models often don't).
    cutoff_ts = 0.0
    if settings.models_max_age_days > 0:
        cutoff_ts = time.time() - settings.models_max_age_days * 86400

    models = []
    for m in raw_models:
        model_id = m.get("id", "")
        caps = _infer_capabilities(model_id)
        if capability and capability not in caps:
            continue
        if allowlist and model_id.lower() not in allowlist:
            continue
        created = m.get("created")
        if not isinstance(created, int | float):
            created = None
        if cutoff_ts and created and created < cutoff_ts:
            continue
        models.append(
            ModelInfo(
                id=model_id,
                name=model_id,
                capabilities=caps,
                owned_by=m.get("owned_by"),
                created=int(created) if created else None,
            )
        )

    # Newest first; models without a timestamp keep their original order at the end.
    models.sort(key=lambda x: x.created or 0, reverse=True)

    return ModelListResponse(models=models)
