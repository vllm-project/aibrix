"""vLLM-Omni provider implementation.

Adapts vLLM-Omni APIs to the standard provider interfaces.  Key differences
from the OpenAI provider:

* **Image generation / editing** use ``/v1/chat/completions`` (not
  ``/v1/images/generations``).  The image is returned as a base64 data-URI
  inside ``choices[0].message.content[0].image_url.url``.
* **TTS** uses the same ``/v1/audio/speech`` endpoint but adds a ``language``
  field and uses vLLM-Omni voice names (vivian, ryan, …).
* **ASR** is OpenAI-compatible — the implementation delegates to the OpenAI
  provider.
* **Video** generation via ``/v1/videos`` is *synchronous* (returns base64
  MP4 directly) rather than the async job+poll pattern.
* **Chat** adds ``modalities: ["text"]`` to every request.

See ``apps/chat/MODELS_DEPLOYMENT_GUIDE.md`` for the full API reference.
"""

from __future__ import annotations

import base64
import logging
import time
import uuid
from collections import OrderedDict
from collections.abc import AsyncIterator
from typing import Any

import httpx

from services.providers.base import (
    AudioProvider,
    ChatProvider,
    ImageProvider,
    VideoProvider,
)

logger = logging.getLogger(__name__)


def _headers(api_key: str) -> dict[str, str]:
    h: dict[str, str] = {"Content-Type": "application/json"}
    if api_key:
        h["Authorization"] = f"Bearer {api_key}"
    return h


def _auth_headers(api_key: str) -> dict[str, str]:
    """Headers without Content-Type (for multipart requests)."""
    h: dict[str, str] = {}
    if api_key:
        h["Authorization"] = f"Bearer {api_key}"
    return h


# ── Chat ─────────────────────────────────────────────────


class VLLMOmniChatProvider(ChatProvider):
    """Chat provider for Qwen2.5-Omni-7B served via vLLM-Omni.

    Adds ``modalities: ["text"]`` to every request so vLLM-Omni returns
    text-only output (no audio).
    """

    def __init__(self, base_url: str, api_key: str = "") -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self._client: httpx.AsyncClient | None = None

    def _make_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(
            timeout=httpx.Timeout(connect=10.0, read=120.0, write=10.0, pool=5.0),
            limits=httpx.Limits(max_connections=100, max_keepalive_connections=20),
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
        payload: dict[str, Any] = {
            "model": model,
            "messages": messages,
            "temperature": temperature,
            "max_tokens": max_tokens,
            "stream": False,
            "modalities": ["text"],
        }
        resp = await self.client.post(
            f"{self.base_url}/v1/chat/completions",
            json=payload,
            headers=_headers(self.api_key),
        )
        if resp.status_code != 200:
            logger.error(
                "vLLM-Omni chat failed: %s %s — %s",
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

        payload: dict[str, Any] = {
            "model": model,
            "messages": messages,
            "temperature": temperature,
            "max_tokens": max_tokens,
            "stream": True,
            "modalities": ["text"],
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
                    "vLLM-Omni chat stream failed: %s %s — %s",
                    resp.status_code, resp.reason_phrase, body.decode(),
                )
            resp.raise_for_status()
            async for event in parse_openai_sse(resp.aiter_lines()):
                yield event


# ── Image ────────────────────────────────────────────────


def _parse_size(size: str) -> tuple[int, int]:
    """Parse "WxH" into (width, height), default 1024x1024."""
    try:
        w, h = size.lower().split("x")
        return int(w), int(h)
    except (ValueError, AttributeError):
        return 1024, 1024


def _extract_b64_images(resp_json: dict) -> list[dict]:
    """Extract base64 image data from a vLLM-Omni /v1/chat/completions response.

    vLLM-Omni returns images as:
      choices[0].message.content = [
        {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
      ]
    """
    images: list[dict] = []
    for choice in resp_json.get("choices", []):
        content = choice.get("message", {}).get("content", [])
        if isinstance(content, str):
            continue
        for block in content:
            if block.get("type") != "image_url":
                continue
            data_uri = block.get("image_url", {}).get("url", "")
            if "base64," in data_uri:
                b64_str = data_uri.split("base64,", 1)[1]
                images.append({"b64_json": b64_str, "url": data_uri})
            elif data_uri:
                images.append({"url": data_uri})
    return images


class VLLMOmniImageProvider(ImageProvider):
    """Image generation via Qwen-Image / Z-Image-Turbo served by vLLM-Omni.

    Uses ``/v1/chat/completions`` with generation parameters passed in the
    request body (height, width, num_inference_steps, true_cfg_scale, seed).
    """

    def __init__(
        self, base_url: str, api_key: str = "",
        *, image_edit_url: str = "", image_edit_key: str = "",
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.image_edit_url = (
            image_edit_url.rstrip("/") if image_edit_url else self.base_url
        )
        self.image_edit_key = image_edit_key or self.api_key
        self._client: httpx.AsyncClient | None = None

    def _make_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(
            timeout=httpx.Timeout(connect=10.0, read=300.0, write=10.0, pool=5.0),
            limits=httpx.Limits(max_connections=100, max_keepalive_connections=20),
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
        width, height = _parse_size(size)
        payload: dict[str, Any] = {
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "height": height,
            "width": width,
            "num_inference_steps": kwargs.get("num_inference_steps", 50),
            "true_cfg_scale": kwargs.get("true_cfg_scale", 4.0),
            "guidance_scale": kwargs.get("guidance_scale", 1.0),
        }
        if "seed" in kwargs:
            payload["seed"] = kwargs["seed"]
        if "negative_prompt" in kwargs:
            payload["negative_prompt"] = kwargs["negative_prompt"]
        if n > 1:
            payload["num_outputs_per_prompt"] = n

        resp = await self.client.post(
            f"{self.base_url}/v1/chat/completions",
            json=payload,
            headers=_headers(self.api_key),
        )
        if resp.status_code != 200:
            logger.error(
                "vLLM-Omni image gen failed: %s — %s",
                resp.status_code, resp.text[:500],
            )
        resp.raise_for_status()

        data = resp.json()
        images = _extract_b64_images(data)
        return {"created": int(time.time()), "data": images}

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
        """Edit an image via vLLM-Omni (Qwen-Image-Edit).

        Encodes the input image as a base64 data-URI and sends it inside
        the message content array alongside the text prompt.
        """
        width, height = _parse_size(size)
        img_b64 = base64.b64encode(image).decode()

        # Determine MIME type from content_type or filename
        mime = content_type or "image/png"
        data_uri = f"data:{mime};base64,{img_b64}"

        payload: dict[str, Any] = {
            "model": model,
            "messages": [{
                "role": "user",
                "content": [
                    {"type": "text", "text": prompt},
                    {"type": "image_url", "image_url": {"url": data_uri}},
                ],
            }],
            "height": height,
            "width": width,
            "num_inference_steps": kwargs.get("num_inference_steps", 50),
            "true_cfg_scale": kwargs.get("true_cfg_scale", 4.0),
            "guidance_scale": kwargs.get("guidance_scale", 1.0),
        }
        if "seed" in kwargs:
            payload["seed"] = kwargs["seed"]

        resp = await self.client.post(
            f"{self.image_edit_url}/v1/chat/completions",
            json=payload,
            headers=_headers(self.image_edit_key),
        )
        if resp.status_code != 200:
            logger.error(
                "vLLM-Omni image edit failed: %s — %s",
                resp.status_code, resp.text[:500],
            )
        resp.raise_for_status()

        data = resp.json()
        images = _extract_b64_images(data)
        return {"created": int(time.time()), "data": images}


# ── Audio ────────────────────────────────────────────────


class VLLMOmniAudioProvider(AudioProvider):
    """Audio provider for vLLM-Omni (Qwen3-ASR + Qwen3-TTS).

    * **ASR** (``transcribe``) is OpenAI-compatible — same endpoint and format.
    * **TTS** (``speech``) adds the ``language`` field required by Qwen3-TTS.
    """

    def __init__(
        self, base_url: str, api_key: str = "",
        *, asr_url: str = "", asr_key: str = "",
        tts_url: str = "", tts_key: str = "",
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.asr_url = asr_url.rstrip("/") if asr_url else self.base_url
        self.asr_key = asr_key or self.api_key
        self.tts_url = tts_url.rstrip("/") if tts_url else self.base_url
        self.tts_key = tts_key or self.api_key
        self._client: httpx.AsyncClient | None = None

    def _make_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(
            timeout=httpx.Timeout(connect=10.0, read=120.0, write=10.0, pool=5.0),
            limits=httpx.Limits(max_connections=100, max_keepalive_connections=20),
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
        # vLLM ASR is OpenAI-compatible
        files = {"file": (filename, file_bytes)}
        data: dict[str, str] = {"model": model}
        if language:
            data["language"] = language

        resp = await self.client.post(
            f"{self.asr_url}/v1/audio/transcriptions",
            files=files,
            data=data,
            headers=_auth_headers(self.asr_key),
        )
        resp.raise_for_status()
        return resp.json()

    async def speech(
        self,
        text: str,
        model: str = "tts-1",
        voice: str = "vivian",
        response_format: str = "wav",
        speed: float = 1.0,
    ) -> bytes:
        """Generate speech via Qwen3-TTS.

        Adds ``language: "Auto"`` so the model auto-detects the input language.
        """
        payload: dict[str, Any] = {
            "input": text,
            "model": model,
            "voice": voice,
            "response_format": response_format,
            "speed": speed,
            "language": "Auto",
        }
        resp = await self.client.post(
            f"{self.tts_url}/v1/audio/speech",
            json=payload,
            headers=_headers(self.tts_key),
        )
        if resp.status_code != 200:
            logger.error(
                "vLLM-Omni TTS failed: %s — %s",
                resp.status_code, resp.text[:500],
            )
        resp.raise_for_status()
        return resp.content


# ── Video ────────────────────────────────────────────────


class _BoundedCache:
    """LRU-style bounded cache backed by OrderedDict."""

    def __init__(self, max_items: int = 20):
        self._cache: OrderedDict[str, dict] = OrderedDict()
        self._max = max_items

    def set(self, key: str, value: dict) -> None:
        if key in self._cache:
            self._cache.move_to_end(key)
        self._cache[key] = value
        while len(self._cache) > self._max:
            self._cache.popitem(last=False)

    def get(self, key: str) -> dict | None:
        if key in self._cache:
            self._cache.move_to_end(key)
            return self._cache[key]
        return None

    def pop(self, key: str, default=None):
        return self._cache.pop(key, default)


class VLLMOmniVideoProvider(VideoProvider):
    """Video generation via Wan2.2 served by vLLM-Omni.

    The vLLM-Omni ``/v1/videos`` endpoint is **synchronous** — it blocks
    until the video is ready and returns base64 MP4 directly.  We wrap the
    response in the standard job format with ``status: "completed"``.
    """

    def __init__(self, base_url: str, api_key: str = "") -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self._client: httpx.AsyncClient | None = None
        # Cache completed results for get_status() calls (bounded to avoid OOM)
        self._results = _BoundedCache(max_items=20)

    def _make_client(self) -> httpx.AsyncClient:
        return httpx.AsyncClient(
            timeout=httpx.Timeout(connect=10.0, read=600.0, write=10.0, pool=5.0),
            limits=httpx.Limits(max_connections=100, max_keepalive_connections=20),
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
        size: str = "832x480",
        seconds: int = 4,
        **kwargs: Any,
    ) -> dict:
        width, height = _parse_size(size)
        fps = kwargs.get("fps", 16)
        num_frames = fps * seconds + 1

        form_data: dict[str, str] = {
            "prompt": prompt,
            "width": str(width),
            "height": str(height),
            "num_frames": str(num_frames),
            "fps": str(fps),
            "num_inference_steps": str(kwargs.get("num_inference_steps", 40)),
            "guidance_scale": str(kwargs.get("guidance_scale", 4.0)),
            "flow_shift": str(kwargs.get("flow_shift", 5.0)),
        }
        if "negative_prompt" in kwargs:
            form_data["negative_prompt"] = kwargs["negative_prompt"]
        if "seed" in kwargs:
            form_data["seed"] = str(kwargs["seed"])
        if "guidance_scale_2" in kwargs:
            form_data["guidance_scale_2"] = str(kwargs["guidance_scale_2"])
        if "boundary_ratio" in kwargs:
            form_data["boundary_ratio"] = str(kwargs["boundary_ratio"])

        job_id = str(uuid.uuid4())

        try:
            resp = await self.client.post(
                f"{self.base_url}/v1/videos",
                data=form_data,
                headers=_auth_headers(self.api_key),
            )
            if resp.status_code != 200:
                logger.error(
                    "vLLM-Omni video gen failed: %s — %s",
                    resp.status_code, resp.text[:500],
                )
            resp.raise_for_status()

            resp_data = resp.json()
            generations = resp_data.get("data", [])

            result = {
                "id": job_id,
                "status": "completed",
                "prompt": prompt,
                "model": model,
                "generations": generations,
            }
        except httpx.HTTPError as e:
            logger.exception("vLLM-Omni video generation error")
            result = {
                "id": job_id,
                "status": "failed",
                "prompt": prompt,
                "model": model,
                "error": str(e),
            }

        self._results.set(job_id, result)
        return result

    async def get_status(self, job_id: str) -> dict:
        """Return cached result — vLLM-Omni video gen is synchronous."""
        result = self._results.get(job_id)
        if result is not None:
            return result
        return {
            "id": job_id,
            "status": "failed",
            "error": "Job not found",
        }

    async def get_content(self, job_id: str) -> bytes:
        """Return video bytes from cached generation result."""
        result = self._results.get(job_id)
        if not result or result.get("status") != "completed":
            raise ValueError(f"No completed video for job {job_id}")
        generations = result.get("generations", [])
        if not generations:
            raise ValueError(f"No video data for job {job_id}")
        # vLLM-Omni returns base64-encoded MP4 in generations[0]["video"]
        b64_data = generations[0].get("video", "")
        if not b64_data:
            raise ValueError(f"No video bytes for job {job_id}")
        return base64.b64decode(b64_data)
