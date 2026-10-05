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

import random

import pytest

from aibrix_kvcache.cache_hashable import TokenListView
from aibrix_kvcache.l2.key_builders import (
    FarmHasher,
    HexKeyBuilder,
    RawKeyBuilder,
    RollingHashKeyBuilder,
    SimpleHashKeyBuilder,
)

CHUNK_SIZE = 512
BLOCK_SIZE = 16

def cache_chunk_keys(
    prefix: TokenListView | None, tokens: TokenListView, chunk_size: int,
    block_size: int,
):
    if prefix is not None:
        all = prefix + tokens
        prefix_len = len(prefix)
    else:
        all = tokens
        prefix_len = 0

    num_tokens = len(tokens)
    aligned_num_tokens = num_tokens - num_tokens % block_size
    num_chunks = -(-aligned_num_tokens // chunk_size)
    chunk_start = prefix_len
    for _ in range(num_chunks):
        chunk_end = chunk_start + chunk_size
        yield (
            all[:chunk_start],
            all[chunk_start:chunk_end],
        )
        chunk_start += chunk_size


@pytest.fixture(
    params=[
        HexKeyBuilder(BLOCK_SIZE),
        RawKeyBuilder(BLOCK_SIZE),
        RollingHashKeyBuilder(FarmHasher(), BLOCK_SIZE),
        SimpleHashKeyBuilder(FarmHasher(), BLOCK_SIZE),
    ],
    ids=[
        "HexKeyBuilder",
        "RawKeyBuilder",
        "RollingHashKeyBuilder",
        "SimpleHashKeyBuilder",
    ],
)
def key_builder(request):
    return request.param


@pytest.fixture(params=[512, 4 * 1024, 32 * 1024])
def prompt_length(request):
    return request.param

    
def bench_key_builder(key_builder, all_tokens: TokenListView):
    for chunk_prefix, chunk_tokens in cache_chunk_keys(
        None, all_tokens, CHUNK_SIZE, BLOCK_SIZE
    ):
        result = key_builder.build(chunk_prefix, chunk_tokens)
        assert len(result) > 0
        for key_tuple in result:
            assert len(key_tuple) == 2
            assert isinstance(key_tuple[0], TokenListView)
            assert isinstance(key_tuple[1], bytes)


def test_key_builder(benchmark, key_builder, prompt_length):
    random.seed(123)

    all = TokenListView(
        [random.randint(0, 99999999) for _ in range(prompt_length)]
    )

    # Run benchmark
    benchmark(bench_key_builder, key_builder, all)
