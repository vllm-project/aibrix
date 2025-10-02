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

import asyncio
import contextlib
import functools
import logging
import threading
from abc import ABC, abstractmethod
from concurrent.futures import Executor, ThreadPoolExecutor
from dataclasses import dataclass
from typing import Iterator, List, Sequence, Tuple, overload

import torch
import torch.distributed as dist
import uvloop

from . import envs
from .cache_args import parse_kvcache_api_args
from .cache_handle import (
    GDRKVCacheHandle,
    KVCacheHandle,
    MemoryRegionKVCacheHandle,
)
from .cache_hashable import (
    BlockHashes,
    KVCacheKey,
    KVCacheKeyTypes,
    TokenListView,
)
from .common.absl_logging import getLogger, log_every_n_seconds, log_if
from .config import KVCacheConfig
from .l1 import L1Cache
from .l2 import KeyBuilder, L2Cache
from .memory import ManagedMemoryRegion, MemoryRegion, TensorPoolAllocator
from .meta_service import MetaService
from .metrics import KVCacheMetrics, MeasurableBase, MetricRecorder
from .profiling import nvtx_range
from .spec import KVCacheBlockLayout, KVCacheBlockSpec
from .status import Status, StatusCodes
from .utils import round_down, round_up

logger = getLogger(__name__)

asyncio.set_event_loop_policy(uvloop.EventLoopPolicy())
TESTING_DISABLE_PIN_MEMORY: bool = False


@dataclass
class KVCacheFeature:
    """The features of the kv cache.
    Args:
        gdr_put: Whether the cache manager supports GDR put.
        gdr_get: Whether the cache manager supports GDR get.
    """

    gdr_put: bool = False
    gdr_get: bool = False


class KVCacheManager(ABC):
    """The KV cache manager.

    Args:
        config: The KV cache manager configuration.
    """

    def __init__(self, config: KVCacheConfig) -> None:
        self.config: KVCacheConfig = config
        self.block_spec: KVCacheBlockSpec = self.config.block_spec
        self.block_layout: KVCacheBlockLayout = (
            self.config.block_spec.block_layout
        )
        self.block_shape: Tuple[int, ...] = self.config.block_spec.block_shape
        self.block_dtype: torch.dtype = self.config.block_spec.block_dtype
        self.block_ntokens: int = self.config.block_spec.block_ntokens
        self.block_nbytes: int = self.config.block_spec.block_nbytes
        self.block_shape_token_dim: int = self.block_spec.block_shape_token_dim

    @property
    @abstractmethod
    def feature(self) -> KVCacheFeature:
        """Get the feature of the kv cache.
        Returns:
            The feature of the kv cache.
        """
        raise NotImplementedError

    @property
    @abstractmethod
    def block_size(self) -> int:
        """Get the block size of the kv cache.
        Returns:
            The block size of the kv cache.
        """
        raise NotImplementedError

    @property
    @abstractmethod
    def chunk_size(self) -> int:
        """Get the chunk size of the kv cache.
        Returns:
            The chunk size of the kv cache.
        """
        raise NotImplementedError

    def register_kvcache(
        self, kvcache: torch.Tensor | List[torch.Tensor]
    ) -> Status:
        """Register kvcache with backend-specific register function.
        Args:
            kvcache: kvcache to be registered.
        Returns:
            Status of the register operation.
        """
        raise NotImplementedError

    @overload
    def prefetch(
        self, prefix: TokenListView | None, query: TokenListView
    ) -> None:
        """(Optional) Prefetch the kv cache for the given cache key.
        Args:
            prefix: The prefix tokens of the kv cache. E.g., [1, 2, 3]
            query: The query tokens of the kv cache. E.g., [4, 5, 6, 7]
        """
        ...

    @overload
    def prefetch(self, prefix: BlockHashes | None, query: BlockHashes) -> None:
        """(Optional) Prefetch the kv cache for the given cache key.
        Args:
            prefix: The prefix block hashes of the kv cache.
            query: The query block hashes of the kv cache.
        """
        ...

    @overload
    def prefetch(self, cache_key: KVCacheKey) -> None:
        """(Optional) Prefetch the kv cache for the given cache key.
        Args:
            cache_key: The cache key of the kv cache. E.g., KVCacheKey(
                [1, 2, 3], [4, 5, 6, 7])
        """
        ...

    def prefetch(self, *args, **kwargs) -> None:
        pass

    @overload
    def allocate_for(
        self, prefix: TokenListView | None, query: TokenListView
    ) -> Status[KVCacheHandle]:
        """Allocate a cache handle that points to buffers owned
        by the kv cache service.

        Args:
            prefix: The prefix tokens of the kv cache. E.g., [1, 2, 3]
            query: The query tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            The cache handle.
        """
        ...

    @overload
    def allocate_for(
        self, prefix: BlockHashes | None, query: BlockHashes
    ) -> Status[KVCacheHandle]:
        """Allocate a cache handle that points to buffers owned
        by the kv cache service.

        Args:
            prefix: The prefix block hashes of the kv cache.
            query: The query block hashes of the kv cache.
        Returns:
            The cache handle.
        """
        ...

    @overload
    def allocate_for(self, cache_key: KVCacheKey) -> Status[KVCacheHandle]:
        """Allocate a cache handle that points to buffers owned
        by the kv cache service.

        Args:
            cache_key: The cache key of the kv cache. E.g., KVCacheKey(
                [1, 2, 3], [4, 5, 6, 7])
        Returns:
            The cache handle.
        """
        ...

    @abstractmethod
    def allocate_for(self, *args, **kwargs) -> Status[KVCacheHandle]:
        raise NotImplementedError

    @overload
    def acquire(
        self,
        prefix: TokenListView | None,
        query: TokenListView,
    ) -> Status[Tuple[int, KVCacheHandle]]:
        """Acquire cache handle of the kv tensors for the given prefix and
        query tokens.

        The returned cache handle pointing to buffers owned by the kv cache
        service. We can use "KVCacheHandle.to_tensors()" to get tensors sharing
        the same storage. After the kv tensors are used, we need to explicitly
        `ref_down()` the cache handle to let the kv cache service know that
        these buffers are not referenced anymore.

        Args:
            prefix: The prefix tokens of the kv cache. E.g., [1, 2, 3]
            query: The query tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            Number of tokens have been fetched from the kv cache service.
            The cache handles corresponding to the given tokens.
        """
        ...

    @overload
    def acquire(
        self,
        prefix: BlockHashes | None,
        query: BlockHashes,
    ) -> Status[Tuple[int, KVCacheHandle]]:
        """Acquire cache handle of the kv tensors for the given prefix and
        query blocks.

        The returned cache handle pointing to buffers owned by the kv cache
        service. We can use "KVCacheHandle.to_tensors()" to get tensors sharing
        the same storage. After the kv tensors are used, we need to explicitly
        `ref_down()` the cache handle to let the kv cache service know that
        these buffers are not referenced anymore.

        Args:
            prefix: The prefix block hashes of the kv cache.
            query: The query block hashes of the kv cache.
        Returns:
            Number of tokens have been fetched from the kv cache service.
            The cache handles corresponding to the given tokens.
        """
        ...

    @overload
    def acquire(
        self, cache_key: KVCacheKey
    ) -> Status[Tuple[int, KVCacheHandle]]:
        """Acquire cache handle of the kv tensors for the given cache key.

        The returned cache handle pointing to buffers owned by the kv cache
        service. We can use "KVCacheHandle.to_tensors()" to get tensors sharing
        the same storage. After the kv tensors are used, we need to explicitly
        `ref_down()` the cache handle to let the kv cache service know that
        these buffers are not referenced anymore.

        Args:
            cache_key: The cache key of the kv cache. E.g., KVCacheKey(
                [1, 2, 3], [4, 5, 6, 7])
        Returns:
            Number of tokens have been fetched from the kv cache service.
            The cache handles corresponding to the given tokens.
        """
        ...

    @abstractmethod
    def acquire(self, *args, **kwargs) -> Status[Tuple[int, KVCacheHandle]]:
        raise NotImplementedError

    @overload
    def get(
        self,
        prefix: TokenListView | None,
        query: TokenListView,
        kv_tensors: KVCacheHandle,
    ) -> Status[int]:
        """Get the kv tensors for the given prefix and query tokens.

        Args:
            prefix: The prefix tokens of the kv cache. E.g., [1, 2, 3]
            query: The query tokens of the kv cache. E.g., [4, 5, 6, 7]
            kv_tensors: The kv tensors to store the fetched kv cache.
        Returns:
            Number of tokens have been fetched from the kv cache service.
        """
        ...

    @overload
    def get(
        self,
        prefix: BlockHashes | None,
        query: BlockHashes,
        kv_tensors: KVCacheHandle,
    ) -> Status[int]:
        """Get the kv tensors for the given prefix and query blocks.

        Args:
            prefix: The prefix block hashes of the kv cache.
            query: The query block hashes of the kv cache.
            kv_tensors: The kv tensors to store the fetched kv cache.
        Returns:
            Number of tokens have been fetched from the kv cache service.
        """
        ...

    @overload
    def get(
        self,
        cache_key: KVCacheKey,
        kv_tensors: KVCacheHandle,
    ) -> Status[int]:
        """Get the kv tensors for the given cache key.

        Args:
            cache_key: The cache key of the kv cache. E.g., KVCacheKey(
                [1, 2, 3], [4, 5, 6, 7])
            kv_tensors: The kv tensors to store the fetched kv cache.
        Returns:
            Number of tokens have been fetched from the kv cache service.
        """
        ...

    @abstractmethod
    def get(self, *args, **kwargs) -> Status[int]:
        raise NotImplementedError

    @overload
    def exists(
        self, prefix: TokenListView | None, query: TokenListView
    ) -> Status[int]:
        """Check if the kv cache corresponding to given prefix and
        query tokens exists.

        Args:
            prefix: The prefix tokens of the kv cache. E.g., [1, 2, 3]
            query: The query tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            Number of tokens existing in the kv cache service.
        """
        ...

    @overload
    def exists(
        self, prefix: BlockHashes | None, query: BlockHashes
    ) -> Status[int]:
        """Check if the kv cache corresponding to given prefix and
        query blocks exists.

        Args:
            prefix: The prefix block hashes of the kv cache.
            query: The query block hashes of the kv cache.
        Returns:
            Number of tokens existing in the kv cache service.
        """
        ...

    @overload
    def exists(self, cache_key: KVCacheKey) -> Status[int]:
        """Check if the kv cache corresponding to given cache key.

        Args:
            cache_key: The cache key of the kv cache. E.g., KVCacheKey(
                [1, 2, 3], [4, 5, 6, 7])
        Returns:
            Number of tokens existing in the kv cache service.
        """
        ...

    @abstractmethod
    def exists(self, *args, **kwargs) -> Status[int]:
        raise NotImplementedError

    @overload
    def put(
        self,
        prefix: TokenListView | None,
        query: TokenListView,
        kv_tensors: KVCacheHandle,
    ) -> Status[int]:
        """Put kv tensors to the kv cache service.

        Args:
            prefix: The prefix tokens of the kv cache. E.g., [1, 2, 3]
            query: The query tokens of the kv cache. E.g., [4, 5, 6, 7]
            kv_tensors:
                Cache handle of the kv tensors to put into the kv cache.

                The layout of kv_tensors must match the layout of the
                kv cache service.

                For example, if the layout is NCLD, then:
                The k, v tensors for i-th token at the j-th layer are
                kv_tensors[i][0[j] and kv_tensors[i][1[j], respectively.

        Returns:
            The status of the put operation and the number of tokens have
            been put or scheduled to put into the kv cache service.
        """
        ...

    @overload
    def put(
        self,
        prefix: BlockHashes | None,
        query: BlockHashes,
        kv_tensors: KVCacheHandle,
    ) -> Status[int]:
        """Put kv tensors to the kv cache service.

        Args:
            prefix: The prefix block hashes of the kv cache.
            query: The query block hashes of the kv cache.
            kv_tensors:
                Cache handle of the kv tensors to put into the kv cache.

                The layout of kv_tensors must match the layout of the
                kv cache service.

                For example, if the layout is NCLD, then:
                The k, v tensors for i-th token at the j-th layer are
                kv_tensors[i][0[j] and kv_tensors[i][1[j], respectively.

        Returns:
            The status of the put operation and the number of tokens have
            been put or scheduled to put into the kv cache service.
        """
        ...

    @overload
    def put(
        self,
        cache_key: KVCacheKey,
        kv_tensors: KVCacheHandle,
    ) -> Status[int]:
        """Put kv tensors to the kv cache service.

        Args:
            cache_key: The cache key of the kv cache. E.g., KVCacheKey(
                [1, 2, 3], [4, 5, 6, 7])
            kv_tensors:
                Cache handle of the kv tensors to put into the kv cache.

                The layout of kv_tensors must match the layout of the
                kv cache service.

                For example, if the layout is NCLD, then:
                The k, v tensors for i-th token at the j-th layer are
                kv_tensors[i][0[j] and kv_tensors[i][1[j], respectively.

        Returns:
            The status of the put operation and the number of tokens have
            been put or scheduled to put into the kv cache service.
        """
        ...

    @abstractmethod
    def put(self, *args, **kwargs) -> Status[int]:
        raise NotImplementedError

    @overload
    def delete(
        self,
        prefix: TokenListView | None,
        query: TokenListView,
    ) -> Status:
        """Delete kv tensors from the kv cache service.
        Args:
            prefix: The prefix tokens of the kv cache. E.g., [1, 2, 3]
            query: The query tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            The status of the delete operation.
        """
        ...

    @overload
    def delete(
        self,
        prefix: BlockHashes | None,
        query: BlockHashes,
    ) -> Status:
        """Delete kv tensors from the kv cache service.
        Args:
            prefix: The prefix block hashes of the kv cache.
            query: The query block hashes of the kv cache.
        Returns:
            The status of the delete operation.
        """
        ...

    @overload
    def delete(self, cache_key: KVCacheKey) -> Status:
        """Delete kv tensors from the kv cache service.
        Args:
            cache_key: The cache key of the kv cache. E.g., KVCacheKey(
                [1, 2, 3], [4, 5, 6, 7])
        Returns:
            The status of the delete operation.
        """
        ...

    @abstractmethod
    def delete(self, *args, **kwargs) -> Status:
        raise NotImplementedError

    def flush(self) -> Status:
        """Flush the kv cache service.

        Returns:
            The status of the flush operation.
        """
        return Status.ok()

    @overload
    def cache_chunk_keys(
        self, prefix: TokenListView | None, query: TokenListView
    ) -> Iterator[
        Tuple[
            TokenListView,
            TokenListView,
            TokenListView | None,
            TokenListView,
        ]
    ]:
        """Get the cache chunk keys.
        Args:
            prefix: The prefix tokens of the kv tensors.
            query: The query tokens of the kv tensors.
        Returns:
            chunk prefix tokens, chunk query tokens, next chunk query tokens,
            and all tokens
        """
        ...

    @overload
    def cache_chunk_keys(
        self, prefix: BlockHashes | None, query: BlockHashes
    ) -> Iterator[
        Tuple[
            BlockHashes,
            BlockHashes,
            BlockHashes | None,
            BlockHashes,
        ]
    ]:
        """Get the cache chunk keys.
        Args:
            prefix: The prefix blocks of the kv tensors.
            query: The query blocks of the kv tensors.
        Returns:
            chunk prefix blocks, chunk query blocks, next chunk query blocks,
            and all blocks
        """
        ...

    @abstractmethod
    def cache_chunk_keys(
        self, prefix: KVCacheKeyTypes | None, query: KVCacheKeyTypes
    ) -> Iterator[
        Tuple[
            KVCacheKeyTypes,
            KVCacheKeyTypes,
            KVCacheKeyTypes | None,
            KVCacheKeyTypes,
        ]
    ]:
        raise NotImplementedError

    @abstractmethod
    def close(self) -> None:
        """Close the kv cache service."""
        raise NotImplementedError

    @property
    @abstractmethod
    def metrics(self) -> KVCacheMetrics:
        """Get the metrics of the kv cache service."""
        raise NotImplementedError


class BaseKVCacheManager(KVCacheManager, MeasurableBase):
    """Base KV cache manager.

    Args:
        config: The KV cache manager configuration.
    """

    def __init__(self, config: KVCacheConfig) -> None:
        KVCacheManager.__init__(self, config)

        self._l1_cache: L1Cache | None = None
        self._l2_cache: L2Cache | None = None
        self._executor: Executor | None = None
        self._event_loop: asyncio.AbstractEventLoop | None = None
        self._thread: threading.Thread | None = None
        self._l2_inflight_writes: int = 0
        self._l2_inflight_quota: int = 0
        self._allocator: TensorPoolAllocator | None = None
        self._metrics: KVCacheMetrics | None = None
        self._ms: MetaService | None = None

        self._lock = threading.Lock()
        self._infight_cv = threading.Condition(self._lock)

        self._double_get_threshold: Tuple[int, float] = (
            envs.AIBRIX_KV_CACHE_OL_DOUBLE_GET_THRESHOLD
        )
        self._l2_cache_per_token_timeout_ms: int = (
            envs.AIBRIX_KV_CACHE_OL_L2_CACHE_PER_TOKEN_TIMEOUT_MS
        )

        self._chunk_size: int = envs.AIBRIX_KV_CACHE_OL_CHUNK_SIZE
        self._max_seq_len: int = envs.AIBRIX_KV_CACHE_OL_MAX_SEQ_LEN
        if self._max_seq_len > 0:
            max_seq_len = round_up(self._max_seq_len, self.block_ntokens)
            log_if(
                logger,
                logging.WARNING,
                "AIBRIX_KV_CACHE_OL_MAX_SEQ_LEN=%d is not divisible by "
                "block_ntokens=%d, aligned to %d",
                max_seq_len > self._max_seq_len,
                self._max_seq_len,
                self.block_ntokens,
                max_seq_len,
            )
            self._max_seq_len = max_seq_len

        if self._chunk_size % self.block_ntokens != 0:
            self._chunk_size = (
                self._chunk_size - self._chunk_size % self.block_ntokens
            )
            logger.warning(
                (
                    "AIBRIX_KV_CACHE_OL_CHUNK_SIZE=%d is not divisible by "
                    "block_ntokens=%d, aligned to %d"
                ),
                envs.AIBRIX_KV_CACHE_OL_CHUNK_SIZE,
                self.block_ntokens,
                self._chunk_size,
            )

        if self._chunk_size < 4 * self.block_ntokens:
            logger.warning(
                "AIBRIX_KV_CACHE_OL_CHUNK_SIZE=%d is too small, using %d",
                self._chunk_size,
                4 * self.block_ntokens,
            )
            self._chunk_size = 4 * self.block_ntokens

        device: str = envs.AIBRIX_KV_CACHE_OL_DEVICE

        pin_memory: bool = False
        if not TESTING_DISABLE_PIN_MEMORY:
            pin_memory = device == "cpu"

        enable_l1: bool = envs.AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED
        enable_l2: bool = len(envs.AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND) > 0

        assert enable_l1 or enable_l2, (
            "At least one cache service must be enabled."
        )

        capacity_nbytes: int = int(
            envs.AIBRIX_KV_CACHE_OL_L1_CACHE_CAPACITY_GB * 1024**3
        )
        enable_time_measurement = (
            envs.AIBRIX_KV_CACHE_OL_TIME_MEASUREMENT_ENABLED
        )
        enable_breakdown_measurement = (
            envs.AIBRIX_KV_CACHE_OL_BREAKDOWN_MEASUREMENT_ENABLED
        )
        self._metrics = KVCacheMetrics(
            block_ntokens=self.block_ntokens,
            capacity_nbytes=capacity_nbytes,
            enable_l1=enable_l1,
            enable_l2=enable_l2,
            enable_time_measurement=enable_time_measurement,
            enable_breakdown_measurement=enable_breakdown_measurement,
        )

        ms_backend: str = envs.AIBRIX_KV_CACHE_OL_META_SERVICE_BACKEND
        if len(ms_backend) > 0:
            self._ms = MetaService.create(ms_backend)
            status = self._ms.open()
            status.raise_if_not_ok()
            logger.info(f"Using meta service backend: {self._ms.name}")

        # init MeasurableBase
        assert self._metrics is not None
        MeasurableBase.__init__(self, self._metrics.mgr)

        # init allocator
        allocator_capacity_nbytes: int = 0

        if enable_l1:
            allocator_capacity_nbytes += capacity_nbytes

        if enable_l2:
            self._l2_inflight_quota = (
                envs.AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_MAX_INFLIGHT_TOKENS
                // self.block_ntokens
            )

            engine_batch_ntokens = self.config.model_spec.max_num_batched_tokens
            if engine_batch_ntokens <= 0:
                engine_batch_ntokens = self._chunk_size

            engine_batch_nblocks = engine_batch_ntokens // self.block_ntokens

            if 0 < self._l2_inflight_quota < 2 * engine_batch_nblocks:
                temp = self._l2_inflight_quota * self.block_ntokens
                self._l2_inflight_quota = 2 * engine_batch_nblocks
                logger.warning(
                    "AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_MAX_INFLIGHT_TOKENS"
                    "=%d is too small, using %d instead",
                    temp,
                    self._l2_inflight_quota * self.block_ntokens,
                )

            max_mr_nbytes = ManagedMemoryRegion.calculate_size(
                self.block_nbytes, self.config.model_spec.max_model_len
            )
            nblocks_per_batch = engine_batch_ntokens // self.block_ntokens
            # more capacity for async/sync load
            if self._l2_inflight_quota > 0:
                more_capacity_nbytes = self._l2_inflight_quota * max_mr_nbytes
            else:
                more_capacity_nbytes = nblocks_per_batch * max_mr_nbytes

            # more capacity for get
            more_capacity_nbytes += 2 * nblocks_per_batch * max_mr_nbytes

            allocator_capacity_nbytes += more_capacity_nbytes

        self._allocator = TensorPoolAllocator.create(
            capacity_nbytes=allocator_capacity_nbytes,
            device=device,
            pin_memory=pin_memory,
        )

        # init caches
        if enable_l1:
            eviction_policy: str = (
                envs.AIBRIX_KV_CACHE_OL_L1_CACHE_EVICTION_POLICY
            )

            self._l1_cache = L1Cache(
                eviction_policy,
                capacity_nbytes,
                self._allocator,
                self.block_spec,
                metrics=self._metrics.l1,
            )

        if enable_l2:
            backend_name: str = envs.AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND
            namespace: str = envs.AIBRIX_KV_CACHE_OL_L2_CACHE_NAMESPACE
            # compression: str = envs.AIBRIX_KV_CACHE_OL_L2_CACHE_COMPRESSION
            ingestion_type: str = (
                envs.AIBRIX_KV_CACHE_OL_L2_CACHE_INGESTION_TYPE
            )
            op_batch: int = envs.AIBRIX_KV_CACHE_OL_L2_CACHE_OP_BATCH
            self._executor = ThreadPoolExecutor(
                envs.AIBRIX_KV_CACHE_OL_L2_CACHE_NUM_ASYNC_WORKERS,
                thread_name_prefix="l2_cache_",
            )

            placement_policy = envs.AIBRIX_KV_CACHE_OL_L2_CACHE_PLACEMENT_POLICY
            refresh_interval_s = (
                envs.AIBRIX_KV_CACHE_OL_META_SERVICE_REFRESH_INTERVAL_S
            )
            key_builder = KeyBuilder.create(
                envs.AIBRIX_KV_CACHE_OL_L2_CACHE_KEY_BUILDER,
                block_size=self.block_ntokens,
            )
            self._l2_cache = L2Cache(
                backend_name=backend_name,
                placement_policy=placement_policy,
                namespace=namespace,
                block_spec=self.block_spec,
                executor=self._executor,
                refresh_interval_s=refresh_interval_s,
                op_batch=op_batch,
                metrics=self._metrics.l2,
                meta_service=self._ms,
                key_builder=key_builder,
            )

            # new an event loop to carry out L2Cache ops
            self._event_loop = asyncio.new_event_loop()
            self._thread = threading.Thread(
                target=self._event_loop.run_forever, daemon=True
            )
            self._thread.start()

            # launch L2Cache
            status = self._l2_cache.open()
            status.raise_if_not_ok()

            if self._l2_cache._backend.feature.rdma:
                reg_status = self._l2_cache.register_slabs(
                    self._allocator.slabs
                )
                if not reg_status.is_ok():
                    logger.fatal(
                        f"Failed to register slabs with "
                        f"{self._l2_cache._backend.name}'s register func, "
                        f"error={reg_status}"
                    )
                    reg_status.raise_if_not_ok()

            # register l1 cache callback
            if self._l1_cache is not None:
                if ingestion_type == "HOT":
                    self._l1_cache.set_on_hot_access_callback(
                        self._l2_ingestion_callback  # type: ignore
                    )
                elif ingestion_type == "ALL":
                    self._l1_cache.set_on_put_callback(
                        self._l2_ingestion_callback  # type: ignore
                    )
                else:
                    self._l1_cache.set_on_evict_callback(
                        self._l2_ingestion_callback  # type: ignore
                    )

        assert self._l1_cache is not None or self._l2_cache is not None, (
            "At least one cache service must be enabled."
        )

        logger.info("%s is initialized", self)

    @functools.cached_property
    def feature(self) -> KVCacheFeature:
        """Get the feature of the kv cache.
        Returns:
            The feature of the kv cache.
        """
        feature = KVCacheFeature()
        if self._l1_cache is not None:
            return feature
        if self._l2_cache is not None and self._l2_cache._backend is not None:
            if self._l2_cache._backend.feature.gdr_get:
                feature.gdr_get = True
            if self._l2_cache._backend.feature.gdr_put:
                feature.gdr_put = True
        return feature

    @property
    def block_size(self) -> int:
        """Get the block size of the kv cache.
        Returns:
            The block size of the kv cache.
        """
        return self.block_ntokens

    @property
    def chunk_size(self) -> int:
        """Get the chunk size of the kv cache.
        Returns:
            The chunk size of the kv cache.
        """
        return self._chunk_size

    @property
    def metrics(self) -> KVCacheMetrics:
        assert self._metrics is not None
        return self._metrics

    def __repr__(self) -> str:
        return (
            f"BaseKVCacheManager(l1_cache={self._l1_cache}, "
            f"l2_cache={self._l2_cache})"
        )

    def __str__(self) -> str:
        return self.__repr__()

    def close(self) -> None:
        # flush
        self.flush()

        # terminate event loop and thread
        if self._event_loop is not None and self._event_loop.is_running():
            with contextlib.suppress(Exception):
                # ignore the exception
                self._event_loop.call_soon_threadsafe(self._event_loop.stop)

        if self._thread is not None and self._thread.is_alive():
            self._thread.join()

        if self._executor is not None:
            self._executor.shutdown(wait=True)

        if self._l1_cache is not None:
            del self._l1_cache

        if self._l2_cache is not None:
            self._l2_cache.close()
            del self._l2_cache

    def _l2_ingestion_callback(
        self,
        key: KVCacheKey,
        mr: MemoryRegion,
    ) -> Status:
        """Ingestion callback for L2Cache.
        Args:
            key: Cache key of the MR to be ingested.
            mr: The memory region to be ingested.
        Returns:
            The status of the ingestion operation and the number of tokens have
            been ingested or scheduled.
        """
        return self._l2_put(key.prefix, key.query, mr)

    def _l2_put(
        self,
        prefix: KVCacheKeyTypes | None,
        query: KVCacheKeyTypes,
        value: MemoryRegion | KVCacheHandle,
    ) -> Status:
        """Put the kv tensors to the L2Cache.
        Args:
            prefix: The prefix tokens of the kv tensors.
            query: The query tokens of the kv tensors.
            value: The kv tensors.
        Returns:
            The status of the put operation and the number of tokens have
            been put or scheduled.
        """
        assert self._l2_cache is not None, "l2_cache is not initialized."

        status = None
        if self._l2_inflight_quota == 0:
            # sync write
            status = self._l2_put_sync(prefix, query, value)
        else:
            # async write
            status = self._l2_put_async(prefix, query, value)

        return status

    def _l2_put_async(
        self,
        prefix: KVCacheKeyTypes | None,
        query: KVCacheKeyTypes,
        value: (MemoryRegion | KVCacheHandle),
    ) -> Status:
        """Put the kv tensors to the L2Cache asynchrously.
        Args:
            prefix: The prefix tokens of the kv tensors.
            query: The tokens of the kv tensors.
            value: The kv tensors.
        Returns:
            The status of the put operation and the number of tokens have
            been scheduled.
        """
        assert self._l2_cache is not None, "l2_cache is not initialized."
        with self._lock:
            log_every_n_seconds(
                logger,
                logging.INFO,
                "l2_cache infight writes %d/quota %d",
                3,
                self._l2_inflight_writes,
                self._l2_inflight_quota,
            )
            if self._l2_inflight_quota <= self._l2_inflight_writes:
                log_every_n_seconds(
                    logger,
                    logging.WARNING,
                    (
                        "There are too many infight writes, skip writing to "
                        "l2_cache. infight writes %d/quota %d"
                    ),
                    10,
                    self._l2_inflight_writes,
                    self._l2_inflight_quota,
                )
                return Status(StatusCodes.DENIED)
            self._l2_inflight_writes += 1

        def _done_callback(
            future: asyncio.Future,
            value: (MemoryRegion | KVCacheHandle),
        ) -> None:
            self._release([value])

            with self._infight_cv:
                self._l2_inflight_writes -= 1
                self._infight_cv.notify_all()
            if not future.result().is_ok():
                log_every_n_seconds(
                    logger,
                    logging.WARNING,
                    "Failed to write to l2_cache, error: %s",
                    10,
                    future.result().value,
                )

        # Async write to L2Cache
        assert self._event_loop is not None
        future = asyncio.run_coroutine_threadsafe(
            self._l2_cache.put(prefix, query, value), self._event_loop
        )
        future.add_done_callback(functools.partial(_done_callback, value=value))
        return Status.ok(len(query))

    def _l2_put_sync(
        self,
        prefix: KVCacheKeyTypes | None,
        query: KVCacheKeyTypes,
        value: (MemoryRegion | KVCacheHandle),
    ) -> Status:
        """Put the kv tensors to the L2Cache blockingly.
        Args:
            prefix: The prefix tokens of the kv tensors.
            query: The tokens of the kv tensors.
            value: The kv tensors.
        Returns:
            The status of the put operation and the number of tokens have
            been put.
        """
        assert self._l2_cache is not None, "l2_cache is not initialized."
        assert self._event_loop is not None
        future = asyncio.run_coroutine_threadsafe(
            self._l2_cache.put(prefix, query, value), self._event_loop
        )
        # wait until the write is done
        status = future.result()

        self._release([value])

        if not status.is_ok():
            return status
        return Status.ok(status.get() * self.block_ntokens)

    def register_kvcache(
        self, kvcache: torch.Tensor | List[torch.Tensor]
    ) -> Status:
        """Register kvcache with backend-specific register function.
        Args:
            kvcache: kvcache to be registered.
        Returns:
            Status of the register operation.
        """
        if self._l2_cache is None or self._l2_cache._backend is None:
            return Status.ok()

        if isinstance(kvcache, torch.Tensor):
            kvcache = [kvcache]

        kvcache_bytes_views = [t.flatten().view(torch.uint8) for t in kvcache]
        return self._l2_cache._backend.register_slabs(kvcache_bytes_views)

    def prefetch(self, *args, **kwargs) -> None:
        # TODO: implement background prefetching that loads kv cache
        # from L2Cache to L1Cache.
        pass

    @nvtx_range("acquire", "KVCacheManager")
    @MeasurableBase.measure(MetricRecorder.OP.ACQUIRE)
    def acquire(self, *args, **kwargs) -> Status[Tuple[int, KVCacheHandle]]:
        prefix, query, _ = parse_kvcache_api_args(*args, **kwargs)

        if not isinstance(query, TokenListView):
            assert self.block_ntokens % query.block_ntokens == 0, (
                f"kvcache's block size ({self.block_ntokens}) must be multiple "
                f"of cache key's block_ntokens ({query.block_ntokens})"
            )

        status = self._acquire_impl(prefix, query)
        if not status.is_ok():
            return Status(status)

        value = status.get()
        return Status.ok(
            (
                len(value) * self.block_ntokens,
                MemoryRegionKVCacheHandle(
                    self.block_dtype,
                    self.block_shape,
                    value,  # type: ignore
                ),
            )
        )

    @nvtx_range("get", "KVCacheManager")
    @MeasurableBase.measure(MetricRecorder.OP.GET)
    def get(self, *args, **kwargs) -> Status[int]:
        prefix, query, kv_tensors = parse_kvcache_api_args(*args, **kwargs)

        if not isinstance(query, TokenListView):
            assert self.block_ntokens % query.block_ntokens == 0, (
                f"kvcache's block size ({self.block_ntokens}) must be multiple "
                f"of cache key's block_ntokens ({query.block_ntokens})"
            )

        if isinstance(kv_tensors, GDRKVCacheHandle):
            assert self.feature.gdr_get, "Does not support GDR get"
        mrs = kv_tensors.memory_regions
        return self._get_impl(prefix, query, mrs)

    def _get_impl(
        self,
        prefix: KVCacheKeyTypes | None,
        query: KVCacheKeyTypes,
        mrs: Sequence[MemoryRegion] | Sequence[Sequence[MemoryRegion]],
    ) -> Status[int]:
        """Get the kv tensors for the given prefix and query tokens.

        Args:
            prefix: The prefix tokens/block hashes of the kv cache.
            query: The query tokens/block hashes of the kv cache.
            mrs: The memory regions to store the fetched kv cache.
        Returns:
            Number of tokens have been fetched from the kv cache service.
        """
        status = self._acquire_impl(prefix, query, mrs)
        if not status.is_ok():
            return Status(status)
        return Status.ok(len(status.get()) * self.block_ntokens)

    @MeasurableBase.measure(MetricRecorder.OP.EXISTS)
    def exists(self, *args, **kwargs) -> Status[int]:
        prefix, query, _ = parse_kvcache_api_args(*args, **kwargs)

        status = self._exists_impl(prefix, query)
        if not status.is_ok():
            return status

        return Status.ok(status.get() * self.block_ntokens)

    def _exists_impl(
        self, prefix: KVCacheKeyTypes | None, query: KVCacheKeyTypes
    ) -> Status[int]:
        """Check if the kv cache corresponding to given prefix and
        query tokens exists.

        Args:
            prefix: The prefix tokens/block hashes of the kv cache.
            query: The query tokens/block hashes of the kv cache.
        Returns:
            Number of tokens existing in the kv cache service.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        num_blocks = len(query) // self.block_ntokens

        # If it is not a full block, return
        if num_blocks == 0:
            return Status(StatusCodes.NOT_FOUND)

        num_existing_blocks = 0
        num_missing_blocks = num_blocks
        l1_status: Status[int] = Status(StatusCodes.NOT_FOUND)
        if self._l1_cache is not None:
            l1_status = self._l1_cache.exists(prefix, query)

            num_existing_blocks = l1_status.get(default=0)
            num_missing_blocks -= num_existing_blocks

            if num_missing_blocks == 0 or not self._use_double_get(
                num_missing_blocks, num_blocks
            ):
                # 1. fully exist in L1Cache, return the result directly
                # 2. L2Cache is not enabled, return the result directly
                # 3. num of missing blocks is less than the threshold,
                #    return the result directly
                return Status(l1_status)

        assert self._l2_cache is not None
        num_existing_tokens = num_existing_blocks * self.block_ntokens
        # check if missing kv tensors are in L2Cache
        if prefix is not None:
            prefix_curr = prefix + query[:num_existing_tokens]
        else:
            prefix_curr = query[:num_existing_tokens]
        tokens_curr = query[num_existing_tokens:]
        timeout_s = (
            num_missing_blocks
            * self.block_ntokens
            * self._l2_cache_per_token_timeout_ms
        ) / 1000

        assert self._event_loop is not None
        future = asyncio.run_coroutine_threadsafe(
            self._l2_cache.exists(prefix_curr, tokens_curr), self._event_loop
        )
        try:
            status = future.result(timeout=timeout_s)
            if not status.is_ok():
                return status if num_existing_blocks == 0 else l1_status

            return Status.ok(num_existing_blocks + status.get())
        except asyncio.CancelledError:
            # cancelled
            return (
                Status(StatusCodes.CANCELLED)
                if num_existing_blocks == 0
                else l1_status
            )
        except asyncio.TimeoutError:
            # timed out
            return (
                Status(StatusCodes.TIMEOUT)
                if num_existing_blocks == 0
                else l1_status
            )
        except Exception as e:
            # other exceptions
            return (
                Status(StatusCodes.ERROR, e)
                if num_existing_blocks == 0
                else l1_status
            )
        finally:
            if not future.done():
                future.cancel()

    def _acquire_impl(
        self,
        prefix: KVCacheKeyTypes | None,
        query: KVCacheKeyTypes,
        output_mrs: Sequence[MemoryRegion]
        | Sequence[Sequence[MemoryRegion]]
        | None = None,
    ) -> (
        Status[Sequence[MemoryRegion]]
        | Status[Sequence[Sequence[MemoryRegion]]]
    ):
        """Get kv tensors from the kv cache service.

        Args:
            prefix: The prefix tokens/block hashes of the kv cache.
            query: The query tokens/block hashes of the kv cache.
            output_mrs: The memory regions to store the fetched kv cache.
        Returns:
            The memory regions corresponding to the tokens.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        if output_mrs is not None:
            num_blocks = len(output_mrs)
            ntokens_to_get = num_blocks * self.block_ntokens
            query = query[:ntokens_to_get]
        else:
            num_blocks = len(query) // self.block_ntokens

        # If it is not a full block, return
        if num_blocks == 0:
            return Status(StatusCodes.NOT_FOUND)

        fetched_mrs: List[MemoryRegion] = []
        num_fetched_blocks = 0
        num_missing_blocks = num_blocks
        l1_status: Status[Sequence[MemoryRegion]] = Status(
            StatusCodes.NOT_FOUND
        )
        if self._l1_cache is not None:
            l1_status = self._l1_cache.acquire(prefix, query)

            fetched_mrs = list(l1_status.get()) if l1_status.is_ok() else []
            num_fetched_blocks = len(fetched_mrs)
            num_missing_blocks = num_blocks - num_fetched_blocks

            if num_missing_blocks == 0 or not self._use_double_get(
                num_missing_blocks, num_blocks
            ):
                # 1. fully hit on L1Cache, return the result directly
                # 2. L2Cache is not enabled, return the result directly
                # 3. num of missing blocks is less than the threshold,
                #    return the result directly
                if not l1_status.is_ok():
                    return l1_status

                if output_mrs is not None:
                    for i in range(num_fetched_blocks):
                        output_mrs[i].copy(fetched_mrs[i])  # type: ignore
                    l1_status = Status.ok(output_mrs[:num_fetched_blocks])  # type: ignore
                return l1_status

        assert self._l2_cache is not None
        # fetch missing kv tensors from L2Cache
        if prefix is not None:
            prefix_curr = (
                prefix + query[: num_fetched_blocks * self.block_ntokens]
            )
        else:
            prefix_curr = query[: num_fetched_blocks * self.block_ntokens]
        tokens_curr = query[num_fetched_blocks * self.block_ntokens :]
        timeout_s = (
            num_missing_blocks
            * self.block_ntokens
            * self._l2_cache_per_token_timeout_ms
        ) / 1000

        if output_mrs is None:
            # allocate MRs to hold fetched tensors
            status = self.allocate_for(prefix_curr, tokens_curr)
            if not status.is_ok():
                return status if num_fetched_blocks == 0 else l1_status
            mrs: List[MemoryRegion] = list(status.get().memory_regions)
        else:
            mrs: List[MemoryRegion] = list(output_mrs[num_fetched_blocks:])  # type: ignore

        ntokens_to_get = len(mrs) * self.block_ntokens
        tokens_curr = tokens_curr[:ntokens_to_get]

        assert self._event_loop is not None
        future = asyncio.run_coroutine_threadsafe(
            self._l2_cache.get(prefix_curr, tokens_curr, mrs), self._event_loop
        )
        try:
            get_status = future.result(timeout=timeout_s)
            if not get_status.is_ok():
                return get_status if num_fetched_blocks == 0 else l1_status

            value = get_status.get()
            l2_fetched_mrs = mrs[:value]
            mrs = mrs[value:]

            # put the fetched kv tensors to L1Cache
            if self._l1_cache is not None:
                put_tokens_curr = tokens_curr[
                    : len(l2_fetched_mrs) * self.block_ntokens
                ]

                if output_mrs is None:
                    for mr in l2_fetched_mrs:
                        mr.ref_up()
                    mrs_to_release = l2_fetched_mrs
                    put_status = self._l1_cache.put(
                        prefix_curr, put_tokens_curr, l2_fetched_mrs
                    )
                    if put_status.is_ok():
                        mrs_to_release = l2_fetched_mrs[put_status.get() :]
                    self._release(mrs_to_release)
                else:
                    put_tensors = [
                        mr.to_tensor(self.block_dtype, self.block_shape)
                        for mr in l2_fetched_mrs
                    ]
                    self._l1_cache.put(
                        prefix_curr, put_tokens_curr, put_tensors
                    )

            if output_mrs is None:
                return Status.ok(fetched_mrs + l2_fetched_mrs)  # type: ignore
            else:
                return Status.ok(output_mrs[: num_fetched_blocks + value])  # type: ignore
        except asyncio.CancelledError:
            # cancelled
            return (
                Status(StatusCodes.CANCELLED)
                if num_fetched_blocks == 0
                else l1_status
            )
        except asyncio.TimeoutError:
            # timed out
            return (
                Status(StatusCodes.TIMEOUT)
                if num_fetched_blocks == 0
                else l1_status
            )
        except Exception as e:
            # other exceptions
            return (
                Status(StatusCodes.ERROR, e)
                if num_fetched_blocks == 0
                else l1_status
            )
        finally:
            if output_mrs is None:
                self._release(mrs)
            if not future.done():
                future.cancel()

    def _use_double_get(
        self, num_missing_blocks: int, num_total_blocks: int
    ) -> bool:
        """Whether to use double get.
        Args:
            num_missing_blocks: The number of missing blocks.
            num_total_blocks: The total number of blocks.
        Returns:
            Whether to use double get.
        """
        if self._l2_cache is None:
            return False
        if len(self._double_get_threshold) == 1:
            # only num is set
            return num_missing_blocks >= self._double_get_threshold[0]
        elif len(self._double_get_threshold) == 2:
            # both ratio and num are set
            return (
                num_missing_blocks / num_total_blocks
                >= self._double_get_threshold[1]
                and num_missing_blocks >= self._double_get_threshold[0]
            )
        return False

    def _release(
        self,
        mr_or_handle_or_tensors: Sequence[
            torch.Tensor | MemoryRegion | KVCacheHandle
        ],
    ) -> None:
        if mr_or_handle_or_tensors is None:
            return
        for x in mr_or_handle_or_tensors:
            if isinstance(x, MemoryRegion):
                x.ref_down()
            elif isinstance(x, KVCacheHandle):
                x.release()

    @nvtx_range("allocate_for", "KVCacheManager")
    def allocate_for(self, *args, **kwargs) -> Status[KVCacheHandle]:
        prefix, query, _ = parse_kvcache_api_args(*args, **kwargs)

        if not MemoryRegion.use_compact_layout():
            key_pairs = list(
                L1Cache.cache_block_keys(prefix, query, self.block_ntokens)
            )
            sizes = tuple(
                ManagedMemoryRegion.calculate_size(
                    self.block_nbytes,
                    len(block_prefix) + len(block_tokens),
                )
                for block_prefix, block_tokens in key_pairs
            )
        else:
            nblocks = len(query) // self.block_ntokens
            sizes = tuple(self.block_nbytes for _ in range(nblocks))

        if self._l1_cache is not None:
            status = self._l1_cache.allocate(sizes)  # type: ignore
        else:
            status = self._allocator.alloc(sizes)  # type: ignore

        if not status.is_ok():
            return Status(status)

        mrs = status.get()
        for mr in mrs:
            mr.block_nbytes = self.block_nbytes  # type: ignore

        return Status.ok(
            MemoryRegionKVCacheHandle(self.block_dtype, self.block_shape, mrs)
        )

    @nvtx_range("put", "KVCacheManager")
    @MeasurableBase.measure(MetricRecorder.OP.PUT)
    def put(self, *args, **kwargs) -> Status[int]:
        prefix, query, kv_tensors = parse_kvcache_api_args(*args, **kwargs)

        if not isinstance(query, TokenListView):
            assert self.block_ntokens % query.block_ntokens == 0, (
                f"kvcache's block size ({self.block_ntokens}) must be multiple "
                f"of cache key's block_ntokens ({query.block_ntokens})"
            )

        pref_len = len(prefix) if prefix is not None else 0
        if pref_len % self.block_ntokens != 0:
            kv_tensors.release()
            return Status(
                StatusCodes.INVALID,
                (
                    f"Prefix length {pref_len} is not aligned to block size "
                    f"{self.block_ntokens}."
                ),
            )

        if self._max_seq_len > 0:
            if pref_len >= self._max_seq_len:
                kv_tensors.release()
                return Status(StatusCodes.DENIED, "Sequence too long")
            elif pref_len + len(query) > self._max_seq_len:
                token_len = round_down(
                    self._max_seq_len - pref_len,
                    self.block_ntokens,
                )
                if token_len == 0:
                    kv_tensors.release()
                    return Status.ok(0)

                # truncate query and kv_tensors
                query = query[:token_len]
                kv_tensors.truncate(token_len // self.block_ntokens)

        if isinstance(kv_tensors, GDRKVCacheHandle):
            assert self.feature.gdr_put, "Does not support GDR put"

        # If L1Cache is enabled, we put kv tensors to L1Cache and leverage its
        # eviction policy to asynchronously ingest kv tensors to L2Cache.
        # Otherwise, we ingest kv tensors to L2Cache directly.
        if self._l1_cache is not None:
            if kv_tensors.memory_region_type is ManagedMemoryRegion:
                mrs = kv_tensors.memory_regions
                status = self._l1_cache.put(prefix, query, mrs)
                # release mrs that are not put to L1Cache
                self._release(mrs[status.get() :])
            else:
                # for a handle with ExternalMemoryRegions, we have to convert
                # it to tensors in order to trigger underlying data copy to
                # ensure L1Cache does not own any external MRs
                tensors = kv_tensors.to_tensors()
                status = self._l1_cache.put(prefix, query, tensors)
                kv_tensors.release()

            if not status.is_ok():
                return status
            return Status.ok(status.get() * self.block_ntokens)
        else:
            return self._l2_put(prefix, query, kv_tensors)

    @nvtx_range("delete", "KVCacheManager")
    def delete(self, *args, **kwargs) -> Status:
        prefix, query, _ = parse_kvcache_api_args(*args, **kwargs)

        if self._l1_cache is not None:
            status = self._l1_cache.delete(prefix, query)
            if not status.is_ok():
                return status

        if self._l2_cache is not None:
            assert self._event_loop is not None
            future = asyncio.run_coroutine_threadsafe(
                self._l2_cache.delete(prefix, query), self._event_loop
            )
            status = future.result()
            if not status.is_ok():
                return status

        return Status.ok()

    @nvtx_range("flush", "KVCacheManager")
    def flush(self) -> Status:
        if self._infight_cv is None:
            return Status.ok()

        try:
            with self._infight_cv:
                while self._l2_inflight_writes > 0:
                    self._infight_cv.wait(timeout=60)
        except TimeoutError:
            # timed out
            return Status(StatusCodes.TIMEOUT)
        except Exception as e:
            # other exceptions
            return Status(StatusCodes.ERROR, value=e)
        return Status.ok()

    def cache_chunk_keys(  # type: ignore
        self, prefix: KVCacheKeyTypes | None, query: KVCacheKeyTypes
    ) -> Iterator[
        Tuple[
            KVCacheKeyTypes,
            KVCacheKeyTypes,
            KVCacheKeyTypes | None,
            KVCacheKeyTypes,
        ]
    ]:
        if prefix is not None:
            all = prefix + query
            prefix_len = len(prefix)
        else:
            all = query
            prefix_len = 0

        num_tokens = len(query)
        aligned_num_tokens = num_tokens - num_tokens % self.block_ntokens
        num_chunks = -(-aligned_num_tokens // self._chunk_size)
        chunk_start = prefix_len
        for _ in range(num_chunks):
            chunk_end = chunk_start + self._chunk_size
            next_chunk_end = chunk_end + self._chunk_size
            yield (
                all[:chunk_start],
                all[chunk_start:chunk_end],
                all[chunk_end:next_chunk_end]
                if next_chunk_end < len(all)
                else None,
                all,
            )
            chunk_start += self._chunk_size


class GroupAwareKVCacheManager(BaseKVCacheManager):
    """Group-aware KV cache manager.

    GroupAwareKVCacheManager uses collectives to ensure all participants
    have the same view towards cache operations.

    Args:
        config: The KV cache config.
        process_group: The process group.
    """

    _COLL_STATUS_ERROR = -1
    _COLL_STATUS_NOT_FOUND = 0

    def __init__(
        self, config: KVCacheConfig, process_group: dist.ProcessGroup
    ) -> None:
        assert dist.is_initialized(), "torch.distributed must be initialized"
        assert process_group is not None, "process_group must be set"

        self.process_group = process_group
        self.world_size = dist.get_world_size(group=process_group)
        self.rank = dist.get_rank(group=process_group)

        if dist.get_backend(process_group) == dist.Backend.NCCL:
            coll_tensor_device = "cuda"
        else:
            coll_tensor_device = "cpu"
        self._coll_tensor = torch.empty(
            (1),
            dtype=torch.int32,
            device=coll_tensor_device,
        )

        super().__init__(config)

    def __repr__(self) -> str:
        return (
            super()
            .__repr__()
            .replace(
                "BaseKVCacheManager",
                f"GroupAwareKVCacheManager[rank={self.rank}, "
                f"world_size={self.world_size}]",
            )
        )

    def __str__(self) -> str:
        return self.__repr__()

    @nvtx_range("acquire", "GroupAwareKVCacheManager")
    @MeasurableBase.measure(MetricRecorder.OP.ACQUIRE)
    def acquire(self, *args, **kwargs) -> Status[Tuple[int, KVCacheHandle]]:
        prefix, query, _ = parse_kvcache_api_args(*args, **kwargs)

        status = self._group_aware_acquire_impl(prefix, query)
        if not status.is_ok():
            return Status(status)
        value = status.get()
        handle = MemoryRegionKVCacheHandle(
            self.block_dtype, self.block_shape, value[1]
        )
        return Status.ok((value[0], handle))

    def _group_aware_acquire_impl(
        self,
        prefix: KVCacheKeyTypes | None,
        query: KVCacheKeyTypes,
    ) -> Status[Tuple[int, Sequence[MemoryRegion]]]:
        """Get kv tensors / cache handles.

        Args:
            prefix: The prefix tokens/block hashes of the kv cache.
            query: The query tokens/block hashes of the kv cache.
        Returns:
            Number of tokens have been fetched from the kv cache service.
            The memory regions corresponding to the given tokens.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        # If it is not a full block, return
        if len(query) // self.block_ntokens == 0:
            return Status(StatusCodes.NOT_FOUND)

        start = 0
        results: List[MemoryRegion] = []
        for chunk_prefix, chunk_tokens, next_tokens, _ in self.cache_chunk_keys(
            prefix, query
        ):
            if next_tokens and len(next_tokens) >= 0:
                # prefetch
                super().prefetch(chunk_prefix + chunk_tokens, next_tokens)
            status = super()._acquire_impl(chunk_prefix, chunk_tokens)
            value = status.get()
            # we only care about num of blocks or if it is an error
            if status.is_ok():
                self._coll_tensor[0] = len(value)
            elif status.is_not_found():
                self._coll_tensor[0] = self._COLL_STATUS_NOT_FOUND
            else:
                self._coll_tensor[0] = self._COLL_STATUS_ERROR
            dist.all_reduce(
                self._coll_tensor,
                op=dist.ReduceOp.MIN,
                group=self.process_group,
            )
            # if any participant encountered an error
            if self._coll_tensor[0] <= 0:
                self._release(value)
                if start > 0:
                    # we have already got some tokens, return success
                    return Status.ok((start, results))
                elif self._coll_tensor[0] == self._COLL_STATUS_NOT_FOUND:
                    return Status(StatusCodes.NOT_FOUND)
                else:
                    # return error
                    return Status(StatusCodes.ERROR)
            elif self._coll_tensor[0] * self.block_ntokens < len(chunk_tokens):
                # some participants have got less tokens than others
                num = self._coll_tensor[0]
                results.extend(value[:num])  # type: ignore
                self._release(value[num:])
                return Status.ok((start + num * self.block_ntokens, results))

            assert len(value) * self.block_ntokens == len(chunk_tokens)
            start += len(chunk_tokens)
            results.extend(status.get())  # type: ignore

        return (
            Status.ok((start, results))
            if start > 0
            else Status(StatusCodes.NOT_FOUND)
        )

    @nvtx_range("get", "GroupAwareKVCacheManager")
    @MeasurableBase.measure(MetricRecorder.OP.GET)
    def get(self, *args, **kwargs) -> Status[int]:
        prefix, query, kv_tensors = parse_kvcache_api_args(*args, **kwargs)

        if isinstance(kv_tensors, GDRKVCacheHandle):
            assert self.feature.gdr_get, "Does not support GDR get"
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        # If it is not a full block, return
        if len(query) // self.block_ntokens == 0:
            return Status(StatusCodes.NOT_FOUND)

        mrs = kv_tensors.memory_regions
        start = 0
        for chunk_prefix, chunk_tokens, _, _ in self.cache_chunk_keys(
            prefix, query
        ):
            chunk_mrs_start = start // self.block_ntokens
            chunk_mrs_end = (
                chunk_mrs_start + len(chunk_tokens) // self.block_ntokens
            )
            chunk_mrs = mrs[chunk_mrs_start:chunk_mrs_end]
            status = super()._get_impl(chunk_prefix, chunk_tokens, chunk_mrs)
            # check if all participants have the same status
            if status.is_ok():
                self._coll_tensor[0] = status.get()
            elif status.is_not_found():
                self._coll_tensor[0] = self._COLL_STATUS_NOT_FOUND
            else:
                self._coll_tensor[0] = self._COLL_STATUS_ERROR
            dist.all_reduce(
                self._coll_tensor,
                op=dist.ReduceOp.MIN,
                group=self.process_group,
            )
            # if any participant encountered an error
            if self._coll_tensor[0] <= 0:
                if start > 0:
                    # we have already got some tokens, return success
                    return Status.ok(start)
                elif self._coll_tensor[0] == self._COLL_STATUS_NOT_FOUND:
                    return Status(StatusCodes.NOT_FOUND)
                else:
                    # return error
                    return Status(StatusCodes.ERROR)
            elif self._coll_tensor[0] < len(chunk_tokens):
                # some participants have got less tokens than others
                return Status.ok(start + self._coll_tensor[0])

            start += len(chunk_tokens)

        return Status.ok(start) if start > 0 else Status(StatusCodes.NOT_FOUND)
