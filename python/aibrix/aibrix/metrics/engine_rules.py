# Copyright 2024 The Aibrix Team.
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

from typing import Dict

from aibrix.metrics.standard_rules import (
    PassthroughStandardRule,
    RenameStandardRule,
    StandardRule,
)

# vLLM metric standard rules - complete coverage
VLLM_METRIC_STANDARD_RULES: Dict[str, StandardRule] = {
    # Core performance metrics - map to aibrix namespace
    "vllm:num_requests_waiting": RenameStandardRule(
        "vllm:num_requests_waiting", "aibrix:queue_size"
    ),
    "vllm:gpu_cache_usage_perc": RenameStandardRule(
        "vllm:gpu_cache_usage_perc", "aibrix:gpu_cache_usage_perc"
    ),
    # Token processing metrics
    "vllm:prompt_tokens_total": RenameStandardRule(
        "vllm:prompt_tokens_total", "aibrix:prompt_tokens_total"
    ),
    "vllm:generation_tokens_total": RenameStandardRule(
        "vllm:generation_tokens_total", "aibrix:generation_tokens_total"
    ),
    # Latency metrics
    "vllm:time_to_first_token_seconds": RenameStandardRule(
        "vllm:time_to_first_token_seconds", "aibrix:time_to_first_token_seconds"
    ),
    "vllm:time_per_output_token_seconds": RenameStandardRule(
        "vllm:time_per_output_token_seconds", "aibrix:time_per_output_token_seconds"
    ),
    "vllm:e2e_request_latency_seconds": RenameStandardRule(
        "vllm:e2e_request_latency_seconds", "aibrix:e2e_request_latency_seconds"
    ),
    # Request success metrics
    "vllm:request_success_total": RenameStandardRule(
        "vllm:request_success_total", "aibrix:request_success_total"
    ),
}

# SGLang metric standard rules - complete coverage
SGLANG_METRIC_STANDARD_RULES: Dict[str, StandardRule] = {
    # Core performance metrics - map to aibrix namespace
    "sglang:num_queue_reqs": RenameStandardRule(
        "sglang:num_queue_reqs", "aibrix:queue_size"
    ),
    "sglang:gen_throughput": RenameStandardRule(
        "sglang:gen_throughput", "aibrix:generation_throughput"
    ),
    # Token processing metrics
    "sglang:prompt_tokens_total": RenameStandardRule(
        "sglang:prompt_tokens_total", "aibrix:prompt_tokens_total"
    ),
    "sglang:generation_tokens_total": RenameStandardRule(
        "sglang:generation_tokens_total", "aibrix:generation_tokens_total"
    ),
    # Latency metrics
    "sglang:time_to_first_token_seconds": RenameStandardRule(
        "sglang:time_to_first_token_seconds", "aibrix:time_to_first_token_seconds"
    ),
    "sglang:time_per_output_token_seconds": RenameStandardRule(
        "sglang:time_per_output_token_seconds", "aibrix:time_per_output_token_seconds"
    ),
    "sglang:e2e_request_latency_seconds": RenameStandardRule(
        "sglang:e2e_request_latency_seconds", "aibrix:e2e_request_latency_seconds"
    ),
    # Cache and utilization metrics
    "sglang:cache_hit_rate": RenameStandardRule(
        "sglang:cache_hit_rate", "aibrix:cache_hit_rate"
    ),
    "sglang:token_usage": RenameStandardRule(
        "sglang:token_usage", "aibrix:token_usage"
    ),
    # Pass-through SGLang-specific metrics for debugging
    "sglang:num_running_reqs": PassthroughStandardRule("sglang:num_running_reqs"),
    "sglang:num_used_tokens": PassthroughStandardRule("sglang:num_used_tokens"),
    "sglang:func_latency_seconds": PassthroughStandardRule(
        "sglang:func_latency_seconds"
    ),
}

# Enhanced vLLM rules with pass-through for debugging metrics
ENHANCED_VLLM_METRIC_STANDARD_RULES: Dict[str, StandardRule] = {
    **VLLM_METRIC_STANDARD_RULES,
    # Pass-through vLLM-specific metrics for debugging
    "vllm:num_requests_running": PassthroughStandardRule("vllm:num_requests_running"),
    "vllm:request_prompt_tokens": PassthroughStandardRule("vllm:request_prompt_tokens"),
    "vllm:request_generation_tokens": PassthroughStandardRule(
        "vllm:request_generation_tokens"
    ),
}

# TODO add more engine standard rules


def get_metric_standard_rules(engine: str) -> Dict[str, StandardRule]:
    engine_lower = engine.lower()
    if engine_lower == "vllm":
        return ENHANCED_VLLM_METRIC_STANDARD_RULES
    elif engine_lower == "sglang":
        return SGLANG_METRIC_STANDARD_RULES
    else:
        raise ValueError(
            f"Engine {engine} is not supported. Supported engines: vllm, sglang"
        )
