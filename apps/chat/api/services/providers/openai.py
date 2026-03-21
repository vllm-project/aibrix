"""OpenAI-compatible provider implementation.

Works with OpenAI, vLLM, AIBrix gateway, and any other server that
speaks the OpenAI API (``/v1/chat/completions``, ``/v1/images/generations``,
``/v1/audio/*``, etc.).
"""

from __future__ import annotations

import io
import logging
import time
from collections.abc import AsyncIterator
from typing import Any

import httpx
from PIL import Image

logging.basicConfig(level=logging.INFO)
logger = logging.getLogger(__name__)

from services.providers.base import (
    AudioProvider,
    ChatProvider,
    ImageProvider,
    VideoProvider,
)


async def _log_request(request: httpx.Request) -> None:
    content_length = request.headers.get("content-length", "?")
    logger.warning(
        ">> %s %s (content-length=%s)",
        request.method, request.url, content_length,
    )


async def _log_response(response: httpx.Response) -> None:
    logger.info(
        "<< %s %s -- %s",
        response.request.method, response.request.url, response.status_code,
    )


def _headers(api_key: str) -> dict[str, str]:
    h: dict[str, str] = {"Content-Type": "application/json"}
    if api_key:
        h["Authorization"] = f"Bearer {api_key}"
    return h


_EVENT_HOOKS = {"request": [_log_request], "response": [_log_response]}


# ── Chat ─────────────────────────────────────────────────


class OpenAIChatProvider(ChatProvider):
    def __init__(self, base_url: str, api_key: str = "") -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self._client: httpx.AsyncClient | None = None

    def _make_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(
            timeout=httpx.Timeout(connect=10.0, read=120.0, write=10.0, pool=5.0),
            limits=httpx.Limits(max_connections=100, max_keepalive_connections=20),
            event_hooks={"request": [_log_request], "response": [_log_response]},
        )

    async def startup(self) -> None:
        self._client = self._make_client()

    async def shutdown(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    @property
    def client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = self._make_client()
        return self._client

    async def complete(
        self,
        messages: list[dict],
        model: str,
        temperature: float = 0.7,
        max_tokens: int = 2048,
        **kwargs: Any,
    ) -> dict:
        payload = {
            "model": model,
            "messages": messages,
            "temperature": temperature,
            "max_tokens": max_tokens,
            "stream": False,
        }
        resp = await self.client.post(
            f"{self.base_url}/v1/chat/completions",
            json=payload,
            headers=_headers(self.api_key),
        )
        if resp.status_code != 200:
            logger.error(
                "Chat completion failed: %s %s — %s",
                resp.status_code, resp.reason_phrase, resp.text,
            )
        resp.raise_for_status()
        return resp.json()

    async def complete_stream(
        self,
        messages: list[dict],
        model: str,
        temperature: float = 0.7,
        max_tokens: int = 2048,
        **kwargs: Any,
    ) -> AsyncIterator[str]:
        from services.providers.sse_utils import parse_openai_sse

        payload = {
            "model": model,
            "messages": messages,
            "temperature": temperature,
            "max_tokens": max_tokens,
            "stream": True,
        }
        async with self.client.stream(
            "POST",
            f"{self.base_url}/v1/chat/completions",
            json=payload,
            headers=_headers(self.api_key),
        ) as resp:
            if resp.status_code != 200:
                body = await resp.aread()
                logger.error(
                    "Chat stream failed: %s %s — %s",
                    resp.status_code, resp.reason_phrase, body.decode(),
                )
            resp.raise_for_status()
            async for event in parse_openai_sse(resp.aiter_lines()):
                yield event


# ── Image ────────────────────────────────────────────────


class OpenAIImageProvider(ImageProvider):
    def __init__(self, base_url: str, api_key: str = "") -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self._client: httpx.AsyncClient | None = None

    def _make_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(
            timeout=httpx.Timeout(connect=10.0, read=120.0, write=10.0, pool=5.0),
            limits=httpx.Limits(max_connections=100, max_keepalive_connections=20),
            event_hooks={"request": [_log_request], "response": [_log_response]},
        )

    async def startup(self) -> None:
        self._client = self._make_client()

    async def shutdown(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    @property
    def client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = self._make_client()
        return self._client

    async def generate(
        self,
        prompt: str,
        model: str,
        size: str = "1024x1024",
        n: int = 1,
        **kwargs: Any,
    ) -> dict:
        payload: dict[str, Any] = {
            "prompt": prompt,
            "model": model,
            "size": size,
            "n": n,
        }
        # Forward extra OpenAI params (quality, style, response_format, …)
        for key in ("quality", "style", "response_format"):
            if key in kwargs:
                payload[key] = kwargs[key]

        resp = await self.client.post(
            f"{self.base_url}/v1/images/generations",
            json=payload,
            headers=_headers(self.api_key),
        )
        resp.raise_for_status()
        return resp.json()

    async def edit(
        self,
        image: bytes,
        filename: str,
        content_type: str,
        prompt: str,
        model: str = "dall-e-2",
        size: str = "1024x1024",
        n: int = 1,
        **kwargs: Any,
    ) -> dict:
        headers: dict[str, str] = {}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"

        # dall-e-2 uses inpainting (transparent pixels only) so we need
        # RGBA conversion and a fully-transparent mask for opaque images.
        # GPT image models (gpt-image-*, chatgpt-image-*) understand the
        # image content directly and don't need mask-based inpainting.
        is_dalle2 = model.startswith("dall-e")
        mask_bytes: bytes | None = None

        if is_dalle2:
            img = Image.open(io.BytesIO(image))
            needs_mask = img.mode not in ("RGBA", "LA", "L")
            if needs_mask:
                img = img.convert("RGBA")
                buf = io.BytesIO()
                img.save(buf, format="PNG")
                image = buf.getvalue()
                filename = filename.rsplit(".", 1)[0] + ".png"
                content_type = "image/png"

                mask = Image.new("RGBA", img.size, (0, 0, 0, 0))
                mask_buf = io.BytesIO()
                mask.save(mask_buf, format="PNG")
                mask_bytes = mask_buf.getvalue()

        logger.info(
            "Image edit: model=%s file=%s size=%d bytes mask=%s",
            model, filename, len(image),
            "full" if mask_bytes else ("from-alpha" if is_dalle2 else "none"),
        )
        files: dict[str, tuple[str, bytes, str]] = {
            "image": (filename, image, content_type),
        }
        if mask_bytes:
            files["mask"] = ("mask.png", mask_bytes, "image/png")
        data: dict[str, str] = {
            "prompt": prompt,
            "model": model,
            "size": size,
            "n": str(n),
        }

        resp = await self.client.post(
            f"{self.base_url}/v1/images/edits",
            files=files,
            data=data,
            headers=headers,
        )
        if resp.status_code != 200:
            logger.error(
                "Image edit failed: %s %s — %s",
                resp.status_code, resp.reason_phrase, resp.text,
            )
        resp.raise_for_status()
        return resp.json()


# ── Audio ────────────────────────────────────────────────


class OpenAIAudioProvider(AudioProvider):
    def __init__(self, base_url: str, api_key: str = "") -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self._client: httpx.AsyncClient | None = None

    def _make_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(
            timeout=httpx.Timeout(connect=10.0, read=120.0, write=10.0, pool=5.0),
            limits=httpx.Limits(max_connections=100, max_keepalive_connections=20),
            event_hooks={"request": [_log_request], "response": [_log_response]},
        )

    async def startup(self) -> None:
        self._client = self._make_client()

    async def shutdown(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    @property
    def client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = self._make_client()
        return self._client

    async def transcribe(
        self,
        file_bytes: bytes,
        filename: str,
        model: str = "whisper-1",
        language: str | None = None,
    ) -> dict:
        headers: dict[str, str] = {}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"

        files = {"file": (filename, file_bytes)}
        data: dict[str, str] = {"model": model}
        if language:
            data["language"] = language

        resp = await self.client.post(
            f"{self.base_url}/v1/audio/transcriptions",
            files=files,
            data=data,
            headers=headers,
        )
        resp.raise_for_status()
        return resp.json()

    async def speech(
        self,
        text: str,
        model: str = "tts-1",
        voice: str = "alloy",
        response_format: str = "mp3",
        speed: float = 1.0,
        **kwargs: Any,
    ) -> bytes:
        payload: dict[str, Any] = {
            "input": text,
            "model": model,
            "voice": voice,
            "response_format": response_format,
            "speed": speed,
        }
        resp = await self.client.post(
            f"{self.base_url}/v1/audio/speech",
            json=payload,
            headers=_headers(self.api_key),
        )
        resp.raise_for_status()
        return resp.content


# ── Video ────────────────────────────────────────────────


class OpenAIVideoProvider(VideoProvider):
    """OpenAI Sora video generation.

    Sora API uses multipart/form-data for POST /v1/videos and returns an
    async job that must be polled via GET /v1/videos/{id}.  The finished
    video is downloaded from GET /v1/videos/{id}/content.
    """

    def __init__(self, base_url: str, api_key: str = "") -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self._client: httpx.AsyncClient | None = None

    def _make_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(
            timeout=httpx.Timeout(connect=10.0, read=120.0, write=10.0, pool=5.0),
            limits=httpx.Limits(max_connections=100, max_keepalive_connections=20),
            event_hooks={"request": [_log_request], "response": [_log_response]},
        )

    async def startup(self) -> None:
        self._client = self._make_client()

    async def shutdown(self) -> None:
        if self._client is not None:
            await self._client.aclose()
            self._client = None

    @property
    def client(self) -> httpx.AsyncClient:
        if self._client is None:
            self._client = self._make_client()
        return self._client

    def _auth(self) -> dict[str, str]:
        h: dict[str, str] = {}
        if self.api_key:
            h["Authorization"] = f"Bearer {self.api_key}"
        return h

    async def generate(
        self,
        prompt: str,
        model: str,
        size: str = "1280x720",
        seconds: int = 4,
        image: bytes | None = None,
        **kwargs: Any,
    ) -> dict:
        # Sora API accepts multipart/form-data.
        # httpx `data=` sends x-www-form-urlencoded; use `files=` to force multipart.
        fields: dict[str, str] = {
            "prompt": prompt,
            "model": model,
            "size": size,
            "seconds": str(seconds),
        }
        # Convert to files= tuples: each value becomes a (None, value) pair
        # which httpx sends as multipart/form-data fields (no filename).
        multipart = {k: (None, v) for k, v in fields.items()}
        resp = await self.client.post(
            f"{self.base_url}/v1/videos",
            files=multipart,
            headers=self._auth(),
        )
        if resp.status_code != 200:
            logger.error(
                "Video generate failed: %s %s — %s",
                resp.status_code, resp.reason_phrase, resp.text,
            )
        resp.raise_for_status()
        data = resp.json()

        # Normalise to our VideoJobResponse shape
        return {
            "id": data.get("id", ""),
            "status": data.get("status", "queued"),
            "prompt": prompt,
            "model": model,
            "progress": data.get("progress"),
        }

    async def get_status(self, job_id: str) -> dict:
        resp = await self.client.get(
            f"{self.base_url}/v1/videos/{job_id}",
            headers=self._auth(),
        )
        resp.raise_for_status()
        data = resp.json()

        result: dict[str, Any] = {
            "id": data.get("id", job_id),
            "status": data.get("status", "queued"),
            "progress": data.get("progress"),
        }
        if data.get("error"):
            result["error"] = str(data["error"])
        return result

    async def get_content(self, job_id: str) -> bytes:
        resp = await self.client.get(
            f"{self.base_url}/v1/videos/{job_id}/content",
            headers=self._auth(),
        )
        resp.raise_for_status()
        return resp.content
