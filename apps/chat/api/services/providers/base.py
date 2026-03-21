"""Abstract base classes for provider adapters."""

from __future__ import annotations

from abc import ABC, abstractmethod
from collections.abc import AsyncIterator
from typing import Any


class ChatProvider(ABC):
    """Adapter for chat completion APIs."""

    async def startup(self) -> None:
        """Called once on application startup to create persistent resources."""

    async def shutdown(self) -> None:
        """Called once on application shutdown to release resources."""

    @abstractmethod
    async def complete(
        self,
        messages: list[dict],
        model: str,
        temperature: float = 0.7,
        max_tokens: int = 2048,
        **kwargs: Any,
    ) -> dict:
        """Non-streaming chat completion. Returns the raw response dict."""

    @abstractmethod
    async def complete_stream(
        self,
        messages: list[dict],
        model: str,
        temperature: float = 0.7,
        max_tokens: int = 2048,
        **kwargs: Any,
    ) -> AsyncIterator[str]:
        """Streaming chat completion. Yields JSON strings with
        ``{"event": "text_delta", "delta": "..."}`` or
        ``{"event": "done", ...}``."""


class ImageProvider(ABC):
    """Adapter for image generation APIs."""

    async def startup(self) -> None:
        """Called once on application startup to create persistent resources."""

    async def shutdown(self) -> None:
        """Called once on application shutdown to release resources."""

    @abstractmethod
    async def generate(
        self,
        prompt: str,
        model: str,
        size: str = "1024x1024",
        n: int = 1,
        **kwargs: Any,
    ) -> dict:
        """Generate images. Returns ``{"created": ..., "data": [...]}``."""

    @abstractmethod
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
        """Edit an image given a reference image and prompt.
        Returns ``{"created": ..., "data": [...]}``."""


class AudioProvider(ABC):
    """Adapter for audio transcription and speech APIs."""

    async def startup(self) -> None:
        """Called once on application startup to create persistent resources."""

    async def shutdown(self) -> None:
        """Called once on application shutdown to release resources."""

    @abstractmethod
    async def transcribe(
        self,
        file_bytes: bytes,
        filename: str,
        model: str = "whisper-1",
        language: str | None = None,
    ) -> dict:
        """Transcribe audio. Returns ``{"text": "..."}``."""

    @abstractmethod
    async def speech(
        self,
        text: str,
        model: str = "tts-1",
        voice: str = "alloy",
        response_format: str = "mp3",
        speed: float = 1.0,
        **kwargs: Any,
    ) -> bytes:
        """Text-to-speech. Returns raw audio bytes."""


class VideoProvider(ABC):
    """Adapter for video generation APIs."""

    async def startup(self) -> None:
        """Called once on application startup to create persistent resources."""

    async def shutdown(self) -> None:
        """Called once on application shutdown to release resources."""

    @abstractmethod
    async def generate(
        self,
        prompt: str,
        model: str,
        size: str = "1280x720",
        seconds: int = 4,
        image: bytes | None = None,
        **kwargs: Any,
    ) -> dict:
        """Submit a video generation job. Returns ``{"id": ..., "status": ...}``.

        Pass ``image`` bytes for Image-to-Video (I2V) generation."""

    @abstractmethod
    async def get_status(self, job_id: str) -> dict:
        """Poll job status. Returns ``{"id": ..., "status": ..., ...}``."""

    @abstractmethod
    async def get_content(self, job_id: str) -> bytes:
        """Download the finished video as raw bytes (e.g. MP4)."""
