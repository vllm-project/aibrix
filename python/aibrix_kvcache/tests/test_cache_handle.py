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

import pytest
import torch

from aibrix_kvcache import (
    GDRKVCacheHandle,
    KVCacheBlockSpec,
    KVCacheTensorSpec,
    KVCacheBlockLayout,
)
from .conftest import make_block_tables_slot_mapping

@pytest.fixture
def block_spec():
    shape = (8, 2, 16, 2, 32)
    return KVCacheBlockSpec(
        block_ntokens=shape[2],
        block_dtype=torch.uint16,
        block_layout=KVCacheBlockLayout.LCND,
        tensor_spec=KVCacheTensorSpec(
            heads=[1, 2],
            layers=list(range(shape[0])),
            head_size=shape[-1],
        ),
    )

def test_gdr_cache_handle(block_spec):
    block_ntokens = block_spec.block_ntokens
    block_tables, slot_mapping, _ = \
        make_block_tables_slot_mapping(block_ntokens, [139])
    num_blocks = len(slot_mapping) // block_ntokens
    assert block_tables.shape[1] >= num_blocks
    assert block_tables[0][num_blocks - 1] > 0
    num_layers = len(block_spec.tensor_spec.layers)
    kvcaches = []

    expected_val = lambda block_id, layer_id, cache_type, offset: (
        block_id * num_layers * 2 * block_ntokens
        + layer_id * 2 * block_ntokens
        + cache_type * block_ntokens
        + offset
    )

    for layer_id in range(num_layers):
        kvcache = torch.zeros(
            (2, block_tables.max() + 1, *block_spec.block_shape[2:]),
            dtype=block_spec.block_dtype,
        )
        # assign expected values so we can check later
        for cache_type in range(2):
            for block_id in block_tables[0].tolist():
                for offset in range(block_ntokens):
                    kvcache[cache_type, block_id, offset] = expected_val(
                        block_id, layer_id, cache_type, offset
                    )
        kvcaches.append(kvcache)

    ntokens = 128
    handle = GDRKVCacheHandle.create(
        blocks=kvcaches,
        block_spec=block_spec,
        slot_mapping=slot_mapping[:ntokens],
    )

    mrs = handle.to_tensors()
    assert len(mrs) == num_blocks
    total_nbytes = 0
    for block_mrs in mrs:
        for mr in block_mrs:
            total_nbytes += mr.nbytes
    assert total_nbytes == num_blocks * block_spec.block_nbytes

    # check if the memory regions match with the expected layout
    for block_idx in range(ntokens // block_ntokens):
        assert len(mrs[block_idx]) == num_layers * 2  # num_layers * 2 (k & v)
        block_id = slot_mapping[block_idx * block_ntokens] // block_ntokens

        for layer_id in range(num_layers):
            kmr = mrs[block_idx][layer_id * 2]
            vmr = mrs[block_idx][layer_id * 2 + 1]
            assert kmr.shape == vmr.shape
            assert kmr.shape == block_spec.block_shape[2:]

            for offset in range(block_ntokens):
                assert (
                    int(kmr[offset].flatten()[0].item())
                    == expected_val(block_id, layer_id, 0, offset)
                )
                assert (
                    int(vmr[offset].flatten()[0].item())
                    == expected_val(block_id, layer_id, 1, offset)
                )
