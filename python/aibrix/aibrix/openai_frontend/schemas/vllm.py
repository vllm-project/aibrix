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

"""vLLM-specific extension fields for the OpenAI request and response models.

Only fields vLLM accepts or returns beyond the OpenAI schema and the fields
shared with the other engine (``engine_common.py``) are declared here.

Reference: the vLLM OpenAI entrypoint protocols at tag ``v0.33.0rc1``.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional, Union

from pydantic import BaseModel, Field

from aibrix.openai_frontend.schemas.engine_common import (
    OUTPUT_RESULT,
    ChoicesResponse,
    CountTokensResponse,
    DataResponse,
    MessagesResponse,
    NativeResponse,
    SharedChatCompletionFields,
    SharedCompletionChoiceFields,
    SharedCompletionFields,
    SharedEmbeddingFields,
    SharedModelFields,
    SystemOneResponse,
)


class VllmChatCompletionEngineExtensionFields(SharedChatCompletionFields):
    """vLLM /v1/chat/completions extensions."""

    add_generation_prompt: Optional[Any] = None
    add_special_tokens: Optional[Any] = None
    allowed_token_ids: Optional[Any] = None
    bad_words: Optional[Any] = None
    chat_template: Optional[Any] = None
    documents: Optional[Any] = None
    ec_transfer_params: Optional[Any] = None
    echo: Optional[Any] = None
    include_reasoning: Optional[Any] = None
    include_stop_str_in_output: Optional[Any] = None
    kv_transfer_params: Optional[Any] = None
    length_penalty: Optional[Any] = None
    logprob_token_ids: Optional[Any] = None
    media_io_kwargs: Optional[Any] = None
    mm_payload: Optional[Any] = None
    mm_processor_kwargs: Optional[Any] = None
    prompt_logprobs: Optional[Any] = None
    repetition_detection: Optional[Any] = None
    request_id: Optional[Any] = None
    return_mm_kwargs: Optional[Any] = None
    return_prompt_text: Optional[Any] = None
    return_token_offsets: Optional[Any] = None
    return_tokens_as_token_ids: Optional[Any] = None
    return_weight_versions: Optional[Any] = None
    routed_experts_prompt_start: Optional[Any] = None
    spaces_between_special_tokens: Optional[Any] = None
    stream_interval: Optional[Any] = None
    structured_outputs: Optional[Any] = None
    thinking_token_budget: Optional[Any] = None
    truncate_prompt_tokens: Optional[Any] = None
    truncation_side: Optional[Any] = None
    use_beam_search: Optional[Any] = None
    vllm_xargs: Optional[Any] = None
    watermarking: Optional[Any] = None


class VllmCompletionEngineExtensionFields(SharedCompletionFields):
    """vLLM /v1/completions extensions."""

    add_special_tokens: Optional[Any] = None
    allowed_token_ids: Optional[Any] = None
    bad_words: Optional[Any] = None
    ec_transfer_params: Optional[Any] = None
    include_stop_str_in_output: Optional[Any] = None
    kv_transfer_params: Optional[Any] = None
    length_penalty: Optional[Any] = None
    logprob_token_ids: Optional[Any] = None
    mm_payload: Optional[Any] = None
    prompt_embeds: Optional[Any] = None
    prompt_logprobs: Optional[Any] = None
    repetition_detection: Optional[Any] = None
    request_id: Optional[Any] = None
    return_mm_kwargs: Optional[Any] = None
    return_token_offsets: Optional[Any] = None
    return_tokens_as_token_ids: Optional[Any] = None
    return_weight_versions: Optional[Any] = None
    routed_experts_prompt_start: Optional[Any] = None
    spaces_between_special_tokens: Optional[Any] = None
    stream_interval: Optional[Any] = None
    structured_outputs: Optional[Any] = None
    thinking_token_budget: Optional[Any] = None
    truncate_prompt_tokens: Optional[Any] = None
    truncation_side: Optional[Any] = None
    use_beam_search: Optional[Any] = None
    vllm_xargs: Optional[Any] = None
    watermarking: Optional[Any] = None


class VllmEmbeddingEngineExtensionFields(SharedEmbeddingFields):
    """vLLM /v1/embeddings extensions."""

    add_generation_prompt: Optional[Any] = None
    add_special_tokens: Optional[Any] = None
    chat_template: Optional[Any] = None
    embed_dtype: Optional[Any] = None
    endianness: Optional[Any] = None
    media_io_kwargs: Optional[Any] = None
    mm_processor_kwargs: Optional[Any] = None
    padding: Optional[Any] = None
    request_id: Optional[Any] = None
    truncate_prompt_tokens: Optional[Any] = None
    truncation_side: Optional[Any] = None
    use_activation: Optional[Any] = None


# ---------------------------------------------------------------------------
# Responses
# ---------------------------------------------------------------------------


class VllmChatCompletionMessageExtensionFields(BaseModel):
    """vLLM fields on a chat response message and stream delta."""

    reasoning: Optional[str] = None


class VllmChatCompletionStreamChoiceExtensionFields(BaseModel):
    """vLLM fields on a /v1/chat/completions stream choice."""

    stop_reason: Optional[Union[int, str]] = None
    token_ids: Optional[List[int]] = None
    weight_versions: Optional[List[Dict[str, Any]]] = None


class VllmChatCompletionChoiceExtensionFields(
    VllmChatCompletionStreamChoiceExtensionFields
):
    """vLLM fields on a /v1/chat/completions choice."""

    routed_experts: Optional[str] = None


class VllmChatCompletionStreamResponseExtensionFields(BaseModel):
    """vLLM top-level fields on a /v1/chat/completions stream chunk."""

    prompt_token_ids: Optional[List[int]] = None
    prompt_text: Optional[str] = None
    kv_transfer_params: Optional[Dict[str, Any]] = None
    metrics: Optional[Dict[str, Any]] = None


class VllmChatCompletionResponseExtensionFields(
    VllmChatCompletionStreamResponseExtensionFields
):
    """vLLM top-level fields on a /v1/chat/completions response."""

    prompt_logprobs: Optional[List[Optional[Dict[str, Any]]]] = None
    ec_transfer_params: Optional[Dict[str, Any]] = None


class VllmCompletionChoiceExtensionFields(SharedCompletionChoiceFields):
    """vLLM fields on a /v1/completions choice (and stream choice)."""

    stop_reason: Optional[Union[int, str]] = None
    prompt_logprobs: Optional[List[Optional[Dict[str, Any]]]] = None
    routed_experts: Optional[str] = None
    weight_versions: Optional[List[Dict[str, Any]]] = None


class VllmCompletionResponseExtensionFields(BaseModel):
    """vLLM top-level fields on a /v1/completions response (and chunk)."""

    service_tier: Optional[str] = None
    kv_transfer_params: Optional[Dict[str, Any]] = None
    ec_transfer_params: Optional[Dict[str, Any]] = None
    metrics: Optional[Dict[str, Any]] = None


class VllmEmbeddingResponseExtensionFields(BaseModel):
    """vLLM top-level fields on a /v1/embeddings response."""

    id: Optional[str] = None
    created: Optional[int] = None


class VllmPromptTokensDetailsExtensionFields(BaseModel):
    """vLLM fields on ``usage.prompt_tokens_details``."""

    created_cache_tokens: Optional[int] = None
    multimodal_tokens: Optional[Dict[str, int]] = None


class VllmModelExtensionFields(SharedModelFields):
    """vLLM fields on a ``/v1/models`` entry."""

    permission: Optional[List[Dict[str, Any]]] = None


# Native result contracts and endpoint bindings (same upstream version as above).
class VllmRerankResponse(NativeResponse):
    results: list[dict[str, Any]] = Field(json_schema_extra={OUTPUT_RESULT: True})


class VllmTokenizeResponse(NativeResponse):
    tokens: list[int] = Field(json_schema_extra={OUTPUT_RESULT: True})
    token_strs: list[str] | None = Field(None, json_schema_extra={OUTPUT_RESULT: True})


class VllmDetokenizeResponse(NativeResponse):
    prompt: str = Field(json_schema_extra={OUTPUT_RESULT: True})


class CohereMessage(NativeResponse):
    content: list[dict[str, Any]] | None = Field(
        None, json_schema_extra={OUTPUT_RESULT: True}
    )
    tool_calls: list[dict[str, Any]] | None = Field(
        None, json_schema_extra={OUTPUT_RESULT: True}
    )
    tool_plan: str | None = Field(None, json_schema_extra={OUTPUT_RESULT: True})
    citations: list[dict[str, Any]] | None = Field(
        None, json_schema_extra={OUTPUT_RESULT: True}
    )


class CohereChatResponse(NativeResponse):
    message: CohereMessage
    logprobs: list[dict[str, Any]] | None = Field(
        None, json_schema_extra={OUTPUT_RESULT: True}
    )


class CohereEmbedResponse(NativeResponse):
    embeddings: dict[str, Any] = Field(json_schema_extra={OUTPUT_RESULT: True})


class VllmDecisionResponse(NativeResponse):
    answers: list[dict[str, Any]] = Field(json_schema_extra={OUTPUT_RESULT: True})


VLLM_NATIVE_RESPONSES: dict[str, Any] = {
    "/v1/decisions": VllmDecisionResponse,
    "/v1/systemone": SystemOneResponse,
    "/v1/messages": MessagesResponse,
    "/v1/messages/count_tokens": CountTokensResponse,
    "/cohere/v2/chat": CohereChatResponse,
    "/v2/embed": CohereEmbedResponse,
    "/inference/v1/generate": ChoicesResponse,
    "/generative_scoring": DataResponse,
    "/tokenize": VllmTokenizeResponse,
    "/detokenize": VllmDetokenizeResponse,
    **{path: VllmRerankResponse for path in ("/rerank", "/v1/rerank", "/v2/rerank")},
    **{path: DataResponse for path in ("/score", "/v1/score", "/pooling", "/classify")},
}
