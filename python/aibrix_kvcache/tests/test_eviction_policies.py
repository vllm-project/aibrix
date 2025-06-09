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

import pytest
import torch

from aibrix_kvcache.cache_hashable import TokenCacheKey
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


def on_evict_callback(key: TokenCacheKey, value: MemoryRegion):
    evicted_data.append((key, value))


def on_hot_access_callback(key: TokenCacheKey, value: MemoryRegion):
    hot_data.append((key, value))


def build_cache_value(
    allocator: TensorPoolAllocator,
    block_nbytes: int,
    key: TokenCacheKey,
) -> MemoryRegion:
    status = allocator.alloc(TEST_ALLOC_SIZE)
    assert status.is_ok()
    mr = status.get()[0]
    check_ref_count(mr, 1)
    assert mr.length == TEST_ALLOC_SIZE
    mr.block_nbytes = block_nbytes
    randomize_mrs([mr])
    mr.pack_tokens(prefix=key.prefix, tokens=key.tokens)
    mr.seal()
    return mr


def check_ref_count(mr: MemoryRegion, expected_ref_count: int):
    for _, hot_mr in hot_data:
        if hot_mr == mr:
            expected_ref_count += 1
    assert mr.ref_count == expected_ref_count


@pytest.fixture(params=[FIFO, LRU, S3FIFO])
def policy(request):
    yield request.param(
        100 * TEST_ALLOC_SIZE,
        on_evict=on_evict_callback,
        on_hot_access=on_hot_access_callback,
    )  # capacity_nbytes 100 * TEST_ALLOC_SIZE

    for _, mr in evicted_data:
        mr.ref_down()
    evicted_data.clear()

    for _, mr in hot_data:
        mr.ref_down()
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
    key = TokenCacheKey(None, tuple(range(24)))
    mr = build_cache_value(allocator, block_nbytes, key)
    assert policy.put(key, mr).is_ok()
    assert policy.get(key).value.data_ptr() == mr.data_ptr()
    check_ref_count(mr, 2)
    assert len(policy) == mr.length
    policy.assert_consistency()


def test_put_update_existing(policy):
    block_nbytes = 16
    assert len(policy) == 0
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)
    key = TokenCacheKey(None, tuple(range(24)))
    mr0 = build_cache_value(allocator, block_nbytes, key)
    mr1 = build_cache_value(allocator, block_nbytes, key)
    assert policy.put(key, mr0).is_ok()
    assert len(policy) == mr0.length
    assert policy.put(key, mr1).is_ok()
    check_ref_count(mr0, 0)
    assert policy.get(key).value.data_ptr() == mr1.data_ptr()
    check_ref_count(mr1, 2)
    assert len(policy) == mr1.length
    policy.assert_consistency()


def test_eviction(small_capacity_policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)
    key0 = TokenCacheKey(None, tuple(range(16)))
    mr0 = build_cache_value(allocator, 16, key0)
    key1 = TokenCacheKey(None, tuple(range(24)))
    mr1 = build_cache_value(allocator, 16, key1)
    assert small_capacity_policy.put(key0, mr0).is_ok()
    assert small_capacity_policy.put(key1, mr1).is_ok()
    assert len(small_capacity_policy) == 2 * TEST_ALLOC_SIZE
    assert small_capacity_policy.evict().is_ok()
    assert len(small_capacity_policy) == TEST_ALLOC_SIZE
    small_capacity_policy.assert_consistency()


def test_multiple_evictions(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)
    key0 = TokenCacheKey(None, tuple(range(16)))
    key1 = TokenCacheKey(None, tuple(range(2, 16)))
    key2 = TokenCacheKey(None, tuple(range(3, 16)))
    mr0 = build_cache_value(allocator, 16, key0)
    mr1 = build_cache_value(allocator, 16, key1)
    mr2 = build_cache_value(allocator, 16, key2)
    assert policy.put(key0, mr0).is_ok()
    assert policy.put(key1, mr1).is_ok()
    assert policy.put(key2, mr2).is_ok()
    assert len(policy) == 3 * TEST_ALLOC_SIZE
    assert policy.evict(2 * TEST_ALLOC_SIZE).is_ok()
    assert len(policy) == TEST_ALLOC_SIZE
    assert policy.evict(999 * TEST_ALLOC_SIZE).is_ok()
    assert len(policy) == 0
    policy.assert_consistency()


def test_delete(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)
    key = TokenCacheKey(None, tuple(range(16)))
    mr = build_cache_value(allocator, 16, key)
    assert policy.put(key, mr).is_ok()
    assert len(policy) == TEST_ALLOC_SIZE
    assert policy.delete(key).is_ok()
    assert policy.get(key).is_not_found()
    assert len(policy) == 0
    policy.assert_consistency()


def test_delete_empty(policy):
    key = TokenCacheKey(None, tuple(range(16)))
    policy.delete(key)  # Should not raise an error
    assert len(policy) == 0
    policy.assert_consistency()


def test_basic(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)

    capacity = policy.capacity_nbytes
    ground_truth = []

    # insert data
    for i in range(capacity // TEST_ALLOC_SIZE // 2):
        key = TokenCacheKey(None, (i,))
        mr = build_cache_value(allocator, 64, key)
        ground_truth.append((i, mr.to_tensor()))
        policy.put(key, mr)

    assert tensors_equal_unordered(
        tuple(
            [
                (mr.unpack_tokens()[1][0], mr.to_tensor())
                for mr in policy.values()
            ]
        ),
        tuple(ground_truth),
    )

    # test get
    for i in range(capacity // TEST_ALLOC_SIZE // 2):
        key = TokenCacheKey(None, (i,))
        assert torch.equal(
            policy.get(key).value.to_tensor(), ground_truth[i][1]
        )

    # test eviction
    ground_truth = []
    for i in range(capacity // TEST_ALLOC_SIZE):
        idx = i + 999999
        key = TokenCacheKey(None, (idx,))
        mr = build_cache_value(allocator, 64, key)
        ground_truth.append((idx, mr.to_tensor()))
        policy.put(key, mr)
        if policy.name == "S3FIFO":
            # for S3FIFO, we need to get the data to trigger promotion
            # to main fifo when eviction happens on small fifo
            assert policy.get(key).is_ok()

    # test new data
    for i in range(capacity // TEST_ALLOC_SIZE):
        idx = i + 999999
        key = TokenCacheKey(None, (idx,))
        assert torch.equal(
            policy.get(key).value.to_tensor(), ground_truth[i][1]
        )
    policy.assert_consistency()


def test_on_evict_callback(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)

    capacity = policy.capacity_nbytes
    ground_truth = []
    for i in range(capacity // TEST_ALLOC_SIZE):
        key = TokenCacheKey(None, (i,))
        mr = build_cache_value(allocator, 64, key)
        ground_truth.append((i, mr.to_tensor()))
        policy.put(key, mr)

    for i in range(capacity // TEST_ALLOC_SIZE):
        idx = i + 999999
        key = TokenCacheKey(None, (idx,))
        mr = build_cache_value(allocator, 64, key)
        policy.put(key, mr)

    assert len(ground_truth) == len(evicted_data)
    assert tensors_equal_unordered(
        tuple(ground_truth),
        tuple(
            [
                (mr.unpack_tokens()[1][0], mr.to_tensor())
                for _, mr in evicted_data
            ]
        ),
    )
    policy.assert_consistency()


def test_on_hot_access_callback(policy):
    allocator = TensorPoolAllocator(capacity_nbytes=256 * TEST_ALLOC_SIZE)

    capacity = policy.capacity_nbytes
    ground_truth = []
    for i in range(capacity // TEST_ALLOC_SIZE):
        key = TokenCacheKey(None, (i,))
        mr = build_cache_value(allocator, 64, key)
        policy.put(key, mr)
        if i % 3 == 0:
            key = TokenCacheKey(prefix=None, tokens=(i,))
            # get multiple times to ensure no duplicate calls to
            # on_hot_access callback
            for _ in range(5):
                policy.get(key)
            ground_truth.append((i, mr.to_tensor()))

    assert len(ground_truth) == len(hot_data)
    assert tensors_equal_unordered(
        tuple(ground_truth),
        tuple(
            [(mr.unpack_tokens()[1][0], mr.to_tensor()) for _, mr in hot_data]
        ),
    )
    policy.assert_consistency()
