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

"""Pydantic schemas for vLLM multimodal content part extensions.

Extends the OpenAI chat completion content parts with video, audio,
and input_audio types supported by vLLM but not part of the standard
OpenAI API.
"""

from __future__ import annotations

from enum import Enum
from typing import Optional

from pydantic import AnyUrl, BaseModel, ConfigDict, Field

# ---------------------------------------------------------------------------
# URL-based media types (video, audio)
# ---------------------------------------------------------------------------


class VideoUrl(BaseModel):
    url: AnyUrl = Field(..., description="URL of the video.")
    detail: Optional[str] = Field(
        None,
        description="Specifies the detail level of the video.",
    )


class AudioUrl(BaseModel):
    url: AnyUrl = Field(..., description="URL of the audio file.")
    detail: Optional[str] = Field(
        None,
        description="Specifies the detail level of the audio.",
    )


# ---------------------------------------------------------------------------
# Inline audio type (base64-encoded)
# ---------------------------------------------------------------------------


class InputAudio(BaseModel):
    data: str = Field(..., description="Base64-encoded audio data.")
    format: str = Field(..., description="Format of the audio data (e.g. wav, mp3).")


# ---------------------------------------------------------------------------
# Content part type enums
# ---------------------------------------------------------------------------


class VideoUrlType(Enum):
    video_url = "video_url"


class AudioUrlType(Enum):
    audio_url = "audio_url"


class InputAudioType(Enum):
    input_audio = "input_audio"


# ---------------------------------------------------------------------------
# Content part models
# ---------------------------------------------------------------------------


class ChatCompletionRequestMessageContentPartVideo(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    type: VideoUrlType = Field(..., description="The type of the content part.")
    video_url: VideoUrl
    uuid: Optional[str] = Field(
        None, description="Optional unique identifier for the content part."
    )


class ChatCompletionRequestMessageContentPartAudio(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    type: AudioUrlType = Field(..., description="The type of the content part.")
    audio_url: AudioUrl
    uuid: Optional[str] = Field(
        None, description="Optional unique identifier for the content part."
    )


class ChatCompletionRequestMessageContentPartInputAudio(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    type: InputAudioType = Field(..., description="The type of the content part.")
    input_audio: InputAudio
    uuid: Optional[str] = Field(
        None, description="Optional unique identifier for the content part."
    )
