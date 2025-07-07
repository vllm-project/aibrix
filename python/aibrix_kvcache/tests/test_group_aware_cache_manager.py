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
from contextlib import contextmanager
from typing import Any, List

import pytest
import torch
import torch.distributed as dist
import torch.multiprocessing as mp
from tqdm import tqdm

from aibrix_kvcache import (
    GroupAwareKVCacheManager,
    KVCacheBlockLayout,
    KVCacheConfig,
    ModelSpec,
    cache_manager,
)

from .conftest import (
    discard_all_aibrix_envs,
    get_cache_conf,
    randomize_cache_handle,
)

pytest.skip(allow_module_level=True)
cache_manager.TESTING_DISABLE_PIN_MEMORY = True


@pytest.fixture(
    params=["l1", "l2_sync", "l2_async", "l1_l2_sync"], scope="function"
)
def envs(request):
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

    yield request.param


# dist utils
def dist_run(func, envs_name, world_size, layout):
    os.environ["MASTER_ADDR"] = "localhost"
    os.environ["MASTER_PORT"] = "12345"
    processes: List[mp.Process] = []
    for i in range(world_size):
        p = mp.Process(target=func, args=(envs_name, i, world_size, layout))
        processes.append(p)
        p.start()

    for p in processes:
        p.join()

    for p in processes:
        assert p.exitcode == 0


@contextmanager
def process_group(rank: int, world_size: int):
    dist.init_process_group("gloo", rank=rank, world_size=world_size)
    dist.barrier()
    yield
    dist.barrier()
    dist.destroy_process_group()


# cache utils
def my_get_cache_conf(rank: int, world_size: int, layout: KVCacheBlockLayout):
    heads = list(range(world_size * 4))

    shape, spec = get_cache_conf(layout)
    spec.tensor_spec.heads = heads[rank * 4 : (rank + 1) * 4]
    return shape, spec


@contextmanager
def cache_conf(rank: int, world_size: int, layout: KVCacheBlockLayout):
    cache = None
    try:
        shape, spec = my_get_cache_conf(rank, world_size, layout)
        config = KVCacheConfig(block_spec=spec, model_spec=ModelSpec(1024))
        cache = GroupAwareKVCacheManager(
            config=config, process_group=dist.group.WORLD
        )
        yield shape, spec, cache
    finally:
        if cache is not None:
            cache.close()


def _test_put_and_get_aligned(
    envs_name: str, rank: int, world_size: int, layout: KVCacheBlockLayout
):
    with process_group(rank, world_size), cache_conf(
        rank, world_size, layout
    ) as cache_config:
        shape, spec, cache = cache_config
        tokens = [i for i in range(32)]
        origin_tokens = copy.deepcopy(tokens)
        status = cache.allocate_for(None, tokens)
        assert status.is_ok()
        put_handle = status.value
        randomize_cache_handle(put_handle)
        put_tensors = put_handle.to_tensors()
        put_tensors = [t.clone() for t in put_tensors]

        put_status = cache.put(None, tokens, put_handle)
        assert tokens == origin_tokens, f"{tokens}!= {origin_tokens}"
        assert put_status.is_ok(), f"{put_status}"
        assert put_status.value == len(
            tokens
        ), f"{put_status.value}!= {len(tokens)}"

        if envs_name.endswith("async"):
            cache.flush()

        get_status = cache.acquire(None, tokens)
        assert tokens == origin_tokens, f"{tokens}!= {origin_tokens}"
        assert get_status.is_ok(), f"{get_status}"
        assert get_status.value[0] == 32, f"{get_status.value[0]}!= 32"
        get_handle = get_status.value[1]
        assert len(put_handle) == len(
            get_handle
        ), f"{len(put_handle)} != {len(get_handle)}"
        get_tensors = get_handle.to_tensors()
        for pt, gt in zip(put_tensors, get_tensors):
            assert torch.equal(pt, gt)
        get_handle.release()


@pytest.mark.parametrize(
    "layout", [KVCacheBlockLayout.NCLD, KVCacheBlockLayout.LCND]
)
def test_put_and_get_aligned(envs, layout):
    dist_run(_test_put_and_get_aligned, envs, 8, layout)


def _test_stress_cache(
    envs_name: str, rank: int, world_size: int, layout: KVCacheBlockLayout
):
    def _bcast_object(obj: Any) -> Any:
        obj_list = [obj]
        dist.broadcast_object_list(obj_list, src=0, group=dist.group.WORLD)
        return obj_list[0]

    with process_group(rank, world_size), cache_conf(
        rank, world_size, layout
    ) as cache_config:
        shape, spec, cache = cache_config
        random.seed(rank)
        query = {}
        for i in tqdm(range(200), desc="putting cache"):
            num_prefix_blocks = random.randint(0, 10)
            num_prefix_blocks = _bcast_object(num_prefix_blocks)
            if num_prefix_blocks > 0:
                prefix_tokens = [j for j in range(num_prefix_blocks * 16)]
                prefix_tokens = _bcast_object(prefix_tokens)
                status = cache.allocate_for(None, prefix_tokens)
                assert status.is_ok()
                put_handle = status.value
                randomize_cache_handle(put_handle)
                cache.put(None, prefix_tokens, put_handle)

            else:
                prefix_tokens = None

            num_token_blocks = random.randint(16, 256)
            ntokens = num_token_blocks * 16
            ntokens = _bcast_object(ntokens)
            tokens = [j for j in range(ntokens)]
            random.shuffle(tokens)
            tokens = _bcast_object(tokens)
            status = cache.allocate_for(prefix_tokens, tokens)
            assert status.is_ok()
            token_handle = status.value
            randomize_cache_handle(token_handle)
            token_tensors = token_handle.to_tensors()
            token_tensors = [t.clone() for t in token_tensors]
            tokens = tokens[: len(token_handle) * 16]
            cache.put(prefix_tokens, tokens, token_handle)
            query[i] = (prefix_tokens or [], tokens, token_tensors)

        if envs_name.endswith("async"):
            cache.flush()

        results = []
        for i in tqdm(range(200), desc="getting cache"):
            prefix_tokens, tokens, token_tensors = query[i]

            if len(tokens) > 128:
                ntokens_to_del = random.randint(128, len(tokens))
                ntokens_left = (len(tokens) - ntokens_to_del) // 16 * 16
                # we delete some portion of tokens on different ranks to mimic
                # the scenario of different ranks have different cache hits
                del_status = cache.delete(
                    prefix_tokens + tokens[:ntokens_left], tokens[ntokens_left:]
                )
                assert del_status.is_ok(), f"{del_status}"
            else:
                ntokens_left = len(tokens)

            get_status = cache.acquire(prefix_tokens, tokens)
            if get_status.is_ok():
                assert get_status.value[0] > 0, f"{get_status.value[0]}<=0"
                assert (
                    get_status.value[0] <= ntokens_left
                ), f"{get_status.value[0]}>{ntokens_left}"
                get_handle = get_status.value[1]
                get_tensors = get_handle.to_tensors()
                for pt, gt in zip(token_tensors, get_tensors):
                    assert torch.equal(pt, gt)
                results.append(1)
                get_handle.release()
            else:
                results.append(0)

        recorder = cache._recorder

        skips = ["out_of_memory", "denied", "not_found"]
        for reason, num in recorder.put_metrics.num_errors_by_reason.items():
            if num > 0 and reason not in skips:
                raise AssertionError(f"PUT {reason}: {num}")

        for reason, num in recorder.get_metrics.num_errors_by_reason.items():
            if num > 0 and reason not in skips:
                raise AssertionError(f"GET {reason}: {num}")
        num_oks = sum(results)
        assert num_oks > 0, f"{num_oks}<=0"


@pytest.mark.parametrize(
    "layout", [KVCacheBlockLayout.NCLD, KVCacheBlockLayout.LCND]
)
def test_stress_cache(envs, layout):
    dist_run(_test_stress_cache, envs, 8, layout)
