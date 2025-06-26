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

import numpy as np
import pytest

from aibrix_kvcache.l2.key_builders import (
    FarmHasher,
    RollingHashKeyBuilder,
    SimpleHashKeyBuilder,
)

# Fixed tokens length
TOKENS_LENGTH = 512
BLOCK_SIZE = 16


@pytest.fixture(
    params=[
        RollingHashKeyBuilder(FarmHasher(), BLOCK_SIZE),
        SimpleHashKeyBuilder(FarmHasher(), BLOCK_SIZE),
    ]
)
def key_builder(request):
    return request.param


@pytest.fixture(params=[512, 4 * 1024, 32 * 1024])
def prefix_length(request):
    return request.param


def test_key_builder(benchmark, key_builder, prefix_length):
    random.seed(123)

    prefix = np.array(
        [random.randint(0, 99999999) for _ in range(prefix_length)],
        dtype=np.int32,
    )
    tokens = np.array(
        [random.randint(0, 99999999) for _ in range(TOKENS_LENGTH)],
        dtype=np.int32,
    )

    # Run benchmark
    benchmark(key_builder.build, prefix, tokens)

    result = key_builder.build(prefix, tokens)
    assert len(result) > 0
    for key_tuple in result:
        assert len(key_tuple) == 2
        assert isinstance(key_tuple[0], np.ndarray)
        assert isinstance(key_tuple[1], (str, bytes))
