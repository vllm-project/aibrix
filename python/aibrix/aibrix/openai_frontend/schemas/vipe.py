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

"""Pydantic schemas for ViPE pipeline requests and responses."""

from __future__ import annotations

from typing import Any, Dict, List
from urllib.parse import urlparse

from pydantic import BaseModel, Field, field_validator


class ViPEInput(BaseModel):
    """Input specification for a ViPE pipeline request."""

    video_url: str = Field(description="HTTP(S) URL of the source video")

    @field_validator("video_url")
    @classmethod
    def _validate_video_url(cls, v: str) -> str:
        parsed = urlparse(v)
        if parsed.scheme not in {"http", "https"}:
            raise ValueError("'video_url' scheme must be http or https")
        if not parsed.hostname:
            raise ValueError("'video_url' must include a hostname")
        return v


class ViPEParameters(BaseModel):
    """Parameters controlling pipeline behaviour."""

    pipeline: str = Field(default="default", description="Pipeline type to run")
    visualize: bool = Field(
        default=False, description="Enable visualization output (alias for save_viz)"
    )
    save_viz: bool = Field(default=False, description="Render MP4 visualization videos")
    save_artifacts: bool = Field(
        default=True, description="Save pipeline artifacts to output"
    )
    extra: Dict[str, Any] = Field(
        default_factory=dict,
        description="Additional pipeline-specific parameters",
    )


class ViPEOutputSpec(BaseModel):
    """Output destination specification."""

    subdir: str = Field(description="TOS object key prefix for uploaded outputs")

    @field_validator("subdir")
    @classmethod
    def _validate_subdir(cls, v: str) -> str:
        normalized = v.strip().strip("/")
        if not normalized:
            raise ValueError("'output.subdir' must be a non-empty object key prefix")
        if "://" in normalized:
            raise ValueError("'output.subdir' must be an object key prefix, not a URL")
        parts = normalized.split("/")
        if any(part in {"", ".", ".."} for part in parts):
            raise ValueError("'output.subdir' contains invalid path segments")
        return normalized


class ViPERequest(BaseModel):
    """Top-level ViPE pipeline request."""

    custom_id: str = Field(
        description="Client-assigned identifier for tracking this request",
    )
    input: ViPEInput = Field(description="Input data")
    parameters: ViPEParameters = Field(
        default_factory=ViPEParameters, description="Pipeline parameters"
    )
    output: ViPEOutputSpec = Field(description="Output destination")


class ViPEOutputResult(BaseModel):
    """ViPE pipeline output metadata."""

    subdir: str = Field(description="TOS object key prefix used for uploads")
    output_path: str = Field(description="Full TOS URI prefix for the output artifacts")
    artifacts: List[str] = Field(
        default_factory=list, description="Uploaded artifact keys"
    )


class ViPEResponse(BaseModel):
    """Top-level ViPE pipeline response."""

    output: ViPEOutputResult = Field(description="Pipeline output metadata")
