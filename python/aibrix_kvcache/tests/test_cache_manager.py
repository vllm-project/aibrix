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

import copy
import os
import random

import pytest
import torch

from aibrix_kvcache import (
    BaseKVCacheManager,
    KVCacheConfig,
    ModelSpec,
    cache_manager,
)
from aibrix_kvcache.memory import TensorPoolAllocator

from .conftest import discard_all_aibrix_envs, randomize_cache_handle

cache_manager.TESTING_DISABLE_PIN_MEMORY = True


@pytest.fixture(
    params=["l1", "l2_sync", "l2_async", "l1_l2_sync"], scope="function"
)
def cache_mgr_fixture(cache_conf_fixture, request):
    discard_all_aibrix_envs()

    os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB"] = "1"

    if request.param == "l1":
        # enable l1 and disable l2
        os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED"] = "1"
        os.environ["AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND"] = ""

        # let allocator use host memory
        os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_DEVICE"] = "cpu"
        os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_PIN_MEMORY"] = "0"
    elif request.param == "l2_sync":
        # enable l2 and disable l1
        os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED"] = "0"

        os.environ["AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND"] = "MOCK"
        os.environ[
            "AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_MAX_INFLIGHT_TOKENS"
        ] = "0"

    elif request.param == "l2_async":
        # enable l2 and disable l1
        os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED"] = "0"
        os.environ["AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND"] = "MOCK"

    elif request.param == "l1_l2_sync":
        # enable both l1 and l2
        os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED"] = "1"
        os.environ["AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND"] = "MOCK"

        # let allocator use host memory
        os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_DEVICE"] = "cpu"
        os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_PIN_MEMORY"] = "0"
        os.environ["AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB"] = "0.01"

        os.environ["AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_TYPE"] = "EVICTED"
        os.environ[
            "AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_MAX_INFLIGHT_TOKENS"
        ] = "0"
        # always use double get
        os.environ["AIBRIX_KV_CACHE_OL_DOUBLE_GET_THRESHOLD"] = "0"

    shape, spec = cache_conf_fixture

    cache = None
    try:
        config = KVCacheConfig(block_spec=spec, model_spec=ModelSpec(1024))
        # use a small slab size for testing
        TensorPoolAllocator.SLAB_MAX_NBYTES = spec.block_nbytes * 8
        cache = BaseKVCacheManager(config=config)
        yield shape, spec, cache, request.param
    finally:
        if cache is not None:
            print(cache.metrics.summary())
            cache.close()


def test_cache_initialization(cache_mgr_fixture):
    _, _, cache_mgr, param = cache_mgr_fixture
    if "l1" in param:
        assert cache_mgr._l1_cache is not None
    if "l2" in param:
        assert cache_mgr._l2_cache is not None


def test_put_and_get_aligned(cache_mgr_fixture):
    shape, spec, cache_mgr, param = cache_mgr_fixture
    tokens = [i for i in range(32)]
    origin_tokens = copy.deepcopy(tokens)
    status = cache_mgr.allocate_for(None, tokens)
    assert status.is_ok()
    put_handle = status.value
    randomize_cache_handle(put_handle)
    put_tensors = put_handle.to_tensors()
    put_tensors = [t.clone() for t in put_tensors]

    put_status = cache_mgr.put(None, tokens, put_handle)
    assert tokens == origin_tokens
    assert put_status.is_ok()

    if param.endswith("async"):
        cache_mgr.flush()

    get_status = cache_mgr.acquire(None, tokens)
    assert tokens == origin_tokens
    assert get_status.is_ok()
    assert get_status.value[0] == 32
    get_handle = get_status.value[1]
    assert len(put_handle) == len(
        get_handle
    ), f"len(put_handle): {len(put_handle)}, len(get_handle): {len(get_handle)}"
    get_tensors = get_handle.to_tensors()
    for pt, gt in zip(put_tensors, get_tensors):
        assert torch.equal(pt, gt)
    exists_status = cache_mgr.exists(None, tokens)
    assert exists_status.is_ok()
    assert exists_status.value == 32
    get_handle.release()


def test_put_and_get_with_prefix(cache_mgr_fixture):
    shape, spec, cache_mgr, param = cache_mgr_fixture
    tokens0 = [i for i in range(32)]
    status = cache_mgr.allocate_for(None, tokens0)
    assert status.is_ok()
    put_handle0 = status.value
    assert len(put_handle0) == 2
    randomize_cache_handle(put_handle0)
    put_tensors0 = put_handle0.to_tensors()
    put_tensors0 = [t.clone() for t in put_tensors0]

    put_status = cache_mgr.put(None, tokens0, put_handle0)
    assert put_status.is_ok()

    tokens1 = [i for i in range(100, 132)]
    status = cache_mgr.allocate_for(None, tokens1)
    assert status.is_ok()
    put_handle1 = status.value
    assert len(put_handle1) == 2
    randomize_cache_handle(put_handle1)
    put_tensors1 = put_handle1.to_tensors()
    put_tensors1 = [t.clone() for t in put_tensors1]

    put_status = cache_mgr.put(tokens0, tokens1, put_handle1)
    assert put_status.is_ok()

    if param.endswith("async"):
        cache_mgr.flush()

    get_status = cache_mgr.acquire(None, tokens0)
    assert get_status.is_ok()
    assert get_status.value[0] == 32
    get_handle0 = get_status.value[1]
    assert len(put_handle0) == len(
        get_handle0
    ), f"{len(put_handle0)} != {len(get_handle0)}"
    get_tensors0 = get_handle0.to_tensors()
    for pt, gt in zip(put_tensors0, get_tensors0):
        assert torch.equal(pt, gt)

    get_status = cache_mgr.acquire(tokens0, tokens1)
    assert get_status.is_ok()
    assert get_status.value[0] == 32
    get_handle1 = get_status.value[1]
    assert len(put_handle1) == len(
        get_handle1
    ), f"{len(put_handle1)} != {len(get_handle1)}"
    get_tensors1 = get_handle1.to_tensors()
    for pt, gt in zip(put_tensors1, get_tensors1):
        assert torch.equal(pt, gt)

    exists_status = cache_mgr.exists(tokens0, tokens1)
    assert exists_status.is_ok()
    assert exists_status.value == 32

    get_status = cache_mgr.acquire(None, tokens0 + tokens1)
    assert get_status.is_ok()
    assert get_status.value[0] == 64
    get_handle2 = get_status.value[1]
    assert len(get_handle2) == 4, f"len(get_handle2): {len(get_handle2)} != 4"
    get_tensors2 = get_handle2.to_tensors()
    for pt, gt in zip(put_tensors0 + put_tensors1, get_tensors2):
        assert torch.equal(pt, gt)
    get_handle0.release()
    get_handle1.release()
    get_handle2.release()


def test_duplicated_puts(cache_mgr_fixture):
    shape, spec, cache_mgr, param = cache_mgr_fixture
    for _ in range(10):
        tokens = [i for i in range(32)]
        status = cache_mgr.allocate_for(None, tokens)
        assert status.is_ok()
        put_handle = status.value
        randomize_cache_handle(put_handle)
        put_tensors = put_handle.to_tensors()
        put_tensors = [t.clone() for t in put_tensors]

        put_status = cache_mgr.put(None, tokens, put_handle)
        assert put_status.is_ok()

        if param.endswith("async"):
            cache_mgr.flush()

        get_status = cache_mgr.acquire(None, tokens)
        assert get_status.is_ok()
        assert get_status.value[0] == 32
        get_handle = get_status.value[1]
        assert len(put_handle) == len(
            get_handle
        ), f"{len(put_handle)} != {len(get_handle)}"
        get_tensors = get_handle.to_tensors()
        for pt, gt in zip(put_tensors, get_tensors):
            assert torch.equal(pt, gt)
        get_handle.release()


def test_delete(cache_mgr_fixture):
    shape, spec, cache_mgr, param = cache_mgr_fixture
    tokens = [i for i in range(32)]
    origin_tokens = copy.deepcopy(tokens)
    status = cache_mgr.allocate_for(None, tokens)
    assert status.is_ok()
    put_handle = status.value
    randomize_cache_handle(put_handle)
    put_tensors = put_handle.to_tensors()
    put_tensors = [t.clone() for t in put_tensors]

    put_status = cache_mgr.put(None, tokens, put_handle)
    assert tokens == origin_tokens
    assert put_status.is_ok()
    assert put_status.value == 32

    if param.endswith("async"):
        cache_mgr.flush()

    del_status = cache_mgr.delete(tokens[:16], tokens[16:])
    assert del_status.is_ok()

    get_status = cache_mgr.acquire(None, tokens[:16])
    assert get_status.is_ok()
    assert get_status.value[0] == 16
    get_handle = get_status.value[1]
    assert len(get_handle) == 1, f"len(get_handle): {len(get_handle)} != 1"
    assert torch.equal(get_handle.to_tensors()[0], put_tensors[0])
    get_handle.release()

    get_status = cache_mgr.acquire(tokens[:16], tokens[16:])
    assert get_status.is_not_found()


def test_stress_cache(cache_mgr_fixture, compact_layout_enabled):
    shape, spec, cache_mgr, param = cache_mgr_fixture
    query = {}
    for i in range(200):
        num_prefix_blocks = random.randint(0, 10)
        if num_prefix_blocks > 0:
            prefix_tokens = [j for j in range(num_prefix_blocks * 16)]
            status = cache_mgr.allocate_for(None, prefix_tokens)
            assert status.is_ok()
            prefix_handle = status.value
            randomize_cache_handle(prefix_handle)
            prefix_tokens = prefix_tokens[: len(prefix_handle) * 16]
            put_status = cache_mgr.put(None, prefix_tokens, prefix_handle)
            assert not put_status.is_invalid()
            if put_status.is_out_of_memory() or put_status.is_denied():
                continue
            assert put_status.is_ok()
            assert put_status.value >= 0 and put_status.value <= len(
                prefix_tokens
            )

            status = cache_mgr.acquire(None, prefix_tokens)
            assert status.is_ok()
            status.value[1].release()
        else:
            prefix_tokens = None

        num_token_blocks = random.randint(1, 64)
        tokens = [j for j in range(num_token_blocks * 16)]
        random.shuffle(tokens)
        status = cache_mgr.allocate_for(prefix_tokens, tokens)
        if status.is_out_of_memory():
            continue

        assert status.is_ok()
        token_handle = status.value
        randomize_cache_handle(token_handle)
        tokens = tokens[: len(token_handle) * 16]
        token_tensors = token_handle.to_tensors()
        token_tensors = [t.clone() for t in token_tensors]
        put_status = cache_mgr.put(prefix_tokens, tokens, token_handle)
        if put_status.is_out_of_memory() or put_status.is_denied():
            continue

        assert put_status.is_ok()
        assert put_status.value >= 0 and put_status.value <= len(tokens)
        status = cache_mgr.acquire(prefix_tokens, tokens)
        assert not status.is_invalid()
        if not status.is_ok():
            continue

        status.value[1].release()
        query[i] = (prefix_tokens or [], tokens, token_tensors)

    if param.endswith("async"):
        cache_mgr.flush()

    results = []
    for i in range(200):
        if i not in query:
            continue

        prefix_tokens, tokens, token_tensors = query[i]
        j = 0
        while j < len(tokens):
            length = (
                random.randint(1, (len(tokens) - j) // 16) * 16
                if len(tokens) - j > 16
                else 16
            )

            get_status = cache_mgr.acquire(
                prefix_tokens, tokens[j : j + length]
            )
            if get_status.is_ok():
                assert get_status.value[0] > 0
                num = get_status.value[0] // 16
                get_handle = get_status.value[1]
                get_tensors = get_handle.to_tensors()
                for i in range(num):
                    assert torch.equal(
                        get_tensors[i], token_tensors[j // 16 + i]
                    )
                results.append(1)
                get_handle.release()
                exists_status = cache_mgr.exists(
                    prefix_tokens, tokens[j : j + length]
                )
                assert exists_status.is_ok()
                assert exists_status.value >= num * 16
            else:
                results.append(0)
            prefix_tokens += tokens[j : j + length]
            j += length

    recorder = cache_mgr._recorder

    for reason, num in recorder.put_metrics.num_errors_by_reason.items():
        if num > 0 and reason not in ["out_of_memory", "denied", "not_found"]:
            raise AssertionError(f"PUT {reason}: {num}")

    for reason, num in recorder.get_metrics.num_errors_by_reason.items():
        if num > 0 and reason not in ["out_of_memory", "denied", "not_found"]:
            raise AssertionError(f"GET {reason}: {num}")
