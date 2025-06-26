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


from aibrix_kvcache.memory import MemoryRegion, TensorPoolAllocator

from .conftest import randomize_mrs


def test_pack_unpack_basic(compact_layout_enabled):
    block_nbytes = 16
    max_ntokens = 57
    expected_mr_nbytes = block_nbytes if compact_layout_enabled else 256
    assert (
        MemoryRegion.calculate_size(
            block_nbytes=block_nbytes, ntokens=max_ntokens
        )
        == expected_mr_nbytes
    )

    allocator = TensorPoolAllocator(capacity_nbytes=1024)
    status = allocator.alloc(expected_mr_nbytes)
    assert status.is_ok()
    mr = status.get()[0]
    assert mr.length == expected_mr_nbytes
    mr.block_nbytes = block_nbytes
    randomize_mrs([mr])
    # mr is not packed with any tokens, should return None, []
    assert mr.unpack_tokens()[0] is None
    assert len(mr.unpack_tokens()[1]) == 0
    orig_tensor = mr.to_tensor().clone()
    orig_tokens = tuple(range(16))
    mr.pack_tokens(tokens=orig_tokens)
    mr.seal()
    prefix_from_mr, tokens_from_mr = mr.unpack_tokens()
    # check if tokens are unpacked correctly
    assert prefix_from_mr is None
    assert len(tokens_from_mr) == len(orig_tokens)
    assert tokens_from_mr == orig_tokens
    # check if data is preserved
    assert mr.to_tensor().equal(orig_tensor)

    orig_prefix = tuple(range(128, 128 + 16))
    orig_tokens = tuple(range(256, 256 + 16))
    mr.pack_tokens(prefix=orig_prefix, tokens=orig_tokens)
    mr.seal()
    prefix_from_mr, tokens_from_mr = mr.unpack_tokens()
    assert len(prefix_from_mr) == len(orig_prefix)
    assert prefix_from_mr == orig_prefix
    assert len(tokens_from_mr) == len(orig_tokens)
    assert tokens_from_mr == orig_tokens


def test_pack_unpack_max(compact_layout_enabled):
    block_nbytes = 16
    max_ntokens = 57
    expected_mr_nbytes = block_nbytes if compact_layout_enabled else 256
    assert (
        MemoryRegion.calculate_size(
            block_nbytes=block_nbytes, ntokens=max_ntokens
        )
        == expected_mr_nbytes
    )

    allocator = TensorPoolAllocator(capacity_nbytes=1024)
    status = allocator.alloc(expected_mr_nbytes)
    assert status.is_ok()
    mr = status.get()[0]
    assert mr.length == expected_mr_nbytes
    mr.block_nbytes = block_nbytes
    randomize_mrs([mr])
    # mr is not packed with any tokens, should return None, []
    assert mr.unpack_tokens()[0] is None
    assert len(mr.unpack_tokens()[1]) == 0
    orig_tensor = mr.to_tensor().clone()
    orig_tokens = tuple(range(max_ntokens))
    mr.pack_tokens(tokens=orig_tokens)
    mr.seal()
    _, tokens_from_mr = mr.unpack_tokens()
    # check if tokens are unpacked correctly
    assert len(tokens_from_mr) == len(orig_tokens)
    assert tokens_from_mr == orig_tokens
    # check if data is preserved
    assert mr.to_tensor().equal(orig_tensor)
