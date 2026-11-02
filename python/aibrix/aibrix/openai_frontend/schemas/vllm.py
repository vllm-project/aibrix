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

"""vLLM-specific extension fields for the OpenAI request models.

Only fields vLLM accepts beyond the OpenAI schema and the fields shared
with the other engine (``engine_common.py``) are declared here.

Reference: the vLLM OpenAI entrypoint protocols at tag ``v0.33.0rc1``.
"""

from __future__ import annotations

from typing import Any, Optional

from aibrix.openai_frontend.schemas.engine_common import (
    SharedChatCompletionFields,
    SharedCompletionFields,
    SharedEmbeddingFields,
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
