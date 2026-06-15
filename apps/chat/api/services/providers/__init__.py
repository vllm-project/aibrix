"""Provider registry — factory functions for each modality."""

from __future__ import annotations

from typing import Any

from config import settings
from services.providers.base import (
    AudioProvider,
    ChatProvider,
    ImageProvider,
    VideoProvider,
)
from services.providers.openai import (
    OpenAIAudioProvider,
    OpenAIChatProvider,
    OpenAIImageProvider,
    OpenAIVideoProvider,
)
from services.providers.vllm_omni import (
    VLLMOmniAudioProvider,
    VLLMOmniChatProvider,
    VLLMOmniImageProvider,
    VLLMOmniVideoProvider,
)

_REGISTRY: dict[str, dict[str, type]] = {
    "chat": {
        "openai": OpenAIChatProvider,
        "vllm_omni": VLLMOmniChatProvider,
    },
    "image": {
        "openai": OpenAIImageProvider,
        "vllm_omni": VLLMOmniImageProvider,
    },
    "audio": {
        "openai": OpenAIAudioProvider,
        "vllm_omni": VLLMOmniAudioProvider,
    },
    "video": {
        "openai": OpenAIVideoProvider,
        "vllm_omni": VLLMOmniVideoProvider,
    },
}

# Singleton cache — providers are reused across requests for connection pooling
_provider_cache: dict[str, Any] = {}


def get_chat_provider() -> ChatProvider:
    key = f"chat:{settings.chat_provider}"
    if key not in _provider_cache:
        name = settings.chat_provider
        cls = _REGISTRY["chat"][name]
        _provider_cache[key] = cls(
            base_url=settings.get_chat_url(),
            api_key=settings.get_chat_key(),
        )
    return _provider_cache[key]


def get_image_provider() -> ImageProvider:
    key = f"image:{settings.image_provider}"
    if key not in _provider_cache:
        name = settings.image_provider
        cls = _REGISTRY["image"][name]
        _provider_cache[key] = cls(
            base_url=settings.get_image_url(),
            api_key=settings.get_image_key(),
            image_edit_url=settings.get_image_edit_url(),
            image_edit_key=settings.get_image_edit_key(),
        )
    return _provider_cache[key]


def get_audio_provider() -> AudioProvider:
    key = f"audio:{settings.audio_provider}"
    if key not in _provider_cache:
        name = settings.audio_provider
        cls = _REGISTRY["audio"][name]
        _provider_cache[key] = cls(
            base_url=settings.get_asr_url(),
            api_key=settings.get_asr_key(),
            asr_url=settings.get_asr_url(),
            asr_key=settings.get_asr_key(),
            tts_url=settings.get_tts_url(),
            tts_key=settings.get_tts_key(),
        )
    return _provider_cache[key]


def get_video_provider() -> VideoProvider:
    key = f"video:{settings.video_provider}"
    if key not in _provider_cache:
        name = settings.video_provider
        cls = _REGISTRY["video"][name]
        _provider_cache[key] = cls(
            base_url=settings.get_video_url(),
            api_key=settings.get_video_key(),
        )
    return _provider_cache[key]
