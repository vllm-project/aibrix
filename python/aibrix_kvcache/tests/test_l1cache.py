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
import random

import pytest
import torch

from aibrix_kvcache.l1 import L1Cache

from .conftest import CACHE_DTYPE, get_allocator, release_mrs


def test_cache_initialization(cache_conf_fixture):
    capacity = 10
    shape, spec = cache_conf_fixture
    cache = L1Cache(
        eviction_policy="LRU",
        capacity=capacity,
        allocator=get_allocator(capacity, shape, CACHE_DTYPE),
        block_spec=spec,
    )

    assert cache.capacity == capacity
    assert cache.block_shape == tuple(shape)


def test_put_and_get_aligned(cache_conf_fixture):
    capacity = 10
    shape, spec = cache_conf_fixture

    cache = L1Cache(
        eviction_policy="LRU",
        capacity=capacity,
        allocator=get_allocator(capacity, shape, CACHE_DTYPE),
        block_spec=spec,
    )

    tokens = [i for i in range(32)]
    origin_tokens = copy.deepcopy(tokens)
    shape[spec.block_shape_token_dim] = 32
    kv_tensors = torch.randn(*shape, dtype=CACHE_DTYPE)

    put_status = cache.put(None, tokens, kv_tensors)
    assert tokens == origin_tokens
    assert put_status.is_ok()
    assert put_status.value == 2

    get_status = cache.acquire(None, tokens)
    assert tokens == origin_tokens
    assert get_status.is_ok()
    assert len(get_status.value) == 2
    mrs = get_status.value
    tensors = [mr.to_tensor(spec.block_dtype, spec.block_shape) for mr in mrs]
    cat = torch.cat(tensors, dim=spec.block_shape_token_dim)
    assert cat.shape == kv_tensors.shape
    assert torch.equal(cat, kv_tensors)
    exists_status = cache.exists(None, tokens)
    assert exists_status.is_ok()
    assert exists_status.value == 2
    release_mrs(mrs)


def test_put_and_get_unaligned(cache_conf_fixture):
    capacity = 10
    shape, spec = cache_conf_fixture

    cache = L1Cache(
        eviction_policy="LRU",
        capacity=capacity,
        allocator=get_allocator(capacity, shape, CACHE_DTYPE),
        block_spec=spec,
    )

    tokens = [i for i in range(35)]
    shape[spec.block_shape_token_dim] = len(tokens)
    kv_tensors = torch.randn(*shape, dtype=CACHE_DTYPE)

    put_status = cache.put(None, tokens, kv_tensors)
    assert put_status.is_ok()
    assert put_status.value == 2

    get_status = cache.acquire(None, tokens)
    assert get_status.is_ok()
    assert len(get_status.value) == 2
    mrs = get_status.value
    tensors = [mr.to_tensor(spec.block_dtype, spec.block_shape) for mr in mrs]
    slices = [slice(None)] * len(shape)
    slices[spec.block_shape_token_dim] = slice(0, 32)
    assert torch.equal(
        torch.cat(tensors, dim=spec.block_shape_token_dim),
        kv_tensors[tuple(slices)],
    )
    exists_status = cache.exists(None, tokens)
    assert exists_status.is_ok()
    assert exists_status.value == 2
    release_mrs(mrs)


@pytest.mark.parametrize("eviction_policy", ["FIFO", "LRU", "S3FIFO"])
def test_put_and_get_with_prefix(cache_conf_fixture, eviction_policy):
    capacity = 10
    shape, spec = cache_conf_fixture

    cache = L1Cache(
        eviction_policy=eviction_policy,
        capacity=capacity,
        allocator=get_allocator(capacity, shape, CACHE_DTYPE),
        block_spec=spec,
    )

    tokens0 = [i for i in range(32)]
    shape[spec.block_shape_token_dim] = len(tokens0)
    kv_tensors0 = torch.randn(*shape, dtype=CACHE_DTYPE)

    put_status = cache.put(None, tokens0, kv_tensors0)
    assert put_status.is_ok()
    assert put_status.value == 2

    tokens1 = [i for i in range(100, 135)]
    shape[spec.block_shape_token_dim] = len(tokens1)
    kv_tensors1 = torch.randn(*shape, dtype=CACHE_DTYPE)

    put_status = cache.put(tokens0, tokens1, kv_tensors1)
    assert put_status.is_ok()
    assert put_status.value == 2

    get_status = cache.acquire(None, tokens0)
    assert get_status.is_ok()
    mrs = get_status.value
    tensors = [mr.to_tensor(spec.block_dtype, spec.block_shape) for mr in mrs]
    assert torch.equal(
        torch.cat(tensors, dim=spec.block_shape_token_dim), kv_tensors0
    )
    release_mrs(mrs)

    get_status = cache.acquire(tokens0, tokens1)
    assert get_status.is_ok()
    mrs = get_status.value
    tensors = [mr.to_tensor(spec.block_dtype, spec.block_shape) for mr in mrs]
    slices = [slice(None)] * len(shape)
    slices[spec.block_shape_token_dim] = slice(0, 32)
    assert torch.equal(
        torch.cat(tensors, dim=spec.block_shape_token_dim),
        kv_tensors1[tuple(slices)],
    )
    exists_status = cache.exists(tokens0, tokens1)
    assert exists_status.is_ok()
    assert exists_status.value == 2
    release_mrs(mrs)

    get_status = cache.acquire(None, tokens0 + tokens1)
    assert get_status.is_ok()
    mrs = get_status.value
    tensors = [mr.to_tensor(spec.block_dtype, spec.block_shape) for mr in mrs]
    tensors0 = torch.cat(
        tensors[: len(tokens0) // 16], dim=spec.block_shape_token_dim
    )
    tensors1 = torch.cat(
        tensors[len(tokens0) // 16 : len(tokens0 + tokens1) // 16],
        dim=spec.block_shape_token_dim,
    )
    assert torch.equal(tensors0, kv_tensors0)
    assert torch.equal(tensors1, kv_tensors1[tuple(slices)])
    release_mrs(mrs)


@pytest.mark.parametrize("eviction_policy", ["FIFO", "LRU", "S3FIFO"])
def test_duplicated_puts(cache_conf_fixture, eviction_policy):
    capacity = 10
    shape, spec = cache_conf_fixture

    cache = L1Cache(
        eviction_policy=eviction_policy,
        capacity=capacity,
        allocator=get_allocator(capacity, shape, CACHE_DTYPE),
        block_spec=spec,
    )

    for _ in range(10):
        tokens = [i for i in range(32)]
        shape[spec.block_shape_token_dim] = len(tokens)
        kv_tensors = torch.randn(*shape, dtype=CACHE_DTYPE)

        put_status = cache.put(None, tokens, kv_tensors)
        assert put_status.is_ok()
        assert put_status.value == 2

        get_status = cache.acquire(None, tokens)
        assert get_status.is_ok()
        mrs = get_status.value
        tensors = [
            mr.to_tensor(spec.block_dtype, spec.block_shape) for mr in mrs
        ]
        assert torch.equal(
            torch.cat(tensors, dim=spec.block_shape_token_dim),
            kv_tensors,
        )
        assert len(cache) == 2
        release_mrs(mrs)


@pytest.mark.parametrize("eviction_policy", ["FIFO", "LRU", "S3FIFO"])
def test_cache_eviction(cache_conf_fixture, eviction_policy):
    capacity = 10
    shape, spec = cache_conf_fixture

    cache = L1Cache(
        eviction_policy=eviction_policy,
        capacity=capacity,
        allocator=get_allocator(capacity, shape, CACHE_DTYPE),
        block_spec=spec,
    )

    for i in range(0, capacity, 2):
        tokens = [i * 64 + j for j in range(32)]
        shape[spec.block_shape_token_dim] = len(tokens)
        kv_tensors = torch.randn(*shape, dtype=CACHE_DTYPE)

        put_status = cache.put(None, tokens, kv_tensors)
        assert put_status.is_ok(), f"i={i}, len(cache)={len(cache)}"
        assert (
            put_status.value
            == kv_tensors.shape[spec.block_shape_token_dim]
            // spec.block_ntokens
        )
        assert len(cache) == (i // 2 + 1) * 2

    assert len(cache) == 10
    tokens = [640 + j for j in range(32)]
    shape[spec.block_shape_token_dim] = len(tokens)
    kv_tensors = torch.randn(*shape, dtype=CACHE_DTYPE)
    put_status = cache.put(None, tokens, kv_tensors)
    assert put_status.is_ok()
    assert (
        put_status.value
        == kv_tensors.shape[spec.block_shape_token_dim] // spec.block_ntokens
    )


@pytest.mark.parametrize("eviction_policy", ["FIFO", "LRU", "S3FIFO"])
def test_stress_cache(cache_conf_fixture, eviction_policy):
    capacity = 10000
    shape, spec = cache_conf_fixture

    cache = L1Cache(
        eviction_policy=eviction_policy,
        capacity=capacity,
        allocator=get_allocator(capacity, shape, CACHE_DTYPE),
        block_spec=spec,
    )

    query = {}
    for i in range(500):
        num_prefix_blocks = random.randint(0, 10)
        prefix_tokens = [
            j for j in range(num_prefix_blocks * spec.block_ntokens)
        ]
        shape[spec.block_shape_token_dim] = len(prefix_tokens)
        prefix_kv_tensors = torch.randn(*shape, dtype=CACHE_DTYPE)
        put_status = cache.put(None, prefix_tokens, prefix_kv_tensors)
        if put_status.is_out_of_memory():
            continue

        assert put_status.is_ok()
        assert (
            put_status.value >= 0
            and put_status.value
            <= prefix_kv_tensors.shape[spec.block_shape_token_dim]
        )
        status = cache.acquire(None, prefix_tokens)
        if status.is_ok():
            release_mrs(status.value)

        ntokens = random.randint(16, 1024)
        tokens = [j for j in range(ntokens)]
        random.shuffle(tokens)
        shape[spec.block_shape_token_dim] = len(tokens)
        kv_tensors = torch.randn(*shape, dtype=CACHE_DTYPE)
        put_status = cache.put(prefix_tokens, tokens, kv_tensors)
        if put_status.is_out_of_memory():
            continue

        assert put_status.is_ok()
        assert (
            put_status.value >= 0
            and put_status.value <= kv_tensors.shape[spec.block_shape_token_dim]
        )
        status = cache.acquire(prefix_tokens, tokens)
        if status.is_ok():
            release_mrs(status.value)
        query[i] = (prefix_tokens, tokens, kv_tensors)

    # check if fragmentation ratio is acceptable
    assert len(cache) > capacity * 0.8

    results = []
    for i in range(500):
        if i not in query:
            continue

        prefix_tokens, tokens, kv_tensors = query[i]
        j = 0
        while j < len(tokens):
            length = (
                random.randint(1, (len(tokens) - j) // spec.block_ntokens)
                * spec.block_ntokens
                if len(tokens) - j > spec.block_ntokens
                else spec.block_ntokens
            )

            get_status = cache.acquire(prefix_tokens, tokens[j : j + length])
            if get_status.is_ok():
                assert len(get_status.value) > 0
                mrs = get_status.value
                tensors = [
                    mr.to_tensor(spec.block_dtype, spec.block_shape)
                    for mr in mrs
                ]
                slices = [slice(None)] * len(shape)
                slices[spec.block_shape_token_dim] = slice(
                    j, j + len(get_status.value) * spec.block_ntokens
                )
                assert torch.equal(
                    torch.cat(tensors, dim=spec.block_shape_token_dim),
                    kv_tensors[tuple(slices)],
                )
                release_mrs(mrs)
                results.append(1)
                exists_status = cache.exists(
                    prefix_tokens, tokens[j : j + length]
                )
                assert exists_status.is_ok()
                assert exists_status.value == len(get_status.value)
            else:
                results.append(0)
            prefix_tokens += tokens[j : j + length]
            j += length

    num_oks = sum(results)
    assert num_oks > 250
