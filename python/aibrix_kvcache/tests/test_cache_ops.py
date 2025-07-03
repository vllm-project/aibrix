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

import random

import pytest
import torch
import vllm
from aibrix_kvcache import _custom_ops as ops

from vllm.platforms import current_platform
from vllm.utils import get_kv_cache_torch_dtype

pytest.skip(allow_module_level=True)

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
@pytest.mark.parametrize("kv_cache_dtype", ["auto"])
@pytest.mark.parametrize("offload_layout", ["LCND", "NCLD"])
@torch.inference_mode()
def test_reshape_and_cache_multi_layer(
    kv_cache_factory_flashinfer,
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
    current_platform.seed_everything(seed)
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
    if offload_layout == "LCND":
        for _ in range(num_blocks):
            offload_kv_cache_block = torch.randn(
                num_layers,
                2,
                block_size,
                embed_dim,
                dtype=get_kv_cache_torch_dtype(kv_cache_dtype, dtype),
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
                dtype=get_kv_cache_torch_dtype(kv_cache_dtype, dtype),
                device="cpu",
                pin_memory=True,
            )
            offload_kv_cache_blocks.append(offload_kv_cache_block)

    # Create the KV caches.
    key_caches, value_caches = kv_cache_factory_flashinfer(
        num_blocks,
        block_size,
        num_layers,
        num_heads,
        head_size,
        kv_cache_dtype,
        dtype,
        device=device,
    )
    kv_caches = [
        torch.stack((key_caches[i], value_caches[i]))
        for i in range(num_layers)
    ]

    scale = max(block.amax() for block in offload_kv_cache_blocks)
    scale = (scale / 64.0).to(torch.float32).to(device)
    scales = [scale] * num_layers

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
    for i in range(num_layers):
        for j in range(num_tokens):
            block_idx = block_indicies_lst[j]
            block_offset = block_offsets_lst[j]
            ol_block_idx = j // block_size
            ol_block_offset = j % block_size
            if offload_layout == "LCND":
                cloned_kv_caches[i][0, block_idx, block_offset, :] = (
                    offload_kv_cache_blocks[ol_block_idx][
                        i, 0, ol_block_offset].view(num_heads, head_size))
                cloned_kv_caches[i][1, block_idx, block_offset, :] = (
                    offload_kv_cache_blocks[ol_block_idx][
                        i, 1, ol_block_offset].view(num_heads, head_size))
            else:
                cloned_kv_caches[i][0, block_idx, block_offset, :] = (
                    offload_kv_cache_blocks[ol_block_idx][ol_block_offset, 0,
                                                          i].view(
                                                              num_heads,
                                                              head_size))
                cloned_kv_caches[i][1, block_idx, block_offset, :] = (
                    offload_kv_cache_blocks[ol_block_idx][ol_block_offset, 1,
                                                          i].view(
                                                              num_heads,
                                                              head_size))

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
@pytest.mark.parametrize("kv_cache_dtype", ["auto"])
@pytest.mark.parametrize("offload_layout", ["LCND", "NCLD"])
@torch.inference_mode()
def test_reshape_and_offload_multi_layer(
    kv_cache_factory_flashinfer,
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
    current_platform.seed_everything(seed)
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
    if offload_layout == "LCND":
        for _ in range(num_blocks):
            offload_kv_cache_blocks.append(
                torch.randn(
                    num_layers,
                    2,
                    block_size,
                    embed_dim,
                    dtype=get_kv_cache_torch_dtype(kv_cache_dtype, dtype),
                    device="cpu",
                    pin_memory=True,
                ))
    else:  # offload_layout == "NCLD"
        for _ in range(num_blocks):
            offload_kv_cache_blocks.append(
                torch.randn(
                    block_size,
                    2,
                    num_layers,
                    embed_dim,
                    dtype=get_kv_cache_torch_dtype(kv_cache_dtype, dtype),
                    device="cpu",
                    pin_memory=True,
                ))

    # Create the KV caches.
    key_caches, value_caches = kv_cache_factory_flashinfer(
        num_blocks,
        block_size,
        num_layers,
        num_heads,
        head_size,
        kv_cache_dtype,
        dtype,
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
    for i in range(num_layers):
        for j in range(num_tokens):
            block_idx = block_indicies_lst[j]
            block_offset = block_offsets_lst[j]
            ol_block_idx = j // block_size
            ol_block_offset = j % block_size
            if offload_layout == "LCND":
                cloned_offload_kv_cache_blocks[ol_block_idx][
                    i, 0, ol_block_offset] = kv_caches[i][
                        0, block_idx, block_offset, :].view(embed_dim)
                cloned_offload_kv_cache_blocks[ol_block_idx][
                    i, 1, ol_block_offset] = kv_caches[i][
                        1, block_idx, block_offset, :].view(embed_dim)
            else:
                cloned_offload_kv_cache_blocks[ol_block_idx][
                    ol_block_offset, 0,
                    i] = kv_caches[i][0, block_idx,
                                      block_offset, :].view(embed_dim)
                cloned_offload_kv_cache_blocks[ol_block_idx][
                    ol_block_offset, 1,
                    i] = kv_caches[i][1, block_idx,
                                      block_offset, :].view(embed_dim)

    for i in range(num_blocks):
        torch.testing.assert_close(offload_kv_cache_blocks[i],
                                   cloned_offload_kv_cache_blocks[i])

