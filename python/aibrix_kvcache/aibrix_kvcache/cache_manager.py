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
from typing import Iterator, List, Sequence, Tuple

import torch
import torch.distributed as dist
import uvloop

from . import envs
from .cache_handle import KVCacheHandle, MemoryRegionKVCacheHandle
from .cache_hashable import TokenCacheKey
from .common import nvtx_range
from .common.absl_logging import getLogger, log_every_n_seconds
from .config import KVCacheConfig
from .l1 import L1Cache
from .l2 import KeyBuilder, L2Cache
from .memory import MemoryRegion, TensorPoolAllocator
from .meta_service import MetaService
from .metrics import KVCacheMetrics, MeasurableBase, MetricRecorder
from .spec import KVCacheBlockLayout, KVCacheBlockSpec
from .status import Status, StatusCodes

logger = getLogger(__name__)

asyncio.set_event_loop_policy(uvloop.EventLoopPolicy())
TESTING_DISABLE_PIN_MEMORY: bool = False


@dataclass
class KVCacheFeature:
    """The features of the kv cache.
    Args:
        zero_copy: Whether the kv cache supports zero-copy.
    """

    zero_copy: bool = False


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
    def chunk_size(self) -> int:
        """Get the chunk size of the kv cache.
        Returns:
            The chunk size of the kv cache.
        """
        raise NotImplementedError

    def prefetch(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> None:
        """(Optional) Prefetch the kv cache for the given prefix and tokens.
        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        """
        pass

    @abstractmethod
    def allocate_for(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Status[KVCacheHandle]:
        """Allocate a cache handle that points to buffers owned
        by the kv cache service.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            The cache handle.
        """
        raise NotImplementedError

    @abstractmethod
    def acquire(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status[Tuple[int, KVCacheHandle]]:
        """Acquire cache handle of the kv tensors for the given prefix and
        tokens.

        The returned cache handle pointing to buffers owned by the kv cache
        service. We can use "KVCacheHandle.to_tensors()" to get tensors sharing
        the same storage. After the kv tensors are used, we need to explicitly
        `ref_down()` the cache handle to let the kv cache service know that
        these buffers are not referenced anymore.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            Number of tokens have been fetched from the kv cache service.
            The cache handles corresponding to the given tokens.
        """
        raise NotImplementedError

    @abstractmethod
    def exists(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Status[int]:
        """Check if the kv cache corresponding to given prefix and
        tokens exists.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            Number of tokens existing in the kv cache service.
        """
        raise NotImplementedError

    @abstractmethod
    def put(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        kv_tensors: KVCacheHandle,
    ) -> Status[int]:
        """Put kv tensors to the kv cache service.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
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
        raise NotImplementedError

    @abstractmethod
    def delete(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status:
        """Delete kv tensors from the kv cache service.
        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            The status of the delete operation.
        """
        raise NotImplementedError

    def flush(self) -> Status:
        """Flush the kv cache service.

        Returns:
            The status of the flush operation.
        """
        return Status.ok()

    @abstractmethod
    def cache_chunk_keys(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Iterator[
        Tuple[
            Tuple[int, ...], Tuple[int, ...], Tuple[int, ...], Tuple[int, ...]
        ]
    ]:
        """Get the cache chunk keys.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        Returns:
            chunk prefix tokens, chunk tokens, next chunk tokens, and all tokens
        """
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
                "chunk_size is too small, using %d instead",
                4 * self.block_ntokens,
            )
            self._chunk_size = 4 * self.block_ntokens

        device: str = envs.AIBRIX_KV_CACHE_OL_DEVICE

        pin_memory: bool = False
        if not TESTING_DISABLE_PIN_MEMORY:
            pin_memory = device == "cpu"

        enable_l1: bool = envs.AIBRIX_KV_CACHE_OL_L1_CACHE_ENABLED
        enable_l2: bool = len(envs.AIBRIX_KV_CACHE_OL_L2_CACHE_BACKEND) > 0
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

            max_mr_nbytes = MemoryRegion.calculate_size(
                self.block_nbytes, self.config.model_spec.max_model_len
            )
            nblocks_per_chunk = self._chunk_size // self.block_ntokens
            # more capacity for async/sync load
            if self._l2_inflight_quota > 0:
                more_capacity_nbytes = self._l2_inflight_quota * max_mr_nbytes
            else:
                more_capacity_nbytes = nblocks_per_chunk * max_mr_nbytes

            # more capacity for get
            more_capacity_nbytes += 2 * nblocks_per_chunk * max_mr_nbytes

            allocator_capacity_nbytes += more_capacity_nbytes

        self._allocator = TensorPoolAllocator(
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

        assert (
            self._l1_cache is not None or self._l2_cache is not None
        ), "At least one cache service must be enabled."

        logger.info("%s is initialized", self)

    @property
    def feature(self) -> KVCacheFeature:
        """Get the feature of the kv cache.
        Returns:
            The feature of the kv cache.
        """
        return KVCacheFeature(zero_copy=True)

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
        key: TokenCacheKey,
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
        prefix = key.prefix
        tokens = key.tokens
        assert len(tokens) == self.block_ntokens
        return self._l2_put(prefix, tokens, mr)

    def _l2_put(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        value: MemoryRegion | KVCacheHandle,
    ) -> Status:
        """Put the kv tensors to the L2Cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
            value: The kv tensors.
        Returns:
            The status of the put operation and the number of tokens have
            been put or scheduled.
        """
        assert self._l2_cache is not None, "l2_cache is not initialized."

        status = None
        if self._l2_inflight_quota == 0:
            # sync write
            status = self._l2_put_sync(prefix, tokens, value)
        else:
            # async write
            status = self._l2_put_async(prefix, tokens, value)

        return status

    def _l2_put_async(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        value: (MemoryRegion | KVCacheHandle),
    ) -> Status:
        """Put the kv tensors to the L2Cache asynchrously.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
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
            self._l2_cache.put(prefix, tokens, value), self._event_loop
        )
        future.add_done_callback(functools.partial(_done_callback, value=value))
        return Status.ok(len(tokens))

    def _l2_put_sync(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        value: (MemoryRegion | KVCacheHandle),
    ) -> Status:
        """Put the kv tensors to the L2Cache blockingly.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
            value: The kv tensors.
        Returns:
            The status of the put operation and the number of tokens have
            been put.
        """
        assert self._l2_cache is not None, "l2_cache is not initialized."
        assert self._event_loop is not None
        future = asyncio.run_coroutine_threadsafe(
            self._l2_cache.put(prefix, tokens, value), self._event_loop
        )
        # wait until the write is done
        status = future.result()

        self._release([value])

        if not status.is_ok():
            return status
        return Status.ok(status.get() * self.block_ntokens)

    def prefetch(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> None:
        """(Optional) Prefetch the kv cache for the given prefix and tokens.
        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        """
        # TODO: implement background prefetching that loads kv cache
        # from L2Cache to L1Cache.
        pass

    @nvtx_range("acquire", "KVCacheManager")
    @MeasurableBase.measure(MetricRecorder.OP.ACQUIRE)
    def acquire(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status[Tuple[int, KVCacheHandle]]:
        """Acquire cache handle of the kv tensors for the given prefix and
        tokens.

        The returned cache handle pointing to buffers owned by the kv cache
        service. We can use "KVCacheHandle.to_tensors()" to get tensors
        sharing the same storage.  After the kv tensors are used, we need to
        explicitly `del` the cache handle to let the kv cache service know that
        these buffers are not referenced anymore.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            Number of tokens have been fetched from the kv cache service.
            The cache handle corresponding to the given tokens.
        """
        status = self._get_impl(prefix, tokens)
        if not status.is_ok():
            return Status(status)

        value = status.get()
        return Status.ok(
            (
                len(value) * self.block_ntokens,
                MemoryRegionKVCacheHandle(
                    self.block_dtype, self.block_shape, value
                ),
            )
        )

    @MeasurableBase.measure(MetricRecorder.OP.EXISTS)
    def exists(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Status[int]:
        """Check if the kv cache corresponding to given prefix and
        tokens exists.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            Number of tokens existing in the kv cache service.
        """
        status = self._exists_impl(prefix, tokens)
        if not status.is_ok():
            return status

        return Status.ok(status.get() * self.block_ntokens)

    def _exists_impl(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Status[int]:
        """Check if the kv cache corresponding to given prefix and
        tokens exists.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            Number of tokens existing in the kv cache service.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        num_blocks = len(tokens) // self.block_ntokens

        # If it is not a full block, return
        if num_blocks == 0:
            return Status(StatusCodes.NOT_FOUND)

        num_existing_blocks = 0
        num_missing_blocks = num_blocks
        l1_status: Status[int] = Status(StatusCodes.NOT_FOUND)
        if self._l1_cache is not None:
            l1_status = self._l1_cache.exists(prefix, tokens)

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
        prefix_curr = [t for t in prefix] if prefix is not None else []
        prefix_curr.extend(tokens[:num_existing_tokens])
        tokens_curr = tokens[num_existing_tokens:]
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

    def _get_impl(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status[Sequence[MemoryRegion]]:
        """Get kv tensors from the kv cache service.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            The memory regions corresponding to the tokens.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        num_blocks = len(tokens) // self.block_ntokens

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
            l1_status = self._l1_cache.acquire(prefix, tokens)

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
                return l1_status

        assert self._l2_cache is not None
        # fetch missing kv tensors from L2Cache
        prefix_curr = [t for t in prefix] if prefix is not None else []
        prefix_curr.extend(tokens[: num_fetched_blocks * self.block_ntokens])
        tokens_curr = tokens[num_fetched_blocks * self.block_ntokens :]
        timeout_s = (
            num_missing_blocks
            * self.block_ntokens
            * self._l2_cache_per_token_timeout_ms
        ) / 1000

        # allocate MRs to hold fetched tensors
        status = self.allocate_for(prefix_curr, tokens_curr)
        if not status.is_ok():
            return status if num_fetched_blocks == 0 else l1_status

        mrs = list(status.get().memory_regions)
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
                for mr in l2_fetched_mrs:
                    mr.ref_up()
                mrs_to_release = l2_fetched_mrs
                put_tokens_curr = tokens_curr[
                    : len(l2_fetched_mrs) * self.block_ntokens
                ]
                put_status = self._l1_cache.put(
                    prefix_curr, put_tokens_curr, l2_fetched_mrs
                )
                if put_status.is_ok():
                    mrs_to_release = l2_fetched_mrs[put_status.get() :]
                self._release(mrs_to_release)

            return Status.ok(fetched_mrs + l2_fetched_mrs)
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
    def allocate_for(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Status[KVCacheHandle]:
        """Allocate a cache handle that points to buffers owned by the kv
        cache service.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            The cache handle.
        """
        if not MemoryRegion.use_compact_layout():
            key_pairs = list(
                L1Cache.cache_block_keys(prefix, tokens, self.block_ntokens)
            )
            sizes = tuple(
                MemoryRegion.calculate_size(
                    self.block_nbytes, len(block_prefix) + len(block_tokens)
                )
                for block_prefix, block_tokens in key_pairs
            )
        else:
            nblocks = len(tokens) // self.block_ntokens
            sizes = tuple(self.block_nbytes for _ in range(nblocks))

        if self._l1_cache is not None:
            status = self._l1_cache.allocate(sizes)  # type: ignore
        else:
            status = self._allocator.alloc(sizes)  # type: ignore

        if not status.is_ok():
            return Status(status)

        mrs = status.get()
        for mr in mrs:
            mr.block_nbytes = self.block_nbytes

        return Status.ok(
            MemoryRegionKVCacheHandle(self.block_dtype, self.block_shape, mrs)
        )

    @nvtx_range("put", "KVCacheManager")
    @MeasurableBase.measure(MetricRecorder.OP.PUT)
    def put(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        kv_tensors: KVCacheHandle,
    ) -> Status[int]:
        """Put kv tensors to the kv cache service.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
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
        # If L1Cache is enabled, we put kv tensors to L1Cache and leverage its
        # eviction policy to asynchronously ingest kv tensors to L2Cache.
        # Otherwise, we ingest kv tensors to L2Cache directly.
        if self._l1_cache is not None:
            mrs = kv_tensors.memory_regions
            status = self._l1_cache.put(prefix, tokens, mrs)
            # release mrs that are not put to L1Cache
            self._release(mrs[status.get() :])

            if not status.is_ok():
                return status
            return Status.ok(status.get() * self.block_ntokens)
        else:
            return self._l2_put(prefix, tokens, kv_tensors)

    @nvtx_range("delete", "KVCacheManager")
    def delete(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status:
        """Delete kv tensors from the kv cache service.
        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            The status of the delete operation.
        """
        if self._l1_cache is not None:
            status = self._l1_cache.delete(prefix, tokens)
            if not status.is_ok():
                return status

        if self._l2_cache is not None:
            assert self._event_loop is not None
            future = asyncio.run_coroutine_threadsafe(
                self._l2_cache.delete(prefix, tokens), self._event_loop
            )
            status = future.result()
            if not status.is_ok():
                return status

        return Status.ok()

    @nvtx_range("flush", "KVCacheManager")
    def flush(self) -> Status:
        """Flush the kv cache service.

        Returns:
            The status of the flush operation.
        """
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

    def cache_chunk_keys(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Iterator[
        Tuple[
            Tuple[int, ...], Tuple[int, ...], Tuple[int, ...], Tuple[int, ...]
        ]
    ]:
        """Get the cache chunk keys.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        Returns:
            chunk prefix tokens, chunk tokens, next chunk tokens, and all tokens
        """
        not_none_prefix = tuple(prefix or [])
        all = tuple(not_none_prefix + tuple(tokens))

        prefix_len = len(not_none_prefix)
        num_tokens = len(tokens)
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
                else tuple(),
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

    def __init__(
        self, config: KVCacheConfig, process_group: dist.ProcessGroup
    ) -> None:
        assert dist.is_initialized(), "torch.distributed must be initialized"
        assert process_group is not None, "process_group must be set"

        self.process_group = process_group
        self.world_size = dist.get_world_size(group=process_group)
        self.rank = dist.get_rank(group=process_group)

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
    def acquire(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status[Tuple[int, KVCacheHandle]]:
        """Acquire cache handle of the kv tensors for the given prefix and
        tokens.

        The returned cache handle pointing to buffers owned by the kv cache
        service. We can use "KVCacheHandle.to_tensors()" to get tensors sharing
        the same storage. After the kv tensors are used, we need to explicitly
        `ref_down()` the cache handle to let the kv cache service know that
        these buffers are not referenced anymore.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            Number of tokens have been fetched from the kv cache service.
            The cache handle corresponding to the given tokens.
        """
        status = self._acquire_impl(prefix, tokens)
        if not status.is_ok():
            return Status(status)
        value = status.get()
        handle = MemoryRegionKVCacheHandle(
            self.block_dtype, self.block_shape, value[1]
        )
        return Status.ok((value[0], handle))

    def _acquire_impl(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status[Tuple[int, Sequence[MemoryRegion]]]:
        """Get kv tensors / cache handles.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
            zero_copy: Whether to use zero-copy.
        Returns:
            Number of tokens have been fetched from the kv cache service.
            The memory regions corresponding to the given tokens.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        # If it is not a full block, return
        if len(tokens) // self.block_ntokens == 0:
            return Status(StatusCodes.NOT_FOUND)

        start = 0
        results: List[MemoryRegion] = []
        for chunk_prefix, chunk_tokens, next_tokens, _ in self.cache_chunk_keys(
            prefix, tokens
        ):
            if next_tokens and len(next_tokens) >= 0:
                # prefetch
                super().prefetch(chunk_prefix + chunk_tokens, next_tokens)
            status = super()._get_impl(chunk_prefix, chunk_tokens)
            value = status.get()
            # we only care about the error code and num of blocks
            coll_status = Status.ok(len(value)) if status.is_ok() else status
            # check if all participants have the same status
            pg_statuses: List[Status] = [Status.ok()] * self.world_size
            dist.all_gather_object(
                pg_statuses, coll_status, group=self.process_group
            )
            # if any participant encountered an error
            if not all([s.is_ok() for s in pg_statuses]):
                self._release(value)
                if start > 0:
                    # we have already got some tokens, return success
                    return Status.ok((start, results))
                else:
                    # return the first error
                    return next(s for s in pg_statuses if not s.is_ok())
            elif not all(
                [
                    s.get() * self.block_ntokens == len(chunk_tokens)
                    for s in pg_statuses
                ]
            ):
                # some participants have got less tokens than others
                num = min(s.get() for s in pg_statuses)
                results.extend(value[:num])
                self._release(value[num:])
                return Status.ok((start + num * self.block_ntokens, results))

            assert len(value) * self.block_ntokens == len(chunk_tokens)
            start += len(chunk_tokens)
            results.extend(status.get())

        return (
            Status.ok((start, results))
            if start > 0
            else Status(StatusCodes.NOT_FOUND)
        )
