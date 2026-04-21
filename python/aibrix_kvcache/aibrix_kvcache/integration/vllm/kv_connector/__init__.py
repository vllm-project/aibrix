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

logger = logging.getLogger(__name__)

VLLM_V1_WORKER_GPU_MODEL_RUNNER_MODULE = "vllm.v1.worker.gpu_model_runner"


def _apply_gpu_model_runner_patches(module):
    """Apply AIBrix patches to vLLM's GPUModelRunner.

    Wraps execute_model to:
    1. Trigger KV cache loading from external L1 cache before the forward
       pass (via kv_connector.start_load_kv_before_update)
    2. Trigger saving to external L1 cache after the forward pass
       (via kv_connector.wait_for_save)

    With get_num_new_matched_tokens implemented on the scheduler side
    (see aibrix_offloading_connector_type1.AIBrixOffloadingConnector),
    vLLM adjusts num_computed_tokens / num_scheduled_tokens before the
    worker receives scheduler_output. Therefore this patch does NOT need
    to preprocess scheduler_output — it only triggers the data transfer.
    """
    GPUModelRunner = module.GPUModelRunner

    # Detect already-patched (can happen when the connector module is
    # reloaded across fork boundaries): check the live method binding,
    # not class flags that may survive forks without the binding.
    if GPUModelRunner.execute_model.__name__ == "_patched_execute_model":
        logger.info("[AIBrix] GPUModelRunner already monkey-patched, skipping")
        return

    # Source-patch detection: if vLLM itself has been patched to accept
    # load_results in _update_states, we skip the monkey-patch.
    import inspect

    sig = inspect.signature(GPUModelRunner._update_states)
    if "load_results" in sig.parameters:
        logger.info(
            "[AIBrix] vLLM source patch detected, skipping monkey-patch"
        )
        return

    logger.info("[AIBrix] Applying monkey-patch to GPUModelRunner...")

    from vllm.distributed.kv_transfer import has_kv_transfer_group

    _orig_execute_model = GPUModelRunner.execute_model

    def _patched_execute_model(self, scheduler_output, *args, **kwargs):
        """Wrapped execute_model that triggers KV load before and
        KV save after the forward pass."""
        # LOAD phase: fetch KV from external cache into GPU buffers
        if has_kv_transfer_group():
            from vllm.distributed.kv_transfer import get_kv_transfer_group

            kv_connector = get_kv_transfer_group()
            if scheduler_output.kv_connector_metadata is not None:
                kv_connector.bind_connector_metadata(
                    scheduler_output.kv_connector_metadata
                )
                kv_connector.start_load_kv_before_update()

        # Forward pass
        result = _orig_execute_model(self, scheduler_output, *args, **kwargs)

        # SAVE phase: push new KV to external cache.
        # vLLM's native _get_kv_connector_output context manager may have
        # cleared _connector_metadata by this point, so we re-bind it.
        if has_kv_transfer_group():
            from vllm.distributed.kv_transfer import get_kv_transfer_group

            kv_connector = get_kv_transfer_group()
            if scheduler_output.kv_connector_metadata is not None:
                try:
                    kv_connector.bind_connector_metadata(
                        scheduler_output.kv_connector_metadata
                    )
                    kv_connector.wait_for_save()
                except Exception as e:  # noqa: BLE001
                    logger.warning("[AIBrix] wait_for_save failed: %r", e)

        return result

    GPUModelRunner.execute_model = _patched_execute_model
    logger.info("[AIBrix] GPUModelRunner monkey-patched successfully")


def aibrix_patch_vllm():
    """Apply AIBrix patches to vLLM at import time."""
    try:
        module = importlib.import_module(VLLM_V1_WORKER_GPU_MODEL_RUNNER_MODULE)
        _apply_gpu_model_runner_patches(module)
    except ImportError as e:
        logger.warning("[AIBrix] Failed to import gpu_model_runner: %s", e)


aibrix_patch_vllm()
