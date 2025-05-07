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
import shutil
from concurrent.futures import ThreadPoolExecutor

import pytest
import torch

from aibrix_kvcache.l2 import L2Cache

from .conftest import (
    CACHE_DTYPE,
    TEMP_ROOT,
    get_allocator,
    randomize_mrs,
    release_mrs,
)

# rocksdb envs
os.environ["AIBRIX_KV_CACHE_OL_ROCKSDB_ROOT"] = TEMP_ROOT


@pytest.fixture
def l2cache_fixture(cache_conf_fixture):
    if os.path.exists(TEMP_ROOT):
        shutil.rmtree(TEMP_ROOT, ignore_errors=True)

    shape, spec = cache_conf_fixture

    cache = None
    try:
        cache = L2Cache(
            backend_name="ROCKSDB",
            placement_policy="SIMPLE",
            namespace="test",
            block_spec=spec,
            executor=ThreadPoolExecutor(max_workers=2),
        )
        yield shape, spec, cache
    finally:
        if cache is not None:
            cache.close()
            del cache
        if os.path.exists(TEMP_ROOT):
            shutil.rmtree(TEMP_ROOT, ignore_errors=True)


@pytest.mark.asyncio
async def test_put_and_get_aligned(l2cache_fixture):
    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    allocator = get_allocator(16, shape, CACHE_DTYPE)

    tokens = [i for i in range(32)]
    origin_tokens = copy.deepcopy(tokens)
    status = allocator.alloc(2 * allocator.mr_nbytes)
    assert status.is_ok()
    put_mrs = status.value
    randomize_mrs(put_mrs)

    put_status = await l2cache.put(None, tokens, put_mrs)
    assert tokens == origin_tokens
    assert put_status.is_ok()
    assert put_status.value == 2

    status = allocator.alloc(2 * allocator.mr_nbytes)
    assert status.is_ok()
    get_mrs = status.value
    get_status = await l2cache.get(None, tokens, get_mrs)
    assert tokens == origin_tokens
    assert get_status.is_ok()
    assert get_status.value == 2
    for i in range(len(get_mrs)):
        assert torch.equal(get_mrs[i].to_tensor(), put_mrs[i].to_tensor())
    exists_status = await l2cache.exists(None, tokens)
    assert exists_status.is_ok()
    assert exists_status.value == 2
    release_mrs(put_mrs)
    release_mrs(get_mrs)


@pytest.mark.asyncio
async def test_put_and_get_with_prefix(l2cache_fixture):
    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    allocator = get_allocator(16, shape, CACHE_DTYPE)

    tokens0 = [i for i in range(32)]
    status = allocator.alloc(2 * allocator.mr_nbytes)
    assert status.is_ok()
    put_mrs0 = status.value
    randomize_mrs(put_mrs0)

    put_status = await l2cache.put(None, tokens0, put_mrs0)
    assert put_status.is_ok()
    assert put_status.value == 2

    tokens1 = [i for i in range(100, 132)]
    status = allocator.alloc(2 * allocator.mr_nbytes)
    assert status.is_ok()
    put_mrs1 = status.value
    randomize_mrs(put_mrs1)

    put_status = await l2cache.put(tokens0, tokens1, put_mrs1)
    assert put_status.is_ok()
    assert put_status.value == 2

    status = allocator.alloc(2 * allocator.mr_nbytes)
    assert status.is_ok()
    get_mrs0 = status.value
    get_status = await l2cache.get(None, tokens0, get_mrs0)
    assert get_status.is_ok()
    for i in range(len(get_mrs0)):
        assert torch.equal(get_mrs0[i].to_tensor(), put_mrs0[i].to_tensor())

    status = allocator.alloc(2 * allocator.mr_nbytes)
    assert status.is_ok()
    get_mrs1 = status.value
    get_status = await l2cache.get(tokens0, tokens1, get_mrs1)
    assert get_status.is_ok()
    for i in range(len(get_mrs1)):
        assert torch.equal(get_mrs1[i].to_tensor(), put_mrs1[i].to_tensor())

    exists_status = await l2cache.exists(tokens0, tokens1)
    assert exists_status.is_ok()
    assert exists_status.value == 2

    get_mrs = get_mrs0 + get_mrs1
    randomize_mrs(get_mrs)
    get_status = await l2cache.get(None, tokens0 + tokens1, get_mrs)
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

    allocator = get_allocator(16, shape, CACHE_DTYPE)

    for _ in range(10):
        tokens = [i for i in range(32)]
        status = allocator.alloc(2 * allocator.mr_nbytes)
        assert status.is_ok()
        put_mrs = status.value
        randomize_mrs(put_mrs)

        put_status = await l2cache.put(None, tokens, put_mrs)
        assert put_status.is_ok()
        assert put_status.value == 2

        status = allocator.alloc(2 * allocator.mr_nbytes)
        assert status.is_ok()
        get_mrs = status.value
        randomize_mrs(get_mrs)
        get_status = await l2cache.get(None, tokens, get_mrs)
        assert get_status.is_ok()
        for i in range(len(get_mrs)):
            assert torch.equal(get_mrs[i].to_tensor(), put_mrs[i].to_tensor())
        release_mrs(put_mrs)
        release_mrs(get_mrs)


@pytest.mark.asyncio
async def test_delete(l2cache_fixture):
    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    allocator = get_allocator(16, shape, CACHE_DTYPE)

    tokens = [i for i in range(32)]
    origin_tokens = copy.deepcopy(tokens)
    status = allocator.alloc(2 * allocator.mr_nbytes)
    assert status.is_ok()
    put_mrs = status.value
    randomize_mrs(put_mrs)

    put_status = await l2cache.put(None, tokens, put_mrs)
    assert tokens == origin_tokens
    assert put_status.is_ok()
    assert put_status.value == 2

    del_status = await l2cache.delete(tokens[:16], tokens[16:])
    assert del_status.is_ok()

    status = allocator.alloc(2 * allocator.mr_nbytes)
    assert status.is_ok()
    get_mrs = status.value
    randomize_mrs(get_mrs)
    get_status = await l2cache.get(None, tokens[:16], get_mrs[:1])
    assert get_status.is_ok()
    assert get_status.value == 1
    assert torch.equal(get_mrs[0].to_tensor(), put_mrs[0].to_tensor())

    get_status = await l2cache.get(tokens[:16], tokens[16:], get_mrs[:1])
    assert get_status.is_not_found()
    release_mrs(put_mrs)
    release_mrs(get_mrs)


@pytest.mark.asyncio
async def test_stress_cache(l2cache_fixture):
    shape, spec, l2cache = l2cache_fixture
    open_status = l2cache.open()
    open_status.raise_if_has_exception()

    allocator = get_allocator(10240, shape, CACHE_DTYPE)

    query = {}
    for i in range(200):
        num_prefix_blocks = random.randint(0, 10)
        if num_prefix_blocks > 0:
            prefix_tokens = [j for j in range(num_prefix_blocks * 16)]
            status = allocator.alloc(num_prefix_blocks * allocator.mr_nbytes)
            assert status.is_ok()
            prefix_mrs = status.value
            randomize_mrs(prefix_mrs)
            put_status = await l2cache.put(None, prefix_tokens, prefix_mrs)
            if put_status.is_out_of_memory() or put_status.is_denied():
                release_mrs(prefix_mrs)
                continue
            assert put_status.is_ok()
            assert put_status.value >= 0 and put_status.value <= len(prefix_mrs)

            await l2cache.get(None, prefix_tokens, prefix_mrs)
            release_mrs(prefix_mrs)
        else:
            prefix_tokens = None

        num_token_blocks = random.randint(1, 64)
        tokens = [j for j in range(num_token_blocks * 16)]
        random.shuffle(tokens)
        status = allocator.alloc(num_token_blocks * allocator.mr_nbytes)
        assert status.is_ok()
        token_mrs = status.value
        randomize_mrs(token_mrs)
        put_status = await l2cache.put(prefix_tokens, tokens, token_mrs)
        if put_status.is_out_of_memory() or put_status.is_denied():
            release_mrs(token_mrs)
            continue

        assert put_status.is_ok()
        assert put_status.value >= 0 and put_status.value <= len(token_mrs)
        await l2cache.get(prefix_tokens, tokens, token_mrs)
        query[i] = (prefix_tokens or [], tokens, token_mrs)

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
            status = allocator.alloc(length // 16 * allocator.mr_nbytes)
            assert status.is_ok()
            mrs = status.value
            randomize_mrs(mrs)

            get_status = await l2cache.get(
                prefix_tokens, tokens[j : j + length], mrs
            )
            if get_status.is_ok():
                assert get_status.value > 0
                num = get_status.value
                for i in range(num):
                    assert torch.equal(
                        mrs[i].to_tensor(), token_mrs[j // 16 + i].to_tensor()
                    )
                results.append(1)
                exists_status = await l2cache.exists(
                    prefix_tokens, tokens[j : j + length]
                )
                assert exists_status.is_ok()
                assert exists_status.value == num
            else:
                results.append(0)
            prefix_tokens += tokens[j : j + length]
            j += length
            release_mrs(mrs)
        release_mrs(token_mrs)

    num_oks = sum(results)
    assert num_oks > 50
