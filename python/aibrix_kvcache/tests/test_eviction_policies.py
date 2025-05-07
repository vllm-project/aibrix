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

from aibrix_kvcache.l1.eviction_policy import FIFO, LRU, S3FIFO

# S3FIFO envs
os.environ["AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_TO_MAIN_PROMO_THRESHOLD"] = "1"
os.environ["AIBRIX_KV_CACHE_OL_S3FIFO_SMALL_FIFO_CAPACITY_RATIO"] = "0.1"


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


def on_evict_callback(key, value):
    evicted_data.append((key, value))


def on_hot_access_callback(key, value):
    hot_data.append((key, value))


@pytest.fixture(params=[FIFO, LRU, S3FIFO])
def policy(request):
    evicted_data.clear()
    hot_data.clear()
    return request.param(
        100,
        evict_size=2,
        on_evict=on_evict_callback,
        on_hot_access=on_hot_access_callback,
    )  # capacity 100, evict size 2


@pytest.fixture(params=[FIFO, LRU, S3FIFO])
def small_capacity_policy(request):
    return request.param(10, evict_size=1)  # capacity 10, evict size 1


def test_put_and_get(policy):
    assert len(policy) == 0
    key, value = "key1", torch.tensor([1, 2, 3])
    assert policy.put(key, value).is_ok()
    assert torch.equal(policy.get(key).value, value)
    assert len(policy) == 1
    policy.assert_consistency()


def test_put_update_existing(policy):
    assert len(policy) == 0
    key, value = "key1", torch.tensor([1, 2, 3])
    new_value = torch.tensor([9, 10, 11])
    assert policy.put(key, value).is_ok()
    assert len(policy) == 1
    assert policy.put(key, new_value).is_ok()
    assert torch.equal(policy.get(key).value, new_value)
    assert len(policy) == 1
    policy.assert_consistency()


def test_eviction(small_capacity_policy):
    key1, value1 = "key1", torch.tensor([1, 2, 3])
    key2, value2 = "key2", (torch.tensor([4, 5]), torch.tensor([6, 7]))
    assert small_capacity_policy.put(key1, value1).is_ok()
    assert small_capacity_policy.put(key2, value2).is_ok()
    assert len(small_capacity_policy) == 2
    assert small_capacity_policy.evict().is_ok()
    assert len(small_capacity_policy) == 1
    small_capacity_policy.assert_consistency()


def test_multiple_evictions(policy):
    key1, value1 = "key1", torch.tensor([1, 2, 3])
    key2, value2 = "key2", torch.tensor([4, 5, 6])
    key3, value3 = "key3", torch.tensor([7, 8, 9])
    assert policy.put(key1, value1).is_ok()
    assert policy.put(key2, value2).is_ok()
    assert policy.put(key3, value3).is_ok()
    assert len(policy) == 3
    assert policy.evict(2).is_ok()
    assert len(policy) == 1
    policy.assert_consistency()


def test_delete(policy):
    key, value = "key1", torch.tensor([1, 2, 3])
    assert policy.put(key, value).is_ok()
    assert len(policy) == 1
    assert policy.delete(key).is_ok()
    assert policy.get(key).is_not_found()
    assert len(policy) == 0
    policy.assert_consistency()


def test_delete_empty(policy):
    key = "nonexistent"
    policy.delete(key)  # Should not raise an error
    assert len(policy) == 0
    policy.assert_consistency()


def test_basic(policy):
    capacity = policy.capacity
    ground_truth = []

    # insert data
    for i in range(capacity // 2):
        policy.put(i, torch.tensor([1, 2, 3, i]))
        ground_truth.append((i, torch.tensor([1, 2, 3, i])))

    assert tensors_equal_unordered(tuple(policy.items()), tuple(ground_truth))

    # test get
    for i in range(capacity // 2):
        assert torch.equal(policy.get(i).value, torch.tensor([1, 2, 3, i]))

    # test eviction
    for i in range(capacity):
        policy.put(str(i * 10) + "a", torch.tensor([1, 2, 3, i]))
        if policy.name == "S3FIFO":
            # for S3FIFO, we need to get the data to trigger promotion
            # to main fifo when eviction happens on small fifo
            assert policy.get(str(i * 10) + "a").is_ok()

    # test new data
    for i in range(capacity):
        assert torch.equal(
            policy.get(str(i * 10) + "a").value, torch.tensor([1, 2, 3, i])
        )
    policy.assert_consistency()


def test_on_evict_callback(policy):
    capacity = policy.capacity
    ground_truth = []
    for i in range(capacity):
        policy.put(i, torch.tensor([1, 2, 3, i]))
        ground_truth.append((i, torch.tensor([1, 2, 3, i])))

    for i in range(capacity):
        policy[str(i + 1)] = torch.tensor([1, 2, 3, i * 10])

    assert len(ground_truth) == len(evicted_data)
    assert tensors_equal_unordered(tuple(ground_truth), tuple(evicted_data))
    policy.assert_consistency()


def test_on_hot_access_callback(policy):
    capacity = policy.capacity
    ground_truth = []
    for i in range(capacity):
        policy.put(i, torch.tensor([1, 2, 3, i]))
        if i % 3 == 0:
            # get multiple times to ensure no duplicate calls to
            # on_hot_access callback
            for _ in range(5):
                assert torch.equal(
                    policy.get(i).value, torch.tensor([1, 2, 3, i])
                )
            ground_truth.append((i, torch.tensor([1, 2, 3, i])))

    assert len(ground_truth) == len(hot_data)
    assert tensors_equal_unordered(tuple(ground_truth), tuple(hot_data))
    policy.assert_consistency()
