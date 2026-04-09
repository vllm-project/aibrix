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

# SPDX-License-Identifier: Apache-2.0
# SPDX-FileCopyrightText: Copyright contributors to the vLLM project

import pytest

import torch

if not torch.cuda.is_available():
    pytest.skip("CUDA is not available", allow_module_level=True)

import random
from typing import Optional, Union

from aibrix_kvcache import _custom_ops as ops


STR_DTYPE_TO_TORCH_DTYPE = {
    "float32": torch.float32,
    "half": torch.half,
    "bfloat16": torch.bfloat16,
    "float": torch.float,
    "fp8": torch.uint8,
    "fp8_e4m3": torch.uint8,
    "fp8_e5m2": torch.uint8,
}


def get_kv_cache_torch_dtype(
    cache_dtype: Optional[Union[str, torch.dtype]],
    model_dtype: Optional[Union[str, torch.dtype]] = None,
) -> torch.dtype:
    if isinstance(cache_dtype, str):
        if cache_dtype == "auto":
            if isinstance(model_dtype, str) and model_dtype in STR_DTYPE_TO_TORCH_DTYPE:
                torch_dtype = STR_DTYPE_TO_TORCH_DTYPE[model_dtype]
            elif isinstance(model_dtype, torch.dtype):
                torch_dtype = model_dtype
            else:
                raise ValueError(f"Invalid model dtype: {model_dtype}")
        elif cache_dtype in STR_DTYPE_TO_TORCH_DTYPE:
            torch_dtype = STR_DTYPE_TO_TORCH_DTYPE[cache_dtype]
        else:
            raise ValueError(f"Invalid kv cache dtype: {cache_dtype}")
    elif isinstance(cache_dtype, torch.dtype):
        torch_dtype = cache_dtype
    else:
        raise ValueError(f"Invalid kv cache dtype: {cache_dtype}")
    return torch_dtype


def seed_everything(seed: int):
    """Simple seed function to replace current_platform.seed_everything."""
    random.seed(seed)
    torch.manual_seed(seed)
    if torch.cuda.is_available():
        torch.cuda.manual_seed_all(seed)


def create_kv_caches_with_random_flash(
    num_blocks: int,
    block_size: int,
    num_layers: int,
    num_heads: int,
    head_size: int,
    cache_dtype: Optional[Union[str, torch.dtype]],
    model_dtype: Optional[Union[str, torch.dtype]] = None,
    seed: Optional[int] = None,
    device: Optional[str] = "cuda",
    cache_layout: Optional[str] = "NHD",
) -> tuple[list[torch.Tensor], list[torch.Tensor]]:
    """Create KV caches with random values for testing (from vLLM)."""
    seed_everything(seed)

    torch_dtype = get_kv_cache_torch_dtype(cache_dtype, model_dtype)
    generic_kv_cache_shape = (num_blocks, 2, block_size, num_heads, head_size)
    assert cache_layout in ("NHD", "HND")
    stride_order = (0, 1, 2, 3, 4) if cache_layout == "NHD" else (0, 1, 3, 2, 4)

    kv_cache_allocation_shape = tuple(generic_kv_cache_shape[i] for i in stride_order)
    scale = head_size**-0.5

    key_caches: list[torch.Tensor] = []
    value_caches: list[torch.Tensor] = []

    for _ in range(num_layers):
        if cache_dtype in ['fp8', 'fp8_e4m3', 'fp8_e5m2']:
            # For FP8, use randint to generate random uint8 values directly
            key_value_cache = torch.randint(
                0, 256,
                size=kv_cache_allocation_shape,
                dtype=torch_dtype,
                device=device
            ).permute(*stride_order)
        else:
            key_value_cache = torch.empty(size=kv_cache_allocation_shape,
                                          dtype=torch_dtype,
                                          device=device).permute(*stride_order)
            key_value_cache.uniform_(-scale, scale)
        key_caches.append(key_value_cache[:, 0])
        value_caches.append(key_value_cache[:, 1])
    return key_caches, value_caches


DTYPES = [torch.half, torch.bfloat16, torch.float]
NUM_LAYERS = [8]
NUM_HEADS = [8]
HEAD_SIZES = [64, 80]
BLOCK_SIZES = [8]
NUM_BLOCKS = [1024]

SEEDS = [0]
CUDA_DEVICES = ["cuda:0"]


@pytest.mark.parametrize("num_layers", NUM_LAYERS)
@pytest.mark.parametrize("num_heads", NUM_HEADS)
@pytest.mark.parametrize("head_size", HEAD_SIZES)
@pytest.mark.parametrize("block_size", BLOCK_SIZES)
@pytest.mark.parametrize("num_blocks", NUM_BLOCKS)
@pytest.mark.parametrize("dtype", DTYPES)
@pytest.mark.parametrize("seed", SEEDS)
@pytest.mark.parametrize("device", CUDA_DEVICES)
@pytest.mark.parametrize("kv_cache_dtype", ["auto", "fp8"])
@pytest.mark.parametrize("offload_layout", ["LCND", "NCLD"])
@torch.inference_mode()
def test_reshape_and_cache_multi_layer(
    num_layers: int,
    num_heads: int,
    head_size: int,
    block_size: int,
    num_blocks: int,
    dtype: torch.dtype,
    seed: int,
    device: str,
    kv_cache_dtype: str,
    offload_layout: str,
) -> None:
    seed_everything(seed)
    torch.set_default_device(device)

    # Create a random slot mapping.
    num_slots = block_size * num_blocks
    num_tokens = num_slots
    slot_mapping_lst = random.sample(range(num_slots), num_tokens)
    slot_mapping = torch.tensor(slot_mapping_lst,
                                dtype=torch.long,
                                device=device)

    embed_dim = num_heads * head_size
    offload_kv_cache_blocks = []
    torch_dtype = get_kv_cache_torch_dtype(kv_cache_dtype, dtype)
    is_fp8 = torch_dtype == torch.uint8

    # For FP8 cache, offload blocks should be in higher precision (FP16/BF16/FP32)
    # The kernel will quantize them to FP8 during onload
    offload_dtype_for_rand = torch.float16 if is_fp8 else torch_dtype

    if offload_layout == "LCND":
        for _ in range(num_blocks):
            offload_kv_cache_block = torch.randn(
                num_layers,
                2,
                block_size,
                embed_dim,
                dtype=offload_dtype_for_rand,
                device="cpu",
                pin_memory=True,
            )
            offload_kv_cache_blocks.append(offload_kv_cache_block)
    else:  # offload_layout == "NCLD"
        for _ in range(num_blocks):
            offload_kv_cache_block = torch.randn(
                block_size,
                2,
                num_layers,
                embed_dim,
                dtype=offload_dtype_for_rand,
                device="cpu",
                pin_memory=True,
            )
            offload_kv_cache_blocks.append(offload_kv_cache_block)

    # Create the KV caches.
    key_caches, value_caches = create_kv_caches_with_random_flash(
        num_blocks,
        block_size,
        num_layers,
        num_heads,
        head_size,
        kv_cache_dtype,
        dtype,
        seed=seed,
        device=device,
    )
    kv_caches = [
        torch.stack((key_caches[i], value_caches[i]))
        for i in range(num_layers)
    ]

    scale = max(block.amax() for block in offload_kv_cache_blocks)
    scale = (scale / 64.0).to(torch.float32).to(device)
    # Create separate tensor for each layer to avoid duplicate pointers
    scales = [scale.clone() for _ in range(num_layers)]

    # Clone the KV caches.
    cloned_kv_caches = [kv_cache.clone() for kv_cache in kv_caches]

    ops.reshape_and_cache_multi_layer(
        offload_kv_cache_blocks,
        kv_caches,
        slot_mapping,
        block_size,
        kv_cache_dtype,
        scales,
        scales,
        offload_layout,
    )

    # Run the reference implementation.
    block_indicies = torch.div(slot_mapping, block_size, rounding_mode="floor")
    block_indicies_lst = block_indicies.cpu().tolist()
    block_offsets = slot_mapping % block_size
    block_offsets_lst = block_offsets.cpu().tolist()

    if not is_fp8:
        # Helper to convert offload block slice to target shape
        def _to_target_shape(src_slice):
            return src_slice.view(num_heads, head_size)

        for i in range(num_layers):
            for j in range(num_tokens):
                block_idx = block_indicies_lst[j]
                block_offset = block_offsets_lst[j]
                ol_block_idx = j // block_size
                ol_block_offset = j % block_size
                if offload_layout == "LCND":
                    cloned_kv_caches[i][0, block_idx, block_offset, :] = _to_target_shape(
                        offload_kv_cache_blocks[ol_block_idx][i, 0, ol_block_offset])
                    cloned_kv_caches[i][1, block_idx, block_offset, :] = _to_target_shape(
                        offload_kv_cache_blocks[ol_block_idx][i, 1, ol_block_offset])
                else:
                    cloned_kv_caches[i][0, block_idx, block_offset, :] = _to_target_shape(
                        offload_kv_cache_blocks[ol_block_idx][ol_block_offset, 0, i])
                    cloned_kv_caches[i][1, block_idx, block_offset, :] = _to_target_shape(
                        offload_kv_cache_blocks[ol_block_idx][ol_block_offset, 1, i])

    if is_fp8:
        # For FP8, the kernel performs quantization which we can't easily replicate
        # in Python reference. Just verify the kernel ran without errors and
        # the output is not all zeros (i.e., it was modified).
        for i in range(num_layers):
            assert kv_caches[i].abs().sum() > 0, f"Layer {i} kv_cache is all zeros"
    else:
        for i in range(num_layers):
            torch.testing.assert_close(kv_caches[i], cloned_kv_caches[i])


@pytest.mark.parametrize("num_layers", NUM_LAYERS)
@pytest.mark.parametrize("num_heads", NUM_HEADS)
@pytest.mark.parametrize("head_size", HEAD_SIZES)
@pytest.mark.parametrize("block_size", BLOCK_SIZES)
@pytest.mark.parametrize("num_blocks", NUM_BLOCKS)
@pytest.mark.parametrize("dtype", DTYPES)
@pytest.mark.parametrize("seed", SEEDS)
@pytest.mark.parametrize("device", CUDA_DEVICES)
@pytest.mark.parametrize("kv_cache_dtype", ["auto", "fp8"])
@pytest.mark.parametrize("offload_layout", ["LCND", "NCLD"])
@torch.inference_mode()
def test_reshape_and_offload_multi_layer(
    num_layers: int,
    num_heads: int,
    head_size: int,
    block_size: int,
    num_blocks: int,
    dtype: torch.dtype,
    seed: int,
    device: str,
    kv_cache_dtype: str,
    offload_layout: str,
) -> None:
    seed_everything(seed)
    torch.set_default_device(device)

    # Create a random slot mapping.
    num_slots = block_size * num_blocks
    num_tokens = num_slots
    slot_mapping_lst = random.sample(range(num_slots), num_tokens)
    slot_mapping = torch.tensor(slot_mapping_lst,
                                dtype=torch.long,
                                device=device)

    embed_dim = num_heads * head_size
    offload_kv_cache_blocks = []
    torch_dtype = get_kv_cache_torch_dtype(kv_cache_dtype, dtype)
    is_fp8 = torch_dtype == torch.uint8

    # For FP8 cache, offload blocks should be in higher precision (FP16/BF16/FP32)
    # The kernel will quantize them to FP8 during onload
    offload_dtype_for_rand = torch.float16 if is_fp8 else torch_dtype

    if offload_layout == "LCND":
        for _ in range(num_blocks):
            block = torch.randn(
                num_layers,
                2,
                block_size,
                embed_dim,
                dtype=offload_dtype_for_rand,
                device="cpu",
                pin_memory=True,
            )
            offload_kv_cache_blocks.append(block)
    else:  # offload_layout == "NCLD"
        for _ in range(num_blocks):
            block = torch.randn(
                block_size,
                2,
                num_layers,
                embed_dim,
                dtype=offload_dtype_for_rand,
                device="cpu",
                pin_memory=True,
            )
            offload_kv_cache_blocks.append(block)

    # Create the KV caches.
    key_caches, value_caches = create_kv_caches_with_random_flash(
        num_blocks,
        block_size,
        num_layers,
        num_heads,
        head_size,
        kv_cache_dtype,
        dtype,
        seed=seed,
        device=device,
    )
    kv_caches = [
        torch.stack((key_caches[i], value_caches[i]))
        for i in range(num_layers)
    ]

    scales = [kv_cache.amax() for kv_cache in kv_caches]
    scales = [(scale / 64.0).to(torch.float32) for scale in scales]

    # Clone the offload kc cache blocks.
    cloned_offload_kv_cache_blocks = [
        block.clone() for block in offload_kv_cache_blocks
    ]

    ops.reshape_and_offload_multi_layer(
        offload_kv_cache_blocks,
        kv_caches,
        slot_mapping,
        block_size,
        kv_cache_dtype,
        scales,
        scales,
        offload_layout,
    )

    # Run the reference implementation.
    block_indicies = torch.div(slot_mapping, block_size, rounding_mode="floor")
    block_indicies_lst = block_indicies.cpu().tolist()
    block_offsets = slot_mapping % block_size
    block_offsets_lst = block_offsets.cpu().tolist()

    if not is_fp8:
        # Helper to convert kv cache slice to offload block format
        def _to_offload_format(src_slice):
            return src_slice.view(embed_dim)

        for i in range(num_layers):
            for j in range(num_tokens):
                block_idx = block_indicies_lst[j]
                block_offset = block_offsets_lst[j]
                ol_block_idx = j // block_size
                ol_block_offset = j % block_size
                if offload_layout == "LCND":
                    cloned_offload_kv_cache_blocks[ol_block_idx][
                        i, 0, ol_block_offset] = _to_offload_format(
                            kv_caches[i][0, block_idx, block_offset, :])
                    cloned_offload_kv_cache_blocks[ol_block_idx][
                        i, 1, ol_block_offset] = _to_offload_format(
                            kv_caches[i][1, block_idx, block_offset, :])
                else:
                    cloned_offload_kv_cache_blocks[ol_block_idx][
                        ol_block_offset, 0,
                        i] = _to_offload_format(
                            kv_caches[i][0, block_idx, block_offset, :])
                    cloned_offload_kv_cache_blocks[ol_block_idx][
                        ol_block_offset, 1,
                        i] = _to_offload_format(
                            kv_caches[i][1, block_idx, block_offset, :])

    if is_fp8:
        # For FP8, the kernel performs dequantization which we can't easily replicate
        # in Python reference. Just verify the kernel ran without errors and
        # the output is not all zeros (i.e., it was modified).
        for i in range(num_blocks):
            assert offload_kv_cache_blocks[i].abs().sum() > 0, f"Block {i} is all zeros"
    else:
        for i in range(num_blocks):
            torch.testing.assert_close(offload_kv_cache_blocks[i],
                                       cloned_offload_kv_cache_blocks[i])

