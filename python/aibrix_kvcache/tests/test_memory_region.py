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

from aibrix_kvcache import TokenListView
from aibrix_kvcache.memory import ManagedMemoryRegion, TensorPoolAllocator

from .conftest import randomize_mrs


def test_pack_unpack_basic(compact_layout_enabled):
    block_nbytes = 16
    max_ntokens = 57
    expected_mr_nbytes = block_nbytes if compact_layout_enabled else 256
    assert (
        ManagedMemoryRegion.calculate_size(
            block_nbytes=block_nbytes, ntokens=max_ntokens
        )
        == expected_mr_nbytes
    )

    allocator = TensorPoolAllocator.create(capacity_nbytes=1024)
    status = allocator.alloc(expected_mr_nbytes)
    assert status.is_ok()
    mr = status.get()[0]
    assert mr.length == expected_mr_nbytes
    mr.block_nbytes = block_nbytes
    randomize_mrs([mr])
    # mr is not packed with any tokens, should return None, None
    assert mr.unpack_tokens()[0] is None
    assert mr.unpack_tokens()[1] is None
    orig_tensor = mr.to_tensor().clone()
    orig_tokens = TokenListView(tuple(range(16)))
    mr.pack_tokens(query=orig_tokens)
    mr.seal()
    prefix_from_mr, tokens_from_mr = mr.unpack_tokens()
    # check if tokens are unpacked correctly
    assert prefix_from_mr is None
    assert len(tokens_from_mr) == len(orig_tokens)
    assert tokens_from_mr == orig_tokens
    # check if data is preserved
    assert mr.to_tensor().equal(orig_tensor)

    temp = tuple(range(128, 128 + 16)) + tuple(range(256, 256 + 16))
    all = TokenListView(temp)
    orig_prefix = all[:16]
    orig_tokens = all[16:]
    mr.pack_tokens(prefix=orig_prefix, query=orig_tokens)
    mr.seal()
    prefix_from_mr, tokens_from_mr = mr.unpack_tokens()
    assert len(prefix_from_mr) == len(orig_prefix)
    assert prefix_from_mr == orig_prefix
    assert len(tokens_from_mr) == len(orig_tokens)
    assert tokens_from_mr == orig_tokens


@pytest.mark.parametrize(
    "compact_layout_enabled", ["without_compact_layout"], indirect=True
)
def test_unpack_tokens_from_bytes(compact_layout_enabled):
    block_nbytes = 16
    mr_nbytes = 256
    allocator = TensorPoolAllocator.create(capacity_nbytes=1024)
    src = allocator.alloc(mr_nbytes).get()[0]
    dst = allocator.alloc(mr_nbytes).get()[0]
    src.block_nbytes = block_nbytes
    dst.block_nbytes = block_nbytes
    randomize_mrs([src])
    temp = tuple(range(128, 128 + 16)) + tuple(range(256, 256 + 16))
    all = TokenListView(temp)
    orig_prefix = all[:16]
    orig_tokens = all[16:]
    src.pack_tokens(prefix=orig_prefix, query=orig_tokens)
    src.seal()

    # A MR filled from bytes, e.g. by an L2 get, has no tokens cached and
    # must decode them from its own buffer.
    dst.slab[dst.addr : dst.addr + mr_nbytes].copy_(
        src.slab[src.addr : src.addr + mr_nbytes]
    )
    prefix_from_mr, tokens_from_mr = dst.unpack_tokens()
    assert prefix_from_mr == orig_prefix
    assert tokens_from_mr == orig_tokens

    # Without a prefix
    src2 = allocator.alloc(mr_nbytes).get()[0]
    dst2 = allocator.alloc(mr_nbytes).get()[0]
    src2.block_nbytes = block_nbytes
    dst2.block_nbytes = block_nbytes
    src2.pack_tokens(query=orig_tokens)
    src2.seal()
    dst2.slab[dst2.addr : dst2.addr + mr_nbytes].copy_(
        src2.slab[src2.addr : src2.addr + mr_nbytes]
    )
    prefix_from_mr, tokens_from_mr = dst2.unpack_tokens()
    assert prefix_from_mr is None
    assert tokens_from_mr == orig_tokens


def test_pack_unpack_max(compact_layout_enabled):
    block_nbytes = 16
    max_ntokens = 57
    expected_mr_nbytes = block_nbytes if compact_layout_enabled else 256
    assert (
        ManagedMemoryRegion.calculate_size(
            block_nbytes=block_nbytes, ntokens=max_ntokens
        )
        == expected_mr_nbytes
    )

    allocator = TensorPoolAllocator.create(capacity_nbytes=1024)
    status = allocator.alloc(expected_mr_nbytes)
    assert status.is_ok()
    mr = status.get()[0]
    assert mr.length == expected_mr_nbytes
    mr.block_nbytes = block_nbytes
    randomize_mrs([mr])
    # mr is not packed with any tokens, should return None, None
    assert mr.unpack_tokens()[0] is None
    assert mr.unpack_tokens()[1] is None
    orig_tensor = mr.to_tensor().clone()
    orig_tokens = TokenListView(tuple(range(max_ntokens)))
    mr.pack_tokens(query=orig_tokens)
    mr.seal()
    _, tokens_from_mr = mr.unpack_tokens()
    # check if tokens are unpacked correctly
    assert len(tokens_from_mr) == len(orig_tokens)
    assert tokens_from_mr == orig_tokens
    # check if data is preserved
    assert mr.to_tensor().equal(orig_tensor)
