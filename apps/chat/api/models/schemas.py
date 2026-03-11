"""Pydantic models for request/response schemas."""

from __future__ import annotations

import uuid
from datetime import datetime, timezone
from typing import Any, Literal

from pydantic import BaseModel, Field


# ── Messages ───────────────────────────────────────────────


class MessageContent(BaseModel):
    """A single content block inside a message (text or image_url)."""

    type: str  # "text" or "image_url"
    text: str | None = None
    image_url: dict[str, str] | None = None

class ChatAttachment(BaseModel):
    """Serializable attachment metadata for images or files in chat."""

    id: str | None = None
    name: str
    type: str
    kind: Literal["image", "file"]
    preview_url: str | None = None


class Message(BaseModel):
    """Stored message in a conversation."""

    id: str = Field(default_factory=lambda: str(uuid.uuid4()))
    role: str  # "user", "assistant", "system"
    content: str | list[MessageContent]
    parent_id: str | None = None
    model: str | None = None
    attachments: list[ChatAttachment] = Field(default_factory=list)
    created_at: str = Field(
        default_factory=lambda: datetime.now(timezone.utc).isoformat()
    )


# ── Conversations ──────────────────────────────────────────


class Conversation(BaseModel):
    """A conversation with its messages."""

    id: str = Field(default_factory=lambda: str(uuid.uuid4()))
    title: str = "New Chat"
    messages: list[Message] = Field(default_factory=list)
    model: str | None = None
    user_id: str = ""
    project_id: str | None = None
    created_at: str = Field(
        default_factory=lambda: datetime.now(timezone.utc).isoformat()
    )
    updated_at: str = Field(
        default_factory=lambda: datetime.now(timezone.utc).isoformat()
    )


class ConversationSummary(BaseModel):
    """Lightweight view returned when listing conversations."""

    id: str
    title: str
    model: str | None = None
    message_count: int
    created_at: str
    updated_at: str


# ── Chat Completion Request ────────────────────────────────


class CompletionRequest(BaseModel):
    """Frontend sends only the new message; server manages history."""

    message: str | list[MessageContent]
    model: str
    attachments: list[ChatAttachment] = Field(default_factory=list)
    temperature: float = Field(0.7, ge=0.0, le=2.0)
    max_tokens: int = Field(2048, ge=1, le=32768)
    stream: bool = True
    system_prompt: str | None = None


# ── Chat Completion Response (non-streaming) ───────────────


class CompletionResponse(BaseModel):
    id: str
    conversation_id: str
    model: str
    message: Message
    usage: dict[str, int] | None = None


# ── Image Generation ───────────────────────────────────────


class ImageGenerateRequest(BaseModel):
    prompt: str = Field(..., max_length=4000)
    model: str = "dall-e-3"
    size: str = "1024x1024"
    n: int = Field(1, ge=1, le=4)
    quality: str | None = None
    style: str | None = None
    response_format: str | None = None


class ImageData(BaseModel):
    b64_json: str | None = None
    url: str | None = None
    revised_prompt: str | None = None


class ImageGenerateResponse(BaseModel):
    created: int
    data: list[ImageData]


# ── Audio ─────────────────────────────────────────────────


class AudioTranscribeResponse(BaseModel):
    text: str


class AudioSpeechRequest(BaseModel):
    input: str = Field(..., max_length=10000)
    model: str = "tts-1"
    voice: str = "alloy"
    response_format: str = "mp3"
    speed: float = 1.0


# ── Video Generation ───────────────────────────────────────


class VideoGenerateRequest(BaseModel):
    prompt: str = Field(..., max_length=4000)
    model: str = "sora-2"
    size: str = "1280x720"
    seconds: int = Field(4, ge=1, le=60)


class VideoJobResponse(BaseModel):
    id: str
    status: str
    prompt: str | None = None
    model: str | None = None
    progress: float | None = None
    error: str | None = None
    generations: list[dict[str, Any]] | None = None


# ── Models ─────────────────────────────────────────────────


class ModelInfo(BaseModel):
    id: str
    name: str | None = None
    capabilities: list[str] = Field(default_factory=lambda: ["text"])
    owned_by: str | None = None


class ModelListResponse(BaseModel):
    models: list[ModelInfo]


# ── Projects ──────────────────────────────────────────────


class Project(BaseModel):
    """A project with instructions and metadata."""

    id: str = Field(default_factory=lambda: str(uuid.uuid4()))
    name: str
    description: str = ""
    instructions: str = ""
    user_id: str = ""
    created_at: str = Field(
        default_factory=lambda: datetime.now(timezone.utc).isoformat()
    )
    updated_at: str = Field(
        default_factory=lambda: datetime.now(timezone.utc).isoformat()
    )


class ProjectSummary(BaseModel):
    """Lightweight view returned when listing projects."""

    id: str
    name: str
    description: str
    updated_at: str


class CreateProjectRequest(BaseModel):
    name: str
    description: str = ""


class UpdateProjectRequest(BaseModel):
    name: str | None = None
    description: str | None = None
    instructions: str | None = None


# ── Auth ──────────────────────────────────────────────────


class User(BaseModel):
    id: str = Field(default_factory=lambda: str(uuid.uuid4()))
    name: str
    created_at: str = Field(
        default_factory=lambda: datetime.now(timezone.utc).isoformat()
    )


class LoginRequest(BaseModel):
    name: str


class LoginResponse(BaseModel):
    user: User
    token: str


# ── Health ─────────────────────────────────────────────────


class HealthResponse(BaseModel):
    status: str
    version: str
    gateway_reachable: bool


# ── Errors ─────────────────────────────────────────────────


class ErrorDetail(BaseModel):
    code: str
    message: str
    status: int


class ErrorResponse(BaseModel):
    error: ErrorDetail
