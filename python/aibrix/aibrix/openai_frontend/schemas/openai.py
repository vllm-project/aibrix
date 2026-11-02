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

"""Pydantic models for the OpenAI-compatible REST API.

Structure, in request-lifecycle order:

* wire-value literals and shared primitives (errors, usage, tools)
* request messages and the three request models
  (``/v1/completions``, ``/v1/chat/completions``, ``/v1/embeddings``)
* response models (text/chat/streaming/embeddings) and model listing

Standard request fields track openai-python's public type definitions at tag
``v3.41.0``. Engine-specific fields live on the mixins in ``schemas/vllm.py``
(vLLM tag ``v0.33.0rc1``) and ``schemas/sglang.py`` (SGLang tag ``v0.5.22``)
and are forwarded to the engine verbatim.
"""

# mypy: disable-error-code="valid-type,misc,assignment"

from __future__ import annotations

from enum import Enum
from typing import Annotated, Any, Dict, List, Literal, Optional, Union

from pydantic import (
    BaseModel,
    ConfigDict,
    Field,
    RootModel,
    confloat,
    conint,
    model_validator,
)

from aibrix.openai_frontend.schemas.media import (
    ChatCompletionRequestMessageContentPartAudio,
    ChatCompletionRequestMessageContentPartFile,
    ChatCompletionRequestMessageContentPartInputAudio,
    ChatCompletionRequestMessageContentPartRefusal,
    ChatCompletionRequestMessageContentPartVideo,
)
from aibrix.openai_frontend.schemas.sglang import (
    SglangChatCompletionEngineExtensionFields,
    SglangCompletionEngineExtensionFields,
    SglangEmbeddingEngineExtensionFields,
)
from aibrix.openai_frontend.schemas.vipe import ViPERequest, ViPEResponse
from aibrix.openai_frontend.schemas.vllm import (
    VllmChatCompletionEngineExtensionFields as ChatCompletionEngineExtensionFields,
)
from aibrix.openai_frontend.schemas.vllm import (
    VllmCompletionEngineExtensionFields as CompletionEngineExtensionFields,
)
from aibrix.openai_frontend.schemas.vllm import (
    VllmEmbeddingEngineExtensionFields as EmbeddingEngineExtensionFields,
)

# ---------------------------------------------------------------------------
# Wire-value literals
# ---------------------------------------------------------------------------

ReasoningEffort = Literal["none", "minimal", "low", "medium", "high", "xhigh", "max"]

ObjectTypeModel = Literal["model"]
ObjectTypeList = Literal["list"]
ObjectTypeTextCompletion = Literal["text_completion"]
ObjectTypeChatCompletion = Literal["chat.completion"]
ObjectTypeChatCompletionChunk = Literal["chat.completion.chunk"]
ObjectTypeEmbedding = Literal["embedding"]


class ObjectType:
    """String constants for the ``object`` discriminator on responses."""

    model: ObjectTypeModel = "model"
    list: ObjectTypeList = "list"
    text_completion: ObjectTypeTextCompletion = "text_completion"
    chat_completion: ObjectTypeChatCompletion = "chat.completion"
    chat_completion_chunk: ObjectTypeChatCompletionChunk = "chat.completion.chunk"


class FinishReason(str, Enum):
    """Why a text completion stopped."""

    stop = "stop"
    length = "length"
    content_filter = "content_filter"


class ChatCompletionFinishReason(str, Enum):
    """Why a chat completion stopped."""

    stop = "stop"
    length = "length"
    content_filter = "content_filter"
    tool_calls = "tool_calls"
    function_call = "function_call"


ImageDetail = Literal["auto", "low", "high", "original"]
ToolChoiceMode = Literal["none", "auto", "required"]


# ---------------------------------------------------------------------------
# Errors & generic responses
# ---------------------------------------------------------------------------


class Error(BaseModel):
    code: Optional[str] = None
    message: str
    param: Optional[str] = None
    type: str


class ErrorResponse(BaseModel):
    error: Error


class DeleteModelResponse(BaseModel):
    id: str
    deleted: bool
    object: str


# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------


class PromptTokensDetails(BaseModel):
    model_config = ConfigDict(extra="allow")

    cached_tokens: Optional[int] = None
    audio_tokens: Optional[int] = None


class CompletionTokensDetails(BaseModel):
    model_config = ConfigDict(extra="allow")

    reasoning_tokens: Optional[int] = None
    audio_tokens: Optional[int] = None
    accepted_prediction_tokens: Optional[int] = None
    rejected_prediction_tokens: Optional[int] = None


class Usage(BaseModel):
    model_config = ConfigDict(extra="allow")

    prompt_tokens: int = Field(..., description="Number of tokens in the prompt.")
    total_tokens: int = Field(
        ..., description="Total number of tokens used (prompt + completion)."
    )
    completion_tokens: int = Field(
        0, description="Number of tokens in the generated completion."
    )
    prompt_tokens_details: Optional[PromptTokensDetails] = None
    completion_tokens_details: Optional[CompletionTokensDetails] = None


# ---------------------------------------------------------------------------
# Tools / functions
# ---------------------------------------------------------------------------


class FunctionParameters(BaseModel):
    model_config = ConfigDict(extra="allow")


class FunctionDefinition(BaseModel):
    """A callable tool exposed to the model (function calling)."""

    model_config = ConfigDict(extra="allow")

    name: str = Field(
        ...,
        description="The name of the function to call (a-z, A-Z, 0-9, _ or -).",
    )
    description: Optional[str] = None
    parameters: Optional[FunctionParameters] = None
    strict: Optional[bool] = Field(
        None, description="Whether to enforce strict schema adherence."
    )


class ChatCompletionTool(BaseModel):
    """``function`` tools and OpenAI's ``custom`` tools."""

    model_config = ConfigDict(extra="allow")

    type: str = Field(..., description="The type of the tool (`function`/`custom`).")
    function: Optional[FunctionDefinition] = None


class FunctionCallRef(BaseModel):
    name: str


class NamedToolChoice(BaseModel):
    """``{"type": "function", "function": {"name": "..."}}``.

    ``extra="allow"`` also accepts ``custom`` and ``allowed_tools`` choices.
    """

    model_config = ConfigDict(extra="allow")

    type: str
    function: Optional[FunctionCallRef] = None


ToolChoice = Union[ToolChoiceMode, NamedToolChoice]


class FunctionCall(BaseModel):
    """A function call as reported in a response message."""

    name: str
    arguments: str = Field(
        ..., description="JSON arguments generated by the model (may be partial)."
    )


class ToolCallFunction(BaseModel):
    name: Optional[str] = None
    arguments: Optional[str] = None


class ToolCall(BaseModel):
    id: str
    type: str = Field(..., description="The type of the tool (`function`).")
    function: FunctionCall


class ToolCallChunk(BaseModel):
    index: int
    id: Optional[str] = None
    type: Optional[str] = None
    function: Optional[ToolCallFunction] = None


# Deprecated OpenAI function-calling fields (superseded by tools).
class LegacyFunctions(BaseModel):
    name: str
    description: Optional[str] = None
    parameters: Optional[FunctionParameters] = None


# ---------------------------------------------------------------------------
# Structured outputs & streaming options
# ---------------------------------------------------------------------------


class JSONSchemaFormat(BaseModel):
    model_config = ConfigDict(
        extra="allow", populate_by_name=True, serialize_by_alias=True
    )

    name: str = Field(..., description="The name of the response format.")
    description: Optional[str] = None
    # Attribute is `json_schema` only because `schema` shadows a BaseModel
    # attribute; on the wire it is always `schema`, as in the OpenAI API.
    json_schema: Optional[Dict[str, Any]] = Field(None, alias="schema")
    strict: Optional[bool] = None


class ResponseFormat(BaseModel):
    """``text`` / ``json_object`` / ``json_schema`` and vLLM's ``structural_tag``."""

    model_config = ConfigDict(extra="allow")

    type: Literal["text", "json_object", "json_schema", "structural_tag"] = "text"
    json_schema: Optional[JSONSchemaFormat] = None

    @model_validator(mode="after")
    def _validate_schema(self) -> ResponseFormat:
        if self.type == "json_schema" and self.json_schema is None:
            raise ValueError("json_schema is required when type is 'json_schema'")
        return self


class StreamOptions(BaseModel):
    include_usage: Optional[bool] = Field(
        False, description="Emit a final chunk carrying total token usage."
    )
    include_obfuscation: Optional[bool] = None
    # vLLM extension: report usage on every chunk.
    continuous_usage_stats: Optional[bool] = None


# ---------------------------------------------------------------------------
# Logprobs
# ---------------------------------------------------------------------------


class TopLogprob(BaseModel):
    token: str
    logprob: float
    bytes: Optional[List[int]] = None


class TokenLogprob(BaseModel):
    token: str
    logprob: float
    bytes: Optional[List[int]] = None
    top_logprobs: List[TopLogprob] = Field(default_factory=list)


class Logprobs(BaseModel):
    """Token logprobs for text completions."""

    text_offset: Optional[List[int]] = None
    token_logprobs: Optional[List[float]] = None
    tokens: Optional[List[str]] = None
    top_logprobs: Optional[List[Dict[str, float]]] = None


class ChatCompletionLogprobs(BaseModel):
    content: Optional[List[TokenLogprob]] = None


# ===========================================================================
# Chat messages
# ===========================================================================


class _ImageUrl(BaseModel):
    model_config = ConfigDict(extra="allow")

    # Plain str, not AnyUrl: the proxy also accepts tos://, s3://, gs://,
    # hf://, file:// and data: URIs, and AnyUrl would normalise or reject them.
    url: str = Field(
        ...,
        description="A URL (http(s)/tos/s3/gs/hf/file) or a base64 data URI.",
    )
    detail: Optional[ImageDetail] = "auto"


class ChatCompletionRequestMessageContentPartText(BaseModel):
    model_config = ConfigDict(use_enum_values=True, extra="allow")

    type: Literal["text"] = "text"
    text: str


class ChatCompletionRequestMessageContentPartImage(BaseModel):
    model_config = ConfigDict(use_enum_values=True, extra="allow")

    type: Literal["image_url"] = "image_url"
    image_url: _ImageUrl
    uuid: Optional[str] = None


ChatCompletionRequestMessageContentPart = Annotated[
    Union[
        ChatCompletionRequestMessageContentPartText,
        ChatCompletionRequestMessageContentPartImage,
        ChatCompletionRequestMessageContentPartVideo,
        ChatCompletionRequestMessageContentPartAudio,
        ChatCompletionRequestMessageContentPartInputAudio,
        ChatCompletionRequestMessageContentPartFile,
        ChatCompletionRequestMessageContentPartRefusal,
    ],
    Field(discriminator="type"),
]


class ChatCompletionRequestSystemMessage(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    role: Literal["system"] = "system"
    content: Union[str, List[ChatCompletionRequestMessageContentPartText]]
    name: Optional[str] = None


class ChatCompletionRequestDeveloperMessage(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    role: Literal["developer"] = "developer"
    content: Union[str, List[ChatCompletionRequestMessageContentPartText]]
    name: Optional[str] = None


class ChatCompletionRequestUserMessage(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    role: Literal["user"] = "user"
    # ViPE reuses a user message to carry its ViPERequest payload.
    content: Union[str, List[ChatCompletionRequestMessageContentPart], ViPERequest]
    name: Optional[str] = None


class ChatCompletionRequestAssistantMessage(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    role: Literal["assistant"] = "assistant"
    content: Optional[Union[str, List[ChatCompletionRequestMessageContentPart]]] = None
    refusal: Optional[str] = None
    # Prior-turn reasoning replayed by vLLM/SGLang reasoning models.
    reasoning: Optional[str] = None
    reasoning_content: Optional[str] = None
    name: Optional[str] = None
    tool_calls: Optional[List[ToolCall]] = None
    function_call: Optional[FunctionCall] = None  # deprecated


class ChatCompletionRequestToolMessage(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    role: Literal["tool"] = "tool"
    content: Union[str, List[ChatCompletionRequestMessageContentPartText]]
    tool_call_id: str


class ChatCompletionRequestFunctionMessage(BaseModel):
    model_config = ConfigDict(use_enum_values=True)

    role: Literal["function"] = "function"
    content: str
    name: str


class ChatCompletionRequestMessage(RootModel):
    """Any chat request message role."""

    root: Annotated[
        Union[
            ChatCompletionRequestDeveloperMessage,
            ChatCompletionRequestSystemMessage,
            ChatCompletionRequestUserMessage,
            ChatCompletionRequestAssistantMessage,
            ChatCompletionRequestToolMessage,
            ChatCompletionRequestFunctionMessage,
        ],
        Field(discriminator="role"),
    ]

    @property
    def role(self) -> str:
        return self.root.role

    @property
    def content(self) -> Any:
        return self.root.content


# ===========================================================================
# Requests
# ===========================================================================


class CreateCompletionRequest(
    CompletionEngineExtensionFields, SglangCompletionEngineExtensionFields
):
    model_config: ConfigDict = ConfigDict(extra="forbid")

    model: str
    prompt: Optional[Union[str, List[str], List[int], List[List[int]]]] = None

    best_of: Optional[conint(ge=0, le=20)] = 1
    echo: Optional[bool] = False
    frequency_penalty: Optional[confloat(ge=-2.0, le=2.0)] = 0
    logit_bias: Optional[Dict[str, int]] = None
    logprobs: Optional[conint(ge=0, le=5)] = None
    max_tokens: Optional[conint(ge=0)] = None
    n: Optional[conint(ge=1, le=128)] = 1
    presence_penalty: Optional[confloat(ge=-2.0, le=2.0)] = 0
    seed: Optional[int] = None
    stop: Optional[Union[str, List[str]]] = None
    stream: Optional[bool] = False
    stream_options: Optional[StreamOptions] = None
    suffix: Optional[str] = None
    temperature: Optional[confloat(ge=0.0, le=2.0)] = 1
    top_p: Optional[confloat(ge=0.0, le=1.0)] = 1
    user: Optional[str] = None

    @model_validator(mode="after")
    def _require_prompt(self):
        if self.prompt is None and getattr(self, "prompt_embeds", None) is None:
            raise ValueError("'prompt' is required (or vLLM 'prompt_embeds')")
        return self


class CreateChatCompletionRequest(
    ChatCompletionEngineExtensionFields, SglangChatCompletionEngineExtensionFields
):
    model_config: ConfigDict = ConfigDict(extra="forbid")

    messages: List[ChatCompletionRequestMessage] = Field(..., min_length=1)
    model: str

    frequency_penalty: Optional[confloat(ge=-2.0, le=2.0)] = 0
    logit_bias: Optional[Dict[str, int]] = None
    logprobs: Optional[bool] = False
    top_logprobs: Optional[conint(ge=0, le=20)] = None
    max_completion_tokens: Optional[conint(ge=0)] = None
    # Deprecated in favour of max_completion_tokens.
    max_tokens: Optional[conint(ge=0)] = None
    n: Optional[conint(ge=1, le=128)] = 1
    presence_penalty: Optional[confloat(ge=-2.0, le=2.0)] = 0
    response_format: Optional[ResponseFormat] = None
    seed: Optional[int] = None
    stop: Optional[Union[str, List[str]]] = None
    stream: Optional[bool] = False
    stream_options: Optional[StreamOptions] = None
    temperature: Optional[confloat(ge=0.0, le=2.0)] = 0.7
    top_p: Optional[confloat(ge=0.0, le=1.0)] = 1
    tools: Optional[List[ChatCompletionTool]] = None
    tool_choice: Optional[ToolChoice] = None
    user: Optional[str] = None
    reasoning_effort: Optional[ReasoningEffort] = None
    parallel_tool_calls: Optional[bool] = None

    # Newer OpenAI fields.
    audio: Optional[Dict[str, Any]] = None
    metadata: Optional[Dict[str, str]] = None
    modalities: Optional[List[Literal["text", "audio"]]] = None
    moderation: Optional[Dict[str, Any]] = None
    prediction: Optional[Dict[str, Any]] = None
    prompt_cache_key: Optional[str] = None
    prompt_cache_options: Optional[Dict[str, Any]] = None
    prompt_cache_retention: Optional[Literal["in_memory", "24h"]] = None
    safety_identifier: Optional[str] = None
    service_tier: Optional[
        Literal["auto", "default", "flex", "scale", "priority", "fast"]
    ] = None
    store: Optional[bool] = None
    verbosity: Optional[Literal["low", "medium", "high"]] = None
    web_search_options: Optional[Dict[str, Any]] = None

    # Deprecated OpenAI function-calling fields.
    function_call: Optional[Union[Literal["none", "auto"], FunctionCallRef]] = None
    functions: Optional[List[LegacyFunctions]] = Field(
        None, max_length=128, min_length=1
    )

    @model_validator(mode="after")
    def _validate_logprobs(self) -> CreateChatCompletionRequest:
        if self.top_logprobs is not None and not self.logprobs:
            raise ValueError("logprobs must be True if top_logprobs is specified")
        return self


class EmbeddingObject(BaseModel):
    model_config = ConfigDict(extra="allow")

    object: ObjectTypeEmbedding = "embedding"
    embedding: Union[List[float], str]
    index: int


class CreateEmbeddingRequest(
    EmbeddingEngineExtensionFields, SglangEmbeddingEngineExtensionFields
):
    model_config: ConfigDict = ConfigDict(extra="forbid")

    # Optional only for vLLM's chat-style embedding request (uses messages).
    input: Optional[Union[str, List[str], List[int], List[List[int]]]] = None
    messages: Optional[List[ChatCompletionRequestMessage]] = Field(
        None, description="vLLM: embed a chat conversation (multimodal models)."
    )
    model: str
    dimensions: Optional[int] = None
    encoding_format: Optional[Literal["float", "base64", "bytes", "bytes_only"]] = (
        "float"
    )
    user: Optional[str] = None

    @model_validator(mode="after")
    def _require_input_or_messages(self):
        if self.input is None and self.messages is None:
            raise ValueError("either 'input' or 'messages' is required")
        if isinstance(self.input, (str, list)) and len(self.input) == 0:
            raise ValueError("'input' must not be empty")
        return self


# ===========================================================================
# Responses: models
# ===========================================================================


class Model(BaseModel):
    id: str
    created: int
    object: ObjectTypeModel = "model"
    owned_by: str
    # vLLM extensions.
    root: Optional[str] = None
    parent: Optional[str] = None
    max_model_len: Optional[int] = None


class ListModelsResponse(BaseModel):
    object: ObjectTypeList = "list"
    data: List[Model]


# ===========================================================================
# Responses: completions
# ===========================================================================


class CompletionChoice(BaseModel):
    text: str
    index: int
    logprobs: Optional[Logprobs] = None
    finish_reason: Optional[FinishReason] = None


class CreateCompletionResponse(BaseModel):
    id: str
    choices: List[CompletionChoice]
    created: int
    model: str
    object: ObjectTypeTextCompletion = "text_completion"
    system_fingerprint: Optional[str] = None
    usage: Optional[Usage] = None


# ===========================================================================
# Responses: chat
# ===========================================================================


class ChatCompletionResponseMessage(BaseModel):
    role: str = "assistant"
    content: Optional[Union[str, ViPEResponse]] = None
    refusal: Optional[str] = None
    reasoning: Optional[str] = None
    reasoning_content: Optional[str] = None
    tool_calls: Optional[List[ToolCall]] = None
    function_call: Optional[FunctionCall] = None  # deprecated


class ChatCompletionChoice(BaseModel):
    index: int
    message: ChatCompletionResponseMessage
    finish_reason: Optional[ChatCompletionFinishReason] = None
    logprobs: Optional[ChatCompletionLogprobs] = None


class CreateChatCompletionResponse(BaseModel):
    id: str
    choices: List[ChatCompletionChoice]
    created: int
    model: str
    object: ObjectTypeChatCompletion = "chat.completion"
    system_fingerprint: Optional[str] = None
    usage: Optional[Usage] = None


class ChatCompletionStreamResponseDelta(BaseModel):
    role: Optional[str] = None
    content: Optional[Union[str, ViPEResponse]] = None
    refusal: Optional[str] = None
    reasoning: Optional[str] = None
    reasoning_content: Optional[str] = None
    tool_calls: Optional[List[ToolCallChunk]] = None
    function_call: Optional[ToolCallFunction] = None  # deprecated


class ChatCompletionStreamingResponseChoice(BaseModel):
    index: int
    delta: ChatCompletionStreamResponseDelta
    logprobs: Optional[ChatCompletionLogprobs] = None
    finish_reason: Optional[ChatCompletionFinishReason] = None


class CreateChatCompletionStreamResponse(BaseModel):
    id: str
    choices: List[ChatCompletionStreamingResponseChoice]
    created: int
    model: str
    object: ObjectTypeChatCompletionChunk = "chat.completion.chunk"
    system_fingerprint: Optional[str] = None
    usage: Optional[Usage] = None


# ===========================================================================
# Responses: embeddings
# ===========================================================================


class CreateEmbeddingResponse(BaseModel):
    model_config = ConfigDict(extra="allow")

    object: ObjectTypeList = "list"
    data: List[EmbeddingObject]
    model: str
    usage: Optional[Usage] = None
