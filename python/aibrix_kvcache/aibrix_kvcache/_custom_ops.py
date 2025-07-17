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

import torch

from .common.absl_logging import getLogger

logger = getLogger(__name__)

try:
    import aibrix_kvcache._aibrix_C  # noqa: F401
except ImportError as e:
    logger.warning("Failed to import from aibrix_kvcache._aibrix_C with %r", e)


def reshape_and_cache_multi_layer(
    offload_kv_cache_blocks: list[torch.Tensor],
    kv_caches: list[torch.Tensor],
    slot_mapping: torch.Tensor,
    block_size: int,
    kv_cache_dtype: str,
    k_scales: list[torch.Tensor],
    v_scales: list[torch.Tensor],
    layout: str,
) -> None:
    torch.ops._aibrix_C_cache_ops.reshape_and_cache_multi_layer(
        offload_kv_cache_blocks,
        kv_caches,
        slot_mapping,
        block_size,
        kv_cache_dtype,
        k_scales,
        v_scales,
        layout,
    )


def reshape_and_offload_multi_layer(
    offload_kv_cache_blocks: list[torch.Tensor],
    kv_caches: list[torch.Tensor],
    slot_mapping: torch.Tensor,
    block_size: int,
    kv_cache_dtype: str,
    k_scales: list[torch.Tensor],
    v_scales: list[torch.Tensor],
    layout: str,
) -> None:
    torch.ops._aibrix_C_cache_ops.reshape_and_offload_multi_layer(
        offload_kv_cache_blocks,
        kv_caches,
        slot_mapping,
        block_size,
        kv_cache_dtype,
        k_scales,
        v_scales,
        layout,
    )
