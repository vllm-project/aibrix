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
import random
from concurrent.futures import ThreadPoolExecutor
from typing import List

import numpy as np
import pytest
import torch

from aibrix_kvcache.cache_hashable import TokenCacheKey
from aibrix_kvcache.l2 import L2Cache
from aibrix_kvcache.l2.key_builders import Hasher
from aibrix_kvcache.memory import MemoryRegion, TensorPoolAllocator
from aibrix_kvcache.utils import np_array_concat

from .conftest import (
    randomize_mrs,
    release_mrs,
)


def build_get_mr(
    allocator: TensorPoolAllocator, block_nbytes: int, tokens: np.ndarray
) -> MemoryRegion:
    size = MemoryRegion.calculate_size(
        block_nbytes=block_nbytes, ntokens=len(tokens)
    )
    status = allocator.alloc(size)
    assert status.is_ok()
    mr = status.get()[0]
    assert mr.ref_count == 1
    assert mr.length == size
    return mr


def build_put_mr(
    allocator: TensorPoolAllocator,
    block_nbytes: int,
    block_ntokens: int,
    tokens: np.ndarray,
) -> MemoryRegion:
    mr = build_get_mr(allocator, block_nbytes, tokens)
    mr.block_nbytes = block_nbytes
    mr.pack_tokens(
        prefix=tokens[:-block_ntokens], tokens=tokens[-block_ntokens:]
    )
    mr.seal()
    randomize_mrs([mr])
    return mr


def build_get_mrs(
    allocator: TensorPoolAllocator,
    block_nbytes: int,
    block_ntokens: int,
    prefix: np.ndarray | None,
    tokens: np.ndarray,
) -> List[MemoryRegion]:
    prefix_len = len(prefix) if prefix is not None else 0
    assert prefix_len % block_ntokens == 0
    acc_tokens = [
        np_array_concat(prefix, tokens[: s + block_ntokens])
        for s in range(0, len(tokens), block_ntokens)
    ]
    return [
        build_get_mr(allocator, block_nbytes, tokens) for tokens in acc_tokens
    ]


def build_put_mrs(
    allocator: TensorPoolAllocator,
    block_nbytes: int,
    block_ntokens: int,
    prefix: np.ndarray | None,
    tokens: np.ndarray,
) -> List[MemoryRegion]:
    prefix_len = len(prefix) if prefix is not None else 0
    assert prefix_len % block_ntokens == 0
    acc_tokens = [
        np_array_concat(prefix, tokens[: s + block_ntokens])
        for s in range(0, len(tokens), block_ntokens)
    ]
    return [
        build_put_mr(allocator, block_nbytes, block_ntokens, tokens)
        for tokens in acc_tokens
    ]


@pytest.fixture(params=[True, False])
def l2cache_fixture(cache_conf_fixture, request, mocker):
    shape, spec = cache_conf_fixture
    mputmget_enabled = request.param

    if mputmget_enabled:
        os.environ["AIBRIX_KV_CACHE_OL_MOCK_USE_MPUT_MGET"] = "1"

    cache = None
    try:
        cache = L2Cache(
            backend_name="MOCK",
            placement_policy="SIMPLE",
            namespace="test",
            block_spec=spec,
            executor=ThreadPoolExecutor(max_workers=2),
        )
        if mputmget_enabled:
            put_func = mocker.spy(cache, "put")
            get_func = mocker.spy(cache, "get")
            mput_func = mocker.spy(cache, "_mput_impl")
            mget_func = mocker.spy(cache, "_mget_impl")

        yield shape, spec, cache

        if mputmget_enabled:
            assert mput_func.call_count == put_func.call_count
            assert mget_func.call_count == get_func.call_count
    finally:
        if cache is not None:
            cache.close()
            del cache


@pytest.fixture
def l2cache_mputmget_fixture(cache_conf_fixture):
    os.environ["AIBRIX_KV_CACHE_OL_MOCK_USE_MPUT_MGET"] = "1"
    yield l2cache_fixture(cache_conf_fixture)


@pytest.mark.asyncio
async def test_put_and_get_aligned(l2cache_fixture):
    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    capacity_nbytes = 128 * spec.block_nbytes
    allocator = TensorPoolAllocator(capacity_nbytes=capacity_nbytes)

    tokens = np.array([i for i in range(32)], dtype=np.int32)
    cache_key = TokenCacheKey(tokens=tokens)
    put_mrs = build_put_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, None, tokens
    )
    put_status = await l2cache.put(cache_key, put_mrs)
    assert put_status.is_ok()
    assert put_status.value == 2

    get_mrs = build_get_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, None, tokens
    )
    get_status = await l2cache.get(cache_key, get_mrs)
    assert get_status.is_ok()
    assert get_status.value == 2
    for i in range(len(get_mrs)):
        assert torch.equal(get_mrs[i].to_tensor(), put_mrs[i].to_tensor())
    exists_status = await l2cache.exists(cache_key)
    assert exists_status.is_ok()
    assert exists_status.value == 2
    release_mrs(put_mrs)
    release_mrs(get_mrs)


@pytest.mark.asyncio
async def test_put_and_get_with_prefix(l2cache_fixture):
    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    capacity_nbytes = 128 * spec.block_nbytes
    allocator = TensorPoolAllocator(capacity_nbytes=capacity_nbytes)

    tokens0 = np.array([i for i in range(32)], dtype=np.int32)
    cache_key0 = TokenCacheKey(tokens=tokens0)
    put_mrs0 = build_put_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, None, tokens0
    )
    put_status = await l2cache.put(cache_key0, put_mrs0)
    assert put_status.is_ok()
    assert put_status.value == 2

    tokens1 = np.array([i for i in range(100, 132)], dtype=np.int32)
    cache_key1 = TokenCacheKey(prefix=tokens0, tokens=tokens1)
    put_mrs1 = build_put_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, tokens0, tokens1
    )
    put_status = await l2cache.put(cache_key1, put_mrs1)
    assert put_status.is_ok()
    assert put_status.value == 2

    get_mrs0 = build_get_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, None, tokens0
    )
    get_status = await l2cache.get(cache_key0, get_mrs0)
    assert get_status.is_ok()
    for i in range(len(get_mrs0)):
        assert torch.equal(get_mrs0[i].to_tensor(), put_mrs0[i].to_tensor())

    get_mrs1 = build_get_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, tokens0, tokens1
    )
    get_status = await l2cache.get(cache_key1, get_mrs1)
    assert get_status.is_ok()
    for i in range(len(get_mrs1)):
        assert torch.equal(get_mrs1[i].to_tensor(), put_mrs1[i].to_tensor())

    exists_status = await l2cache.exists(cache_key1)
    assert exists_status.is_ok()
    assert exists_status.value == 2

    cache_key = TokenCacheKey(tokens=np_array_concat(tokens0, tokens1))
    get_mrs = get_mrs0 + get_mrs1
    randomize_mrs(get_mrs)
    get_status = await l2cache.get(cache_key, get_mrs)
    assert get_status.is_ok()
    put_mrs = put_mrs0 + put_mrs1
    for i in range(len(get_mrs)):
        assert torch.equal(get_mrs[i].to_tensor(), put_mrs[i].to_tensor())
    release_mrs(put_mrs)
    release_mrs(get_mrs)


@pytest.mark.asyncio
async def test_duplicated_puts(l2cache_fixture):
    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    capacity_nbytes = 128 * spec.block_nbytes
    allocator = TensorPoolAllocator(capacity_nbytes=capacity_nbytes)

    for _ in range(10):
        tokens = np.array([i for i in range(32)], dtype=np.int32)
        cache_key = TokenCacheKey(tokens=tokens)
        put_mrs = build_put_mrs(
            allocator, spec.block_nbytes, spec.block_ntokens, None, tokens
        )

        put_status = await l2cache.put(cache_key, put_mrs)
        assert put_status.is_ok()
        assert put_status.value == 2

        get_mrs = build_get_mrs(
            allocator, spec.block_nbytes, spec.block_ntokens, None, tokens
        )
        get_status = await l2cache.get(cache_key, get_mrs)
        assert get_status.is_ok()
        for i in range(len(get_mrs)):
            assert torch.equal(get_mrs[i].to_tensor(), put_mrs[i].to_tensor())
        release_mrs(put_mrs)
        release_mrs(get_mrs)


@pytest.mark.asyncio
async def test_conflicted_puts(l2cache_fixture, mocker):
    # Mock all hashers to build conflicted keys
    hashers = Hasher.__subclasses__()
    for hasher in hashers:
        mocker.patch.object(hasher, "hash", return_value=98765)

    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    capacity_nbytes = 128 * spec.block_nbytes
    allocator = TensorPoolAllocator(capacity_nbytes=capacity_nbytes)

    tokens0 = np.array([i for i in range(16)], dtype=np.int32)
    cache_key0 = TokenCacheKey(tokens=tokens0)
    put_mrs = build_put_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, None, tokens0
    )
    put_key_pairs = l2cache.key_builder.build(None, tokens0)

    put_status = await l2cache.put(cache_key0, put_mrs)
    assert put_status.is_ok()

    tokens1 = np.array([i * 2 for i in range(16)], dtype=np.int32)
    cache_key1 = TokenCacheKey(tokens=tokens1)
    get_mrs = build_get_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, None, tokens1
    )
    get_key_pairs = l2cache.key_builder.build(None, tokens1)

    # Ensure cache key used for put and get are identical
    assert len(put_key_pairs) == len(get_key_pairs)
    for i in range(len(put_key_pairs)):
        _, put_cache_key = put_key_pairs[i]
        _, get_cache_key = get_key_pairs[i]
        assert put_cache_key == get_cache_key
        assert put_cache_key.__hash__() == get_cache_key.__hash__()

    get_status = await l2cache.get(cache_key1, get_mrs)
    assert get_status.is_not_found()
    release_mrs(put_mrs)
    release_mrs(get_mrs)


@pytest.mark.asyncio
async def test_delete(l2cache_fixture):
    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    capacity_nbytes = 128 * spec.block_nbytes
    allocator = TensorPoolAllocator(capacity_nbytes=capacity_nbytes)

    tokens = np.array([i for i in range(32)], dtype=np.int32)
    cache_key = TokenCacheKey(tokens=tokens)
    put_mrs = build_put_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, None, tokens
    )

    put_status = await l2cache.put(cache_key, put_mrs)
    assert put_status.is_ok()
    assert put_status.value == 2

    delete_key = TokenCacheKey(prefix=tokens[:16], tokens=tokens[16:])
    del_status = await l2cache.delete(delete_key)
    assert del_status.is_ok()

    get_mrs = build_get_mrs(
        allocator, spec.block_nbytes, spec.block_ntokens, None, tokens
    )
    get_key = TokenCacheKey(tokens=tokens)
    get_status = await l2cache.get(get_key, get_mrs)
    assert get_status.is_ok()
    assert get_status.value == 1
    assert torch.equal(get_mrs[0].to_tensor(), put_mrs[0].to_tensor())

    get_status = await l2cache.get(delete_key, get_mrs[:1])
    assert get_status.is_not_found()
    release_mrs(put_mrs)
    release_mrs(get_mrs)


@pytest.mark.asyncio
async def test_stress_cache(l2cache_fixture):
    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    capacity_nbytes = 8192 * spec.block_nbytes
    allocator = TensorPoolAllocator(capacity_nbytes=capacity_nbytes)

    query = {}
    for i in range(200):
        num_prefix_blocks = random.randint(0, 10)
        if num_prefix_blocks > 0:
            prefix_tokens = np.array(
                [j for j in range(num_prefix_blocks * 16)], dtype=np.int32
            )
            prefix_mrs = build_put_mrs(
                allocator,
                spec.block_nbytes,
                spec.block_ntokens,
                None,
                prefix_tokens,
            )
            prefix_cache_key = TokenCacheKey(tokens=prefix_tokens)
            put_status = await l2cache.put(prefix_cache_key, prefix_mrs)
            if put_status.is_out_of_memory() or put_status.is_denied():
                release_mrs(prefix_mrs)
                continue
            assert put_status.is_ok()
            assert put_status.value >= 0 and put_status.value <= len(prefix_mrs)

            await l2cache.get(prefix_cache_key, prefix_mrs)
            release_mrs(prefix_mrs)
        else:
            prefix_tokens = None

        num_token_blocks = random.randint(1, 64)
        tokens = np.array(
            [j for j in range(num_token_blocks * 16)], dtype=np.int32
        )
        random.shuffle(tokens)
        token_mrs = build_put_mrs(
            allocator,
            spec.block_nbytes,
            spec.block_ntokens,
            prefix_tokens,
            tokens,
        )
        token_cache_key = TokenCacheKey(prefix=prefix_tokens, tokens=tokens)
        put_status = await l2cache.put(token_cache_key, token_mrs)
        if put_status.is_out_of_memory() or put_status.is_denied():
            release_mrs(token_mrs)
            continue

        assert put_status.is_ok()
        assert put_status.value >= 0 and put_status.value <= len(token_mrs)
        await l2cache.get(token_cache_key, token_mrs)
        query[i] = (prefix_tokens, tokens, token_mrs)

    results = []
    for i in range(200):
        if i not in query:
            continue

        prefix_tokens, tokens, token_mrs = query[i]
        j = 0
        while j < len(tokens):
            length = (
                random.randint(1, (len(tokens) - j) // 16) * 16
                if len(tokens) - j > 16
                else 16
            )

            mrs = build_get_mrs(
                allocator,
                spec.block_nbytes,
                spec.block_ntokens,
                prefix_tokens,
                tokens[j : j + length],
            )
            cache_key = TokenCacheKey(
                prefix=prefix_tokens, tokens=tokens[j : j + length]
            )

            get_status = await l2cache.get(cache_key, mrs)
            if get_status.is_ok():
                assert get_status.value > 0
                num = get_status.value
                for i in range(num):
                    assert torch.equal(
                        mrs[i].to_tensor(), token_mrs[j // 16 + i].to_tensor()
                    )
                results.append(1)
                exists_status = await l2cache.exists(cache_key)
                assert exists_status.is_ok()
                assert exists_status.value == num
            else:
                results.append(0)
            prefix_tokens = np_array_concat(
                prefix_tokens, tokens[j : j + length]
            )
            j += length
            release_mrs(mrs)
        release_mrs(token_mrs)

    num_oks = sum(results)
    assert num_oks > 50
