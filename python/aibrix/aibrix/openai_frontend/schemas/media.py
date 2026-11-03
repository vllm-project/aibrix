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

"""Multimodal chat content parts beyond OpenAI's ``text`` / ``image_url``.

``video_url`` and ``audio_url`` are the URL-based parts vLLM and SGLang
accept; ``input_audio`` is OpenAI's inline (base64) audio part; ``file`` and
``refusal`` round out the OpenAI part union.  The OpenAI ``text`` /
``image_url`` parts live with the message models in ``openai.py``.

Media URLs are plain strings rather than pydantic ``AnyUrl``: the proxy also
accepts tos://, s3://, gs://, hf://, file:// and ``data:`` URIs, and
``AnyUrl`` would reject or silently normalise them.  URL content is
rewritten to a local ``file://`` path by the proxy before forwarding when
the engine can read local files (see ``proxy/engine_spec.py``).

Reference: vLLM tag ``v0.33.0rc1``, SGLang tag ``v0.5.22``,
openai-python tag ``v3.41.0``.
"""

from __future__ import annotations

from typing import Literal, Optional

from pydantic import BaseModel, ConfigDict, Field


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
    video_url: VideoUrl
    uuid: Optional[str] = Field(
        None, description="Optional unique identifier for the content part."
    )


class ChatCompletionRequestMessageContentPartAudio(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    type: Literal["audio_url"] = Field(..., description="The type of the content part.")
    audio_url: AudioUrl
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
