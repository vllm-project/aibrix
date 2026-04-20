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

import inspect

import torch
from vllm.v1.attention.backend import AttentionBackend
from vllm.v1.attention.selector import (
    get_attn_backend as _vllm_get_attn_backend,
)


def _has_block_size_param(func) -> bool:
    """Check if the function has block_size parameter."""
    sig = inspect.signature(func)
    return "block_size" in sig.parameters


def get_attn_backend(
    head_size: int,
    dtype: torch.dtype,
    kv_cache_dtype: str | None,
    block_size: int | None,
    use_mla: bool = False,
) -> type[AttentionBackend]:
    """
    Compat version of get_attn_backend that works with different vLLM versions.
    """
    if _has_block_size_param(_vllm_get_attn_backend):
        # Version with block_size parameter
        return _vllm_get_attn_backend(
            head_size=head_size,
            dtype=dtype,
            kv_cache_dtype=kv_cache_dtype,
            block_size=block_size,
            use_mla=use_mla,
        )
    else:
        # Version without block_size parameter. Since v0.18.0.
        return _vllm_get_attn_backend(
            head_size=head_size,
            dtype=dtype,
            kv_cache_dtype=kv_cache_dtype,
            use_mla=use_mla,
        )
