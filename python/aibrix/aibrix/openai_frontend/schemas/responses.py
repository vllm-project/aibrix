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

"""Minimal Responses wire contracts, not a complete SDK schema mirror.

Model the common request/response envelope and URL media inputs. Tool calls,
output items, usage details and engine extensions stay JSON objects so new
variants survive forwarding without regenerating the SDK dependency closure.
The inspector derives media fields from these models but walks the original
request, preserving engine-specific shapes and file-ID/base64 alternatives.
"""

from __future__ import annotations

from typing import Annotated, Any, Literal

from pydantic import BaseModel, ConfigDict, Field

from aibrix.openai_frontend.schemas.media import MEDIA_URL

# Accepted true spellings for the Responses GET stream query (Pydantic bool).
RESPONSE_STREAM_TRUE_VALUES = frozenset({"1", "true", "t", "on", "y", "yes"})


class ResponseInputText(BaseModel):
    model_config = ConfigDict(extra="allow")
    type: Literal["input_text"] = "input_text"
    text: str


class ResponseInputImage(BaseModel):
    model_config = ConfigDict(extra="allow")
    type: Literal["input_image"] = "input_image"
    image_url: str | None = Field(None, json_schema_extra={MEDIA_URL: ""})
    file_id: str | None = None
    detail: str | None = None


class ResponseInputFile(BaseModel):
    model_config = ConfigDict(extra="allow")
    type: Literal["input_file"] = "input_file"
    file_url: str | None = Field(None, json_schema_extra={MEDIA_URL: ""})
    file_id: str | None = None
    file_data: str | None = None
    filename: str | None = None


ResponseInputPart = Annotated[
    ResponseInputText | ResponseInputImage | ResponseInputFile,
    Field(discriminator="type"),
]


class ResponseInputMessage(BaseModel):
    model_config = ConfigDict(extra="allow")
    role: str
    content: str | list[ResponseInputPart | dict[str, Any]]
    type: str | None = None


class CreateResponseRequest(BaseModel):
    model_config = ConfigDict(extra="allow")
    model: str | None = None
    input: str | list[ResponseInputMessage | dict[str, Any]] | None = None
    instructions: str | None = None
    previous_response_id: str | None = None
    stream: bool | None = None
    background: bool | None = None
    store: bool | None = None
    max_output_tokens: int | None = None
    max_tool_calls: int | None = None
    temperature: float | None = None
    top_p: float | None = None
    tools: list[dict[str, Any]] | None = None
    tool_choice: str | dict[str, Any] | None = None
    parallel_tool_calls: bool | None = None
    reasoning: dict[str, Any] | None = None
    text: dict[str, Any] | None = None
    metadata: dict[str, Any] | None = None
    include: list[str] | None = None
    truncation: str | None = None
    service_tier: str | None = None
    user: str | None = None


class ResponseCompactParams(BaseModel):
    model_config = ConfigDict(extra="allow")
    model: str
    input: str | list[ResponseInputMessage | dict[str, Any]]
    instructions: str | None = None
    previous_response_id: str | None = None


class InputTokenCountParams(BaseModel):
    model_config = ConfigDict(extra="allow")
    model: str | None = None
    input: str | list[ResponseInputMessage | dict[str, Any]] | None = None
    tools: list[dict[str, Any]] | None = None
    tool_choice: str | dict[str, Any] | None = None
    instructions: str | None = None
    previous_response_id: str | None = None


class Response(BaseModel):
    model_config = ConfigDict(extra="allow")
    id: str
    object: Literal["response"]
    created_at: float
    model: str
    output: list[dict[str, Any]]
    status: str | None = None
    usage: dict[str, Any] | None = None
    error: dict[str, Any] | None = None
    incomplete_details: dict[str, Any] | None = None
    metadata: dict[str, Any] | None = None
    previous_response_id: str | None = None


class CompactedResponse(BaseModel):
    model_config = ConfigDict(extra="allow")
    id: str
    object: Literal["response.compaction"]
    created_at: float
    output: list[dict[str, Any]]
    usage: dict[str, Any] | None = None


class ResponseDeleted(BaseModel):
    model_config = ConfigDict(extra="allow")
    id: str
    object: str
    deleted: bool


class ResponseInputItemList(BaseModel):
    model_config = ConfigDict(extra="allow")
    object: Literal["list"]
    data: list[dict[str, Any]]
    has_more: bool
    first_id: str | None = None
    last_id: str | None = None


class InputTokenCountResponse(BaseModel):
    model_config = ConfigDict(extra="allow")
    object: Literal["response.input_tokens"]
    input_tokens: int


class ResponseStreamEvent(BaseModel):
    """Keep named SSE events and all event-specific fields, including new types."""

    model_config = ConfigDict(extra="allow")
    type: str


# Include values are engine/version-specific; do not narrow the query catalog.
ResponseIncludable = str
