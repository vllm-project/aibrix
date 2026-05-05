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

import importlib
import logging
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from vllm.v1.core.sched.output import SchedulerOutput

logger = logging.getLogger(__name__)

VLLM_V1_WORKER_GPU_MODEL_RUNNER_MODULE = "vllm.v1.worker.gpu_model_runner"

# Track if patches are applied
_patches_applied = False


def _apply_gpu_model_runner_patches(module):
    """Apply patches to an already-imported gpu_model_runner module."""
    GPUModelRunner = module.GPUModelRunner

    # ------------------------------------------------------------------
    # Source-patch detection: if GPUModelRunner already has patched signature,
    # the source-code patch is in place and we should not double-patch.
    # ------------------------------------------------------------------
    if getattr(GPUModelRunner, "_aibrix_patched", False):
        logger.info("[AIBrix] vLLM source patch detected, skipping patch")
        return

    # Check if _update_states already accepts load_results (source patch)
    import inspect

    sig = inspect.signature(GPUModelRunner._update_states)
    if "load_results" in sig.parameters:
        logger.info(
            "[AIBrix] vLLM source patch detected, _update_states has "
            "load_results, skipping patch"
        )
        return

    logger.info("[AIBrix] Applying patches to vLLM GPUModelRunner...")

    # Import has_kv_transfer_group at patch time to avoid import errors
    from vllm.distributed.kv_transfer import has_kv_transfer_group

    # ---- Patch 1: GPUModelRunner.execute_model -----------------------
    # This patch intercepts execute_model to:
    # 1. Call kv_connector_load_before_update() before _update_states
    # 2. Store load_results in context for _update_states to use
    _orig_execute_model = GPUModelRunner.execute_model

    def _patched_execute_model(self, scheduler_output, *args, **kwargs):
        """Wrapped execute_model that calls KV connector before state updates"""
        # Clear previous load_results
        self._aibrix_load_results = {}

        # Get load_results from KV connector before _update_states
        if has_kv_transfer_group() and hasattr(
            self, "kv_connector_load_before_update"
        ):
            self._aibrix_load_results = self.kv_connector_load_before_update(
                scheduler_output
            )
        elif has_kv_transfer_group():
            # Fallback: call directly using mixin method
            from vllm.distributed.kv_transfer import get_kv_transfer_group

            kv_connector = get_kv_transfer_group()
            if scheduler_output.kv_connector_metadata is not None:
                kv_connector.bind_connector_metadata(
                    scheduler_output.kv_connector_metadata
                )
                self._aibrix_load_results = (
                    kv_connector.start_load_kv_before_update()
                )

        # Call original execute_model - it will call _update_states
        return _orig_execute_model(self, scheduler_output, *args, **kwargs)

    GPUModelRunner.execute_model = _patched_execute_model

    # ---- Patch 2: GPUModelRunner._update_states ----------------------
    # This patch pre-processes load_results before calling original
    # _update_states, then fixes up cached request states afterward.
    _orig_update_states = GPUModelRunner._update_states

    def _patched_update_states(self, scheduler_output: "SchedulerOutput"):
        """Wrapped _update_states that handles AIBrix KV load_results."""
        load_results = getattr(self, "_aibrix_load_results", {})

        # 1. Adjust scheduler_output before original
        # 1.1 For new requests: modify scheduler_output.scheduled_new_reqs in
        # place
        _preprocess_new_reqs(scheduler_output, load_results)

        # 1.2 For cached requests: modify scheduler_output.scheduled_cached_reqs
        # in place.
        _preprocess_cached_reqs(scheduler_output, load_results)

        # 2. call original _update_states ----
        _orig_update_states(self, scheduler_output)

    GPUModelRunner._update_states = _patched_update_states

    # Mark class so we never double-patch
    GPUModelRunner._aibrix_patched = True
    logger.info("[AIBrix] GPUModelRunner patched successfully")


def _preprocess_new_reqs(
    scheduler_output: "SchedulerOutput",
    load_results: dict[str, int],
) -> None:
    """Pre-process load_results for new requests.

    Modifies scheduler_output.scheduled_new_reqs in place:
    - Increases num_computed_tokens by num_loaded_tokens
    - Decreases num_scheduled_tokens by num_loaded_tokens
    - Decreases total_num_scheduled_tokens by num_loaded_tokens
    """
    if not load_results:
        return

    for new_req_data in scheduler_output.scheduled_new_reqs:
        req_id = new_req_data.req_id
        num_loaded_tokens = load_results.get(req_id, 0)

        if num_loaded_tokens <= 0:
            continue

        num_scheduled_tokens = scheduler_output.num_scheduled_tokens[req_id]

        # If all tokens would be loaded, leave at least one for compute
        if num_loaded_tokens == num_scheduled_tokens:
            num_loaded_tokens -= 1

        # Adjust computed and scheduled tokens
        new_req_data.num_computed_tokens += num_loaded_tokens
        scheduler_output.num_scheduled_tokens[req_id] -= num_loaded_tokens
        scheduler_output.total_num_scheduled_tokens -= num_loaded_tokens


def _preprocess_cached_reqs(
    scheduler_output: "SchedulerOutput",
    load_results: dict[str, int],
) -> None:
    """Pre-process load_results for cached/running requests.

    Modifies scheduler_output.scheduled_cached_reqs in place:
    - Increases num_computed_tokens[i] by num_loaded_tokens
    - Updates new_token_ids[i] if available
    - Decreases num_scheduled_tokens[req_id] by num_loaded_tokens
    - Decreases total_num_scheduled_tokens by num_loaded_tokens
    """
    if not load_results:
        return

    req_data = scheduler_output.scheduled_cached_reqs

    for i, req_id in enumerate(req_data.req_ids):
        num_loaded_tokens = load_results.get(req_id, 0)

        if num_loaded_tokens <= 0:
            continue

        num_scheduled_tokens = scheduler_output.num_scheduled_tokens[req_id]

        # If all tokens would be loaded, leave at least one for compute
        if num_loaded_tokens == num_scheduled_tokens:
            num_loaded_tokens -= 1

        # Adjust computed and scheduled tokens
        req_data.num_computed_tokens[i] += num_loaded_tokens
        if req_data.new_token_ids and req_data.new_token_ids[i]:
            req_data.new_token_ids[i] = req_data.new_token_ids[i][
                num_loaded_tokens:
            ]

        scheduler_output.num_scheduled_tokens[req_id] -= num_loaded_tokens
        scheduler_output.total_num_scheduled_tokens -= num_loaded_tokens


def aibrix_patch_vllm():
    """Apply AIBrix patches to vLLM"""
    global _patches_applied
    if _patches_applied:
        logger.info("[AIBrix] Already patched — skipping")
        return

    # Patch GPUModelRunner
    try:
        module = importlib.import_module(VLLM_V1_WORKER_GPU_MODEL_RUNNER_MODULE)
        _apply_gpu_model_runner_patches(module)
    except ImportError as e:
        logger.warning("[AIBrix] Failed to patch gpu_model_runner: %s", e)

    _patches_applied = True


aibrix_patch_vllm()
