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

import os

import numpy as np
import pytest
import torch

from aibrix_kvcache.cache_hashable import MemoryRegionCacheEntry, TokenCacheKey
from aibrix_kvcache.l1.eviction_policy import FIFO, LRU, S3FIFO
from aibrix_kvcache.memory import MemoryRegion, TensorPoolAllocator

from .conftest import randomize_mrs

# S3FIFO envs
os.environ["AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_TO_MAIN_PROMO_THRESHOLD"] = "1"
os.environ["AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_FIFO_CAPACITY_RATIO"] = "0.1"


TEST_ALLOC_SIZE = 128


def tensors_equal_unordered(tuple1, tuple2):
    if len(tuple1) != len(tuple2):
        return False

    # order by keys
    sorted_tuple1 = sorted(tuple1, key=lambda x: x[0])
    sorted_tuple2 = sorted(tuple2, key=lambda x: x[0])

    # compare values
    return all(
        torch.equal(a[1], b[1]) for a, b in zip(sorted_tuple1, sorted_tuple2)
    )


evicted_data = []
hot_data = []


def on_evict_callback(entry: MemoryRegionCacheEntry):
    evicted_data.append(entry)


def on_hot_access_callback(entry: MemoryRegionCacheEntry):
    hot_data.append(entry)


def build_cache_entry(
    allocator: TensorPoolAllocator, block_nbytes: int, tokens: list[int]
) -> MemoryRegionCacheEntry:
    status = allocator.alloc(TEST_ALLOC_SIZE)
    assert status.is_ok()
    mr = status.get()[0]
    check_ref_count(mr, 1)
    assert mr.length == TEST_ALLOC_SIZE
    mr.block_nbytes = block_nbytes
    randomize_mrs([mr])
    mr.pack_tokens(tokens=tokens)
    mr.seal()
    return MemoryRegionCacheEntry(mr)


def check_ref_count(mr: MemoryRegion, expected_ref_count: int):
    for entry in hot_data:
        if entry._mr == mr:
            expected_ref_count += 1
    assert mr.ref_count == expected_ref_count


@pytest.fixture(params=[FIFO, LRU, S3FIFO])
def policy(request):
    yield request.param(
        100 * TEST_ALLOC_SIZE,
        on_evict=on_evict_callback,
        on_hot_access=on_hot_access_callback,
    )  # capacity_nbytes 100 * TEST_ALLOC_SIZE

    for entry in evicted_data:
        entry.ref_down()
    evicted_data.clear()

    for entry in hot_data:
        entry.ref_down()
    hot_data.clear()


@pytest.fixture(params=[FIFO, LRU, S3FIFO])
def small_capacity_policy(request):
    return request.param(
        10 * TEST_ALLOC_SIZE,
    )  # capacity_nbytes 10 * TEST_ALLOC_SIZE


def test_put_and_get(policy):
    block_nbytes = 16
    assert len(policy) == 0
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)
    entry = build_cache_entry(allocator, block_nbytes, list(range(24)))
    mr = entry._mr
    assert policy.put(entry).is_ok()
    cache_key = entry.cache_key()
    assert policy.get(cache_key).value.data_ptr() == mr.data_ptr()
    check_ref_count(mr, 2)
    assert len(policy) == mr.length
    policy.assert_consistency()


def test_put_update_existing(policy):
    block_nbytes = 16
    assert len(policy) == 0
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)
    entry0 = build_cache_entry(allocator, block_nbytes, list(range(24)))
    entry1 = build_cache_entry(allocator, block_nbytes, list(range(24)))
    mr0 = entry0._mr
    mr1 = entry1._mr
    assert entry0 == entry1
    assert entry0.cache_key() == entry1.cache_key()
    assert policy.put(entry0).is_ok()
    assert len(policy) == mr0.length
    assert policy.put(entry1).is_ok()
    check_ref_count(mr0, 0)
    cache_key = entry1.cache_key()
    assert policy.get(cache_key).value.data_ptr() == mr1.data_ptr()
    check_ref_count(mr1, 2)
    assert len(policy) == mr1.length
    policy.assert_consistency()


def test_eviction(small_capacity_policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)
    entry0 = build_cache_entry(allocator, 16, list(range(16)))
    entry1 = build_cache_entry(allocator, 16, list(range(24)))
    assert entry0 != entry1
    assert small_capacity_policy.put(entry0).is_ok()
    assert small_capacity_policy.put(entry1).is_ok()
    assert len(small_capacity_policy) == 2 * TEST_ALLOC_SIZE
    assert small_capacity_policy.evict().is_ok()
    assert len(small_capacity_policy) == TEST_ALLOC_SIZE
    small_capacity_policy.assert_consistency()


def test_multiple_evictions(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)
    entry0 = build_cache_entry(allocator, 16, list(range(16)))
    entry1 = build_cache_entry(allocator, 16, list(range(2, 16)))
    entry2 = build_cache_entry(allocator, 16, list(range(3, 16)))
    assert policy.put(entry0).is_ok()
    assert policy.put(entry1).is_ok()
    assert policy.put(entry2).is_ok()
    assert len(policy) == 3 * TEST_ALLOC_SIZE
    assert policy.evict(2 * TEST_ALLOC_SIZE).is_ok()
    assert len(policy) == TEST_ALLOC_SIZE
    assert policy.evict(999 * TEST_ALLOC_SIZE).is_ok()
    assert len(policy) == 0
    policy.assert_consistency()


def test_delete(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)
    entry = build_cache_entry(allocator, 16, list(range(16)))
    assert policy.put(entry).is_ok()
    assert len(policy) == TEST_ALLOC_SIZE
    key = entry.cache_key()
    assert policy.delete(key).is_ok()
    assert policy.get(key).is_not_found()
    assert len(policy) == 0
    policy.assert_consistency()


def test_delete_empty(policy):
    key = TokenCacheKey(tokens=np.array(list(range(16)), dtype=np.int32))
    policy.delete(key)  # Should not raise an error
    assert len(policy) == 0
    policy.assert_consistency()


def test_basic(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)

    capacity = policy.capacity_nbytes
    ground_truth = []

    # insert data
    for i in range(capacity // TEST_ALLOC_SIZE // 2):
        entry = build_cache_entry(allocator, 64, [i])
        ground_truth.append((i, entry._mr.to_tensor()))
        policy.put(entry)

    assert tensors_equal_unordered(
        tuple(
            [
                (entry._mr.unpack_tokens()[1][0], entry._mr.to_tensor())
                for entry in policy
            ]
        ),
        tuple(ground_truth),
    )

    # test get
    for i in range(capacity // TEST_ALLOC_SIZE // 2):
        key = TokenCacheKey(tokens=np.array([i], dtype=np.int32))
        assert torch.equal(
            policy.get(key).value.to_tensor(), ground_truth[i][1]
        )

    # test eviction
    ground_truth = []
    for i in range(capacity // TEST_ALLOC_SIZE):
        idx = i + 999999
        entry = build_cache_entry(allocator, 64, [idx])
        ground_truth.append((idx, entry._mr.to_tensor()))
        policy.put(entry)
        if policy.name == "S3FIFO":
            # for S3FIFO, we need to get the data to trigger promotion
            # to main fifo when eviction happens on small fifo
            assert policy.get(entry).is_ok()

    # test new data
    for i in range(capacity // TEST_ALLOC_SIZE):
        idx = i + 999999
        key = TokenCacheKey(tokens=np.array([idx], dtype=np.int32))
        assert torch.equal(
            policy.get(key).value.to_tensor(), ground_truth[i][1]
        )
    policy.assert_consistency()


def test_on_evict_callback(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)

    capacity = policy.capacity_nbytes
    ground_truth = []
    for i in range(capacity // TEST_ALLOC_SIZE):
        entry = build_cache_entry(allocator, 64, [i])
        ground_truth.append((i, entry._mr.to_tensor()))
        policy.put(entry)

    for i in range(capacity // TEST_ALLOC_SIZE):
        idx = i + 999999
        entry = build_cache_entry(allocator, 64, [idx])
        policy.put(entry)

    assert len(ground_truth) == len(evicted_data)
    assert tensors_equal_unordered(
        tuple(ground_truth),
        tuple(
            [
                (entry._mr.unpack_tokens()[1][0], entry._mr.to_tensor())
                for entry in evicted_data
            ]
        ),
    )
    policy.assert_consistency()


def test_on_hot_access_callback(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)

    capacity = policy.capacity_nbytes
    ground_truth = []
    for i in range(capacity // TEST_ALLOC_SIZE):
        entry = build_cache_entry(allocator, 64, [i])
        policy.put(entry)
        if i % 3 == 0:
            key = TokenCacheKey(tokens=np.array([i], dtype=np.int32))
            # get multiple times to ensure no duplicate calls to
            # on_hot_access callback
            for _ in range(5):
                policy.get(key)
            ground_truth.append((i, entry._mr.to_tensor()))

    assert len(ground_truth) == len(hot_data)
    assert tensors_equal_unordered(
        tuple(ground_truth),
        tuple(
            [
                (entry._mr.unpack_tokens()[1][0], entry._mr.to_tensor())
                for entry in hot_data
            ]
        ),
    )
    policy.assert_consistency()
