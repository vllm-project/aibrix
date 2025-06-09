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
import sys
import threading
from typing import Sequence

import pytest
import redis
import torch

from aibrix_kvcache.cache_handle import KVCacheHandle
from aibrix_kvcache.memory import MemoryRegion
from aibrix_kvcache.spec import (
    KVCacheBlockLayout,
    KVCacheBlockSpec,
    KVCacheTensorSpec,
)

CACHE_SHAPE_NCLD = (16, 2, 8, 2, 32)
CACHE_SHAPE_LCND = (8, 2, 16, 2, 32)
CACHE_DTYPE = torch.bfloat16
TEMP_ROOT = os.path.join(os.path.expanduser("."), ".test_dir")


def discard_all_aibrix_envs():
    # Find all environment variables that start with "AIBRIX_"
    aibrix_keys = [key for key in os.environ if key.startswith("AIBRIX_")]

    # Remove them from the environment
    for key in aibrix_keys:
        del os.environ[key]


def get_cache_conf(layout):
    if layout == KVCacheBlockLayout.NCLD:
        shape = CACHE_SHAPE_NCLD
        return list(shape), KVCacheBlockSpec(
            block_ntokens=shape[0],
            block_dtype=CACHE_DTYPE,
            block_layout=layout,
            tensor_spec=KVCacheTensorSpec(
                heads=[1, 2],
                layers=list(range(shape[2])),
                head_size=shape[-1],
            ),
        )
    elif layout == KVCacheBlockLayout.LCND:
        shape = CACHE_SHAPE_LCND
        return list(shape), KVCacheBlockSpec(
            block_ntokens=shape[2],
            block_dtype=CACHE_DTYPE,
            block_layout=layout,
            tensor_spec=KVCacheTensorSpec(
                heads=[1, 2],
                layers=list(range(shape[0])),
                head_size=shape[-1],
            ),
        )
    return None, None


@pytest.fixture(
    params=[KVCacheBlockLayout.NCLD, KVCacheBlockLayout.LCND], scope="function"
)
def cache_conf_fixture(request):
    layout = request.param
    return get_cache_conf(layout)


def release_mrs(mrs: Sequence[MemoryRegion]):
    [mr.ref_down() for mr in mrs]


def randomize_mrs(mrs: Sequence[MemoryRegion]):
    for mr in mrs:
        # randomize
        mr.slab[mr.addr : mr.addr + mr.length].view(
            CACHE_DTYPE
        ).zero_().uniform_()


def randomize_cache_handle(handle: KVCacheHandle):
    randomize_mrs(handle.memory_regions)


@pytest.fixture
def redis_server():
    """Fixture that launches a fake Redis server for testing."""
    if sys.version_info < (3, 11):
        pytest.skip("This fixture requires Python 3.11+")

    pytest.importorskip("fakeredis")
    from fakeredis import TcpFakeServer

    server_address = ("127.0.0.1", 6379)
    TcpFakeServer.allow_reuse_address = True
    redis_server = TcpFakeServer(
        server_address=server_address, server_type="redis"
    )
    t = threading.Thread(target=redis_server.serve_forever, daemon=True)
    t.start()
    try:
        yield server_address
    finally:
        redis_server.shutdown()
        t.join()


@pytest.fixture
def redis_client(redis_server):
    """Redis client connected to the test redis server."""
    host, port = redis_server
    client = redis.Redis(
        host=host,
        port=port,
    )
    client.ping()  # Verify connection
    try:
        yield client
    finally:
        client.flushall()  # Clean up after each test


@pytest.fixture(
    params=["with_compact_layout", "without_compact_layout"], scope="function"
)
def compact_layout_enabled(request):
    import aibrix_kvcache

    origin = aibrix_kvcache.memory.allocator.MR_USE_COMPACT_LAYOUT
    if request.param == "with_compact_layout":
        aibrix_kvcache.memory.allocator.MR_USE_COMPACT_LAYOUT = True
    else:
        aibrix_kvcache.memory.allocator.MR_USE_COMPACT_LAYOUT = False
    yield request.param == "with_compact_layout"

    aibrix_kvcache.memory.allocator.MR_USE_COMPACT_LAYOUT = origin
