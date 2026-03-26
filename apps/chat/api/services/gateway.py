"""HTTP client for communicating with the AIBrix gateway.

Chat completion is delegated to the provider adapter layer.
Health-check and model listing remain here (simple, no adapter needed).
"""

from __future__ import annotations

import time
from collections.abc import AsyncIterator

import httpx

from config import settings
from services.providers import get_chat_provider

# ── Model list TTL cache ────────────────────────────────
_model_cache: list[dict] | None = None
_model_cache_ts: float = 0
_MODEL_CACHE_TTL = 60.0  # seconds


def _get_headers() -> dict[str, str]:
    headers = {"Content-Type": "application/json"}
    key = settings.get_chat_key()
    if key:
        headers["Authorization"] = f"Bearer {key}"
    return headers


async def check_health() -> bool:
    """Check if the gateway is reachable."""
    try:
        async with httpx.AsyncClient(timeout=5.0) as client:
            resp = await client.get(
                f"{settings.get_chat_url()}/v1/models",
                headers=_get_headers(),
            )
            return resp.status_code == 200
    except httpx.HTTPError:
        return False


async def list_models() -> list[dict]:
    """Fetch available models from the gateway (cached with 60s TTL)."""
    global _model_cache, _model_cache_ts

    now = time.monotonic()
    if _model_cache is not None and (now - _model_cache_ts) < _MODEL_CACHE_TTL:
        return _model_cache

    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            resp = await client.get(
                f"{settings.get_chat_url()}/v1/models",
                headers=_get_headers(),
            )
            resp.raise_for_status()
            data = resp.json()
            _model_cache = data.get("data", [])
            _model_cache_ts = now
            return _model_cache
    except httpx.HTTPError:
        # Graceful degradation: return stale cache if available
        if _model_cache is not None:
            return _model_cache
        return []


async def chat_completion(
    messages: list[dict],
    model: str,
    temperature: float = 0.7,
    max_tokens: int = 2048,
    **kwargs,
) -> dict:
    """Non-streaming chat completion via the configured provider."""
    provider = get_chat_provider()
    return await provider.complete(
        messages=messages,
        model=model,
        temperature=temperature,
        max_tokens=max_tokens,
        **kwargs,
    )


async def chat_completion_stream(
    messages: list[dict],
    model: str,
    temperature: float = 0.7,
    max_tokens: int = 2048,
) -> AsyncIterator[str]:
    """Stream chat completions via the configured provider."""
    provider = get_chat_provider()
    async for event in provider.complete_stream(
        messages=messages,
        model=model,
        temperature=temperature,
        max_tokens=max_tokens,
    ):
        yield event
