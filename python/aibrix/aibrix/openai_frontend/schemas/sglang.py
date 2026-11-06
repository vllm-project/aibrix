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

"""SGLang-specific extension fields for the OpenAI request and response models.

Only fields SGLang accepts or returns beyond the OpenAI schema and the fields
shared with the other engine (``engine_common.py``) are declared here.

Reference: the SGLang OpenAI entrypoint protocol at tag ``v0.5.22``.
"""

from __future__ import annotations

from typing import Any, Dict, List, Optional, Union

from pydantic import BaseModel, ConfigDict, Field

from aibrix.openai_frontend.schemas.engine_common import (
    SharedChatCompletionFields,
    SharedCompletionChoiceFields,
    SharedCompletionFields,
    SharedEmbeddingFields,
)


class SglangGenerateMediaFields(BaseModel):
    """Native media fields; raw engine input shapes are preserved."""

    model_config = ConfigDict(extra="allow")

    image_data: Optional[Any] = Field(
        None, json_schema_extra={"media_type": "image_url"}
    )
    video_data: Optional[Any] = Field(
        None, json_schema_extra={"media_type": "video_url"}
    )
    audio_data: Optional[Any] = Field(
        None, json_schema_extra={"media_type": "audio_url"}
    )


class SglangImageResponseDataFields(BaseModel):
    """SGLang diffusion image output; not a request/input media field."""

    file_path: Optional[str] = Field(None, json_schema_extra={"output_media": "image"})


class SglangVideoResponseFields(BaseModel):
    """SGLang completed video job output paths (multimodal_gen API)."""

    file_path: Optional[str] = Field(None, json_schema_extra={"output_media": "video"})
    file_paths: Optional[List[Optional[str]]] = Field(
        None, json_schema_extra={"output_media": "video"}
    )


class SglangChatCompletionEngineExtensionFields(SharedChatCompletionFields):
    """SGLang /v1/chat/completions extensions."""

    bootstrap_host: Optional[Any] = None
    bootstrap_port: Optional[Any] = None
    bootstrap_room: Optional[Any] = None
    custom_logit_processor: Optional[Any] = None
    custom_params: Optional[Any] = None
    data_parallel_rank: Optional[Any] = None
    disagg_prefill_dp_rank: Optional[Any] = None
    ebnf: Optional[Any] = None
    extra_key: Optional[Any] = None
    images_config: Optional[Any] = None
    input_ids: Optional[Any] = None
    lora_path: Optional[Any] = None
    max_dynamic_patch: Optional[Any] = None
    min_dynamic_patch: Optional[Any] = None
    no_stop_trim: Optional[Any] = None
    regex: Optional[Any] = None
    return_cached_tokens_details: Optional[Any] = None
    return_flat_raw_output_top_logprobs: Optional[Any] = None
    return_flat_raw_top_logprobs_b64: Optional[Any] = None
    return_hidden_states: Optional[Any] = None
    return_input_ids_in_sglext: Optional[Any] = None
    return_meta_info: Optional[Any] = None
    return_output_ids_in_sglext: Optional[Any] = None
    return_prompt_token_ids: Optional[Any] = None
    return_routed_experts: Optional[Any] = None
    return_sampling_mask: Optional[Any] = None
    return_spec_tokens_details: Optional[Any] = None
    rid: Optional[Any] = None
    routed_dp_rank: Optional[Any] = None
    routed_experts_start_len: Optional[Any] = None
    sampling_logprobs_mode: Optional[Any] = None
    separate_reasoning: Optional[Any] = None
    session_params: Optional[Any] = None
    stop_regex: Optional[Any] = None
    stream_reasoning: Optional[Any] = None
    task: Optional[Any] = None
    use_audio_in_video: Optional[Any] = None
    video_config: Optional[Any] = None
    watermark: Optional[Any] = None


class SglangCompletionEngineExtensionFields(SharedCompletionFields):
    """SGLang /v1/completions extensions."""

    bootstrap_host: Optional[Any] = None
    bootstrap_port: Optional[Any] = None
    bootstrap_room: Optional[Any] = None
    custom_labels: Optional[Any] = None
    custom_logit_processor: Optional[Any] = None
    custom_params: Optional[Any] = None
    data_parallel_rank: Optional[Any] = None
    disagg_prefill_dp_rank: Optional[Any] = None
    ebnf: Optional[Any] = None
    extra_key: Optional[Any] = None
    images_config: Optional[Any] = None
    json_schema: Optional[Any] = None
    lora_path: Optional[Any] = None
    no_stop_trim: Optional[Any] = None
    regex: Optional[Any] = None
    return_cached_tokens_details: Optional[Any] = None
    return_hidden_states: Optional[Any] = None
    return_routed_experts: Optional[Any] = None
    return_spec_tokens_details: Optional[Any] = None
    rid: Optional[Any] = None
    routed_dp_rank: Optional[Any] = None
    routed_experts_start_len: Optional[Any] = None
    session_params: Optional[Any] = None
    stop_regex: Optional[Any] = None
    watermark: Optional[Any] = None


class SglangEmbeddingEngineExtensionFields(SharedEmbeddingFields):
    """SGLang /v1/embeddings extensions."""

    embed_override_token_id: Optional[Any] = None
    embed_overrides: Optional[Any] = None
    lora_path: Optional[Any] = None
    rid: Optional[Any] = None


# ---------------------------------------------------------------------------
# Responses
# ---------------------------------------------------------------------------


class SglangChatCompletionMessageExtensionFields(BaseModel):
    """SGLang fields on a chat response message."""

    reasoning_content: Optional[str] = None


class SglangChatCompletionStreamDeltaExtensionFields(
    SglangChatCompletionMessageExtensionFields
):
    """SGLang fields on a chat stream delta."""

    hidden_states: Optional[Any] = None


class SglangChatCompletionStreamChoiceExtensionFields(BaseModel):
    """SGLang fields on a /v1/chat/completions stream choice."""

    matched_stop: Optional[Union[int, str]] = None


class SglangChatCompletionChoiceExtensionFields(
    SglangChatCompletionStreamChoiceExtensionFields
):
    """SGLang fields on a /v1/chat/completions choice."""

    hidden_states: Optional[Any] = None
    prompt_token_ids: Optional[List[int]] = None
    response_token_ids: Optional[List[int]] = None
    meta_info: Optional[Dict[str, Any]] = None


class SglangResponseExtensionFields(BaseModel):
    """SGLang top-level fields on chat/completion responses and chunks."""

    sglext: Optional[Dict[str, Any]] = None


class SglangCompletionChoiceExtensionFields(SharedCompletionChoiceFields):
    """SGLang fields on a /v1/completions choice (and stream choice)."""

    matched_stop: Optional[Union[int, str]] = None
    hidden_states: Optional[Any] = None


class SglangCompletionResponseExtensionFields(SglangResponseExtensionFields):
    """SGLang top-level fields on a /v1/completions response (and chunk)."""

    metadata: Optional[Dict[str, Any]] = None


class SglangUsageExtensionFields(BaseModel):
    """SGLang fields on ``usage``."""

    reasoning_tokens: Optional[int] = None


class SglangPromptTokensDetailsExtensionFields(BaseModel):
    """SGLang fields on ``usage.prompt_tokens_details``."""

    video_tokens: Optional[int] = None
