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

"""SGLang-specific extension fields for the OpenAI request models.

Only fields SGLang accepts beyond the OpenAI schema and the fields shared
with the other engine (``engine_common.py``) are declared here.

Reference: the SGLang OpenAI entrypoint protocol at tag ``v0.5.22``.
"""

from __future__ import annotations

from typing import Any, Optional

from aibrix.openai_frontend.schemas.engine_common import (
    SharedChatCompletionFields,
    SharedCompletionFields,
    SharedEmbeddingFields,
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
