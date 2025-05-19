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
import logging
from concurrent.futures import Executor
from typing import Any, Iterator, List, Sequence, Tuple

import torch
from more_itertools import batched

from ..cache_handle import KVCacheHandle
from ..common import nvtx_range
from ..common.absl_logging import getLogger, log_every_n_seconds
from ..memory import MemoryRegion
from ..meta_service import MetaService
from ..metrics import L2CacheMetrics, MeasurableBase, MetricRecorder
from ..spec import KVCacheBlockLayout, KVCacheBlockSpec
from ..status import Status, StatusCodes
from .connectors import Connector, ConnectorConfig
from .key_builders import KeyBuilder, RawKeyBuilder
from .placement import Placement, PlacementConfig

logger = getLogger(__name__)


class L2Cache(MeasurableBase):
    _backend: Connector

    def __init__(
        self,
        backend_name: str,
        placement_policy: str,
        namespace: str,
        block_spec: KVCacheBlockSpec,
        executor: Executor,
        refresh_interval_s: int = 0,
        op_batch: int = 8,
        metrics: L2CacheMetrics | None = None,
        meta_service: MetaService | None = None,
    ) -> None:
        """Create a cache object.
        Args:
            backend_name (str): The name of cache backend.
            placement_policy (str): The placement policy.
            namespace (str): Namespace.
            block_spec (KVCacheBlockSpec): The block spec.
            executor (Executor): The executor.
            refresh_interval_s (int): The refresh interval in seconds.
            op_batch (int): The number of ops in a batch.
            metrics (L2CacheMetrics): metrics recorder.
            meta_service (MetaService): meta service.
        """
        super().__init__(metrics)
        self.block_spec: KVCacheBlockSpec = block_spec
        self.block_layout: KVCacheBlockLayout = self.block_spec.block_layout
        self.block_shape: Tuple[int, ...] = self.block_spec.block_shape
        self.block_dtype: torch.dtype = self.block_spec.block_dtype
        self.block_ntokens: int = self.block_spec.block_ntokens
        self.block_shape_token_dim: int = self.block_spec.block_shape_token_dim
        self.key_builder: KeyBuilder = RawKeyBuilder(self.block_ntokens)
        self.op_batch: int = op_batch
        self._executor: Executor = executor

        cat_head_ids = "_".join(
            [
                str(self.block_spec.tensor_spec.heads[0]),
                str(self.block_spec.tensor_spec.heads[-1]),
            ]
        )
        cat_layer_ids = "_".join(
            [
                str(self.block_spec.tensor_spec.layers[0]),
                str(self.block_spec.tensor_spec.layers[-1]),
            ]
        )
        partition_id = f"h{cat_head_ids}_l{cat_layer_ids}"
        backend_config = ConnectorConfig(
            backend_name=backend_name,
            namespace=namespace,
            partition_id=partition_id,
            executor=executor,
        )

        if meta_service is None:
            # direct mode
            self._backend = Connector.create(backend_config)
        else:
            # cluster mode, using placement policy
            placement_config = PlacementConfig(
                placement_policy=placement_policy,
                conn_config=backend_config,
                meta_service=meta_service,
                refresh_interval_s=refresh_interval_s,
            )
            self._backend = Placement.create(placement_config)

        logger.info(
            "%s is initialized. Using partition_id=%s.", str(self), partition_id
        )

    def __repr__(self) -> str:
        backend_name = "None" if self._backend is None else self._backend.name
        return f"L2Cache(backend={backend_name})"

    def __str__(self) -> str:
        return self.__repr__()

    def __del__(self) -> None:
        self.close()
        logger.info("%s is closed.", str(self))

    def open(self) -> Status:
        """Open the cache."""
        return self._backend.open()

    def close(self) -> Status:
        """Close the cache."""
        if self._backend is not None:
            return self._backend.close()
        return Status.ok()

    def register_slabs(self, slabs: List[torch.Tensor]) -> Status:
        if not self._backend.feature.rdma:
            raise NotImplementedError
        status = self._backend.register_slabs(slabs)
        if not status.is_ok():
            return status
        return status

    @nvtx_range("prefetch", "kv_cache_ol.L2Cache")
    async def prefetch(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status:
        """Prefetch kv tensors from the cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        Returns:
            The status of the prefetch operation.
        """
        if not self._backend.feature.prefetch:
            return Status.ok()

        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        await asyncio.gather(
            *(
                self._prefetch_impl(cache_key)
                for _, cache_key in self._cache_block_keys(prefix, tokens)
            ),
            return_exceptions=False,  # backend returns exception as status
        )
        return Status.ok()

    async def _prefetch_impl(self, cache_key: str | bytes) -> Status:
        await self._backend.prefetch(cache_key)
        return Status.ok()

    @nvtx_range("exists", "kv_cache_ol.L2Cache")
    @MeasurableBase.measure(MetricRecorder.OP.EXISTS)
    async def exists(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status[int]:
        """Check if kv tensors exist in the cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        Returns:
            The number of blocks that exist in the cache.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        total = 0
        for key_batch in self._cache_block_key_batchs(prefix, tokens):
            tasks = []
            async with asyncio.TaskGroup() as tg:
                for real_key, cache_key in key_batch:
                    tasks.append(
                        tg.create_task(self._backend.exists(cache_key))
                    )

            if len(tasks) == 0:
                break

            should_break = False
            for i in range(len(tasks)):
                if not tasks[i].done() or not tasks[i].result().is_ok():
                    should_break = True
                    break
                total += 1

            if should_break:
                break

        if total == 0:
            return Status(StatusCodes.NOT_FOUND)

        return Status.ok(total)

    @nvtx_range("put", "kv_cache_ol.L2Cache")
    @MeasurableBase.measure(MetricRecorder.OP.PUT)
    async def put(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        kv_tensors: (MemoryRegion | Sequence[MemoryRegion] | KVCacheHandle),
    ) -> Status[int]:
        """Put kv tensors to the cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
            kv_tensors: kv tensors or cache handles.
        Returns:
            The status of the put operation and the number of blocks.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        if len(tokens) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        # If it is not a full block, we don't need to cache it.
        if len(tokens) // self.block_ntokens == 0:
            return Status.ok(0)

        if isinstance(kv_tensors, MemoryRegion):
            # `kv_tensors` comes from L1Cache and should be only one block
            assert len(tokens) // self.block_ntokens == 1
            blocks = [kv_tensors]
        elif isinstance(kv_tensors, list):
            assert isinstance(kv_tensors[0], MemoryRegion)
            blocks = kv_tensors
        elif isinstance(kv_tensors, KVCacheHandle):
            if len(tokens) != len(kv_tensors) * self.block_ntokens:
                return Status(
                    StatusCodes.INVALID,
                    (
                        f"Number of tokens {len(tokens)} is not equal to the "
                        f"number of tokens in key tensors "
                        f"{len(kv_tensors) * self.block_ntokens}."
                    ),
                )

            blocks = list(kv_tensors.memory_regions)
        else:
            raise ValueError(f"Unsupported type {type(kv_tensors).__name__}")

        keys = [key for _, key in self._cache_block_keys(prefix, tokens)]
        # use mget if mput_mget is enabled
        if self._backend.feature.mput_mget:
            block_batches = self._backend.get_batches(
                keys, blocks, self.op_batch
            )
            return await self._mput_impl(block_batches)
        else:
            block_batches = tuple(batched(zip(keys, blocks), self.op_batch))
            return await self._put_impl(block_batches)

    async def _mput_impl(
        self, block_batches: Sequence[Sequence[Tuple[Any, MemoryRegion]]]
    ) -> Status[int]:
        num_processed_blocks = 0
        for batch in block_batches:
            num_blocks_in_batch = len(batch)
            statuses = await self._backend.mput(*zip(*batch))

            if isinstance(statuses, list) and all(
                status.is_ok() for status in statuses
            ):
                # all success, continue to the next batch
                num_processed_blocks += num_blocks_in_batch
                continue
            elif num_processed_blocks > 0:
                # current batch is not the first one.
                # at least one batch is done successfully, return success.
                return Status.ok(num_processed_blocks)
            else:
                # this is the first batch and at least one block in
                # current batch is failed, return error.
                if isinstance(statuses, Status):
                    log_every_n_seconds(
                        logger,
                        logging.ERROR,
                        f"mput failed: {statuses}",
                        n_seconds=3,
                    )
                    return statuses

                failures = [status for status in statuses if not status.is_ok()]
                if len(failures) > 0:
                    return failures[0].result()
                return Status(StatusCodes.ERROR)

        return Status.ok(num_processed_blocks)

    async def _put_impl(
        self, block_batches: Sequence[Sequence[Tuple[Any, MemoryRegion]]]
    ) -> Status[int]:
        num_processed_blocks = 0
        for batch in block_batches:
            tasks = []
            num_blocks_in_batch = len(batch)
            async with asyncio.TaskGroup() as tg:
                for cache_key, block in batch:
                    tasks.append(
                        tg.create_task(self._backend.put(cache_key, block))
                    )

            if len(tasks) == 0:
                return Status(StatusCodes.ERROR)
            elif all(task.done() and task.result().is_ok() for task in tasks):
                # all success, continue to the next batch
                num_processed_blocks += num_blocks_in_batch
                continue
            elif num_processed_blocks > 0:
                # current batch is not the first one.
                # at least one batch is done successfully, return success.
                return Status.ok(num_processed_blocks)
            else:
                # this is the first batch and at least one block in
                # current batch is failed, return error.
                failures = [
                    task
                    for task in tasks
                    if task.done() and not task.result().is_ok()
                ]
                if len(failures) > 0:
                    return failures[0].result()
                return Status(StatusCodes.ERROR)

        return Status.ok(num_processed_blocks)

    @nvtx_range("get", "kv_cache_ol.L2Cache")
    @MeasurableBase.measure(MetricRecorder.OP.GET)
    async def get(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        mrs: Sequence[MemoryRegion],
    ) -> Status[int]:
        """Get kv tensors from the cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
            mrs (Sequence[MemoryRegion]): Memory regions to place the fetched
                                          kv tensors.
        Returns:
            The number of blocks that are fetched.
        """
        assert mrs is not None
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        assert len(mrs) == len(tokens) // self.block_ntokens

        keys = [key for _, key in self._cache_block_keys(prefix, tokens)]
        # use mput if mput_mget is enabled
        if self._backend.feature.mput_mget:
            block_batches = self._backend.get_batches(keys, mrs, self.op_batch)
            return await self._mget_impl(block_batches)
        else:
            block_batches = tuple(batched(zip(keys, mrs), self.op_batch))
            return await self._get_impl(block_batches)

    async def _mget_impl(
        self, block_batches: Sequence[Sequence[Tuple[Any, MemoryRegion]]]
    ) -> Status[int]:
        nr = 0
        for batch in block_batches:
            statuses = await self._backend.mget(*zip(*batch))

            should_break = False
            if isinstance(statuses, Status):
                log_every_n_seconds(
                    logger,
                    logging.ERROR,
                    f"mget failed: {statuses}",
                    n_seconds=3,
                )
                statuses = [statuses]
            for status in statuses:
                if not status.is_ok():
                    should_break = True
                    break
                nr += 1

            if should_break:
                break

        if nr == 0:
            return Status(StatusCodes.NOT_FOUND)

        return Status.ok(nr)

    async def _get_impl(
        self, block_batches: Sequence[Sequence[Tuple[Any, MemoryRegion]]]
    ) -> Status[int]:
        nr = 0
        for batch in block_batches:
            tasks = []
            async with asyncio.TaskGroup() as tg:
                for cache_key, mr in batch:
                    tasks.append(
                        tg.create_task(self._backend.get(cache_key, mr))
                    )

            if len(tasks) == 0:
                break

            should_break = False
            for i in range(len(tasks)):
                if not tasks[i].done() or not tasks[i].result().is_ok():
                    should_break = True
                    break
                nr += 1

            if should_break:
                break

        if nr == 0:
            return Status(StatusCodes.NOT_FOUND)

        return Status.ok(nr)

    @nvtx_range("delete", "kv_cache_ol.L2Cache")
    async def delete(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Status:
        """Delete kv tensors from the cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        Returns:
            The status of the delete operation.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        for _, cache_key in self._cache_block_keys(prefix, tokens):
            await self._backend.delete(cache_key)
        return Status.ok()

    def _cache_block_keys(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Iterator[Tuple[Sequence[int], str | bytes]]:
        """Get the cache block keys of the kv tensors.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        Returns:
            The cache block keys of the kv tensors.
        """
        return iter(self.key_builder.build(prefix, tokens))

    def _cache_block_key_batchs(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Iterator[Iterator[Tuple[Sequence[int], str | bytes]]]:
        """Get the cache block key batchs.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        Returns:
            The cache block key batchs of the kv tensors.
        """
        for batch in batched(
            self._cache_block_keys(prefix, tokens), self.op_batch
        ):
            yield iter(batch)
