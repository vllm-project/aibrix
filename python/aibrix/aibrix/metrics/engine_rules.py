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

from aibrix.metrics.standard_rules import RenameStandardRule, StandardRule

# Standard rule accroding to https://docs.google.com/document/d/1SpSp1E6moa4HSrJnS4x3NpLuj88sMXr2tbofKlzTZpk
VLLM_METRIC_STANDARD_RULES: Dict[str, StandardRule] = {
    "vllm:request_success": RenameStandardRule(
        "vllm:request_success", "aibrix:request_success"
    ),
    "vllm:num_requests_waiting": RenameStandardRule(
        "vllm:num_requests_waiting", "aibrix:queue_size"
    ),
    "vllm:time_to_first_token_seconds": RenameStandardRule(
        "vllm:time_to_first_token_seconds", "aibrix:time_to_first_token_seconds"
    ),
    "vllm:gpu_cache_usage_perc": RenameStandardRule(
        "vllm:gpu_cache_usage_perc", "aibrix:gpu_cache_usage_perc"
    ),
    "vllm:time_per_output_token_seconds": RenameStandardRule(
        "vllm:time_per_output_token_seconds", "aibrix:time_per_output_token"
    ),
    "vllm:e2e_request_latency_seconds": RenameStandardRule(
        "vllm:e2e_request_latency_seconds", "aibrix:e2e_request_latency"
    ),
}

# TODO add more engine standard rules


def get_metric_standard_rules(engine: str) -> Dict[str, StandardRule]:
    if engine.lower() == "vllm":
        return VLLM_METRIC_STANDARD_RULES
    else:
        raise ValueError(f"Engine {engine} is not supported.")
