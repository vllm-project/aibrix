# Copyright 2026 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
# 	http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Multimodal content parts for chat and Responses media inputs.

``video_url`` and ``audio_url`` are the URL-based parts vLLM and SGLang
accept; ``input_audio`` is OpenAI's inline (base64) audio part; ``file`` and
``refusal`` round out the OpenAI part union.  The OpenAI ``text`` /
``image_url`` parts live with the message models in ``openai.py``.

Media URLs are plain strings rather than pydantic ``AnyUrl``: the proxy also
accepts tos://, s3://, gs://, hf://, file:// and ``data:`` URIs, and
``AnyUrl`` would reject or silently normalise them.  URL content is
rewritten to a local ``file://`` path by the proxy before forwarding when
the engine can read local files (see ``proxy/runtime/engine_spec.py``).

Reference: vLLM tag ``v0.33.0rc1``, SGLang tag ``v0.5.22``,
openai-python tag ``v3.41.0``.
"""

from __future__ import annotations

from typing import Annotated, Any, Final, Literal, Optional, get_args, get_origin

from pydantic import BaseModel, ConfigDict, Field

# Schema annotation keys shared by media declarations and inspection.
MEDIA_URL: Final = "media_url"
MEDIA_TYPE: Final = "media_type"
OUTPUT_MEDIA: Final = "output_media"


class VideoUrl(BaseModel):
    model_config = ConfigDict(extra="allow")

    url: str = Field(..., description="URL of the video.")
    detail: Optional[str] = Field(None, description="Detail level of the video.")


class AudioUrl(BaseModel):
    model_config = ConfigDict(extra="allow")

    url: str = Field(..., description="URL of the audio file.")
    detail: Optional[str] = Field(None, description="Detail level of the audio.")


class InputAudio(BaseModel):
    data: str = Field(..., description="Base64-encoded audio data.")
    format: str = Field(..., description="Audio format (e.g. wav, mp3, flac).")


class FileContent(BaseModel):
    model_config = ConfigDict(extra="allow")

    file_data: Optional[str] = Field(None, description="Base64-encoded file data.")
    file_id: Optional[str] = Field(None, description="ID of an uploaded file.")
    filename: Optional[str] = None


class ChatCompletionRequestMessageContentPartVideo(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    type: Literal["video_url"] = Field(..., description="The type of the content part.")
    video_url: VideoUrl = Field(json_schema_extra={MEDIA_URL: "url"})
    uuid: Optional[str] = Field(
        None, description="Optional unique identifier for the content part."
    )


class ChatCompletionRequestMessageContentPartAudio(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    type: Literal["audio_url"] = Field(..., description="The type of the content part.")
    audio_url: AudioUrl = Field(json_schema_extra={MEDIA_URL: "url"})
    uuid: Optional[str] = Field(
        None, description="Optional unique identifier for the content part."
    )


class ChatCompletionRequestMessageContentPartInputAudio(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    type: Literal["input_audio"] = Field(
        ..., description="The type of the content part."
    )
    input_audio: InputAudio
    uuid: Optional[str] = Field(
        None, description="Optional unique identifier for the content part."
    )


class ChatCompletionRequestMessageContentPartFile(BaseModel):
    """OpenAI ``file`` content part (PDF and other documents)."""

    model_config = ConfigDict(use_enum_values=True, extra="allow")

    type: Literal["file"] = Field(..., description="The type of the content part.")
    file: FileContent


class ChatCompletionRequestMessageContentPartRefusal(BaseModel):
    """OpenAI ``refusal`` content part (assistant messages only)."""

    model_config = ConfigDict(use_enum_values=True)

    type: Literal["refusal"] = Field(..., description="The type of the content part.")
    refusal: str


def media_url_fields(content_schema: Any) -> dict[str, tuple[str, str]]:
    """Derive URL metadata from actual request schemas, including nested parts."""
    seen: set[int] = set()

    def walk(schema: Any) -> dict[str, tuple[str, str]]:
        if id(schema) in seen:
            return {}
        seen.add(id(schema))
        origin = get_origin(schema)
        if origin is Annotated:
            return walk(get_args(schema)[0])
        if origin is not None:
            fields: dict[str, tuple[str, str]] = {}
            for member in get_args(schema):
                fields.update(walk(member))
            return fields
        if not isinstance(schema, type) or not issubclass(schema, BaseModel):
            return {}
        fields = {}
        discriminator = schema.model_fields.get("type")
        for name, field in schema.model_fields.items():
            metadata = field.json_schema_extra
            if discriminator is not None and isinstance(metadata, dict):
                nested_url = metadata.get(MEDIA_URL)
                if isinstance(nested_url, str):
                    for tag in get_args(discriminator.annotation):
                        if isinstance(tag, str):
                            fields[tag] = (field.alias or name, nested_url)
            fields.update(walk(field.annotation))
        return fields

    return walk(content_schema)
