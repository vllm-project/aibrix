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

import logging
from typing import Iterator, Sequence, Tuple

import torch

from ..cache_hashable import TokenCacheKey
from ..common import nvtx_range
from ..common.absl_logging import getLogger, log_every_n_seconds
from ..memory import MemoryRegion, TensorPoolAllocator
from ..metrics import L1CacheMetrics, MeasurableBase, MetricRecorder
from ..spec import KVCacheBlockSpec
from ..status import Status, StatusCodes
from ..utils import cpu_perf_timer, human_readable_bytes
from .eviction_policy import BaseEvictionPolicy, Functor

logger = getLogger(__name__)


class L1Cache(MeasurableBase):
    def __init__(
        self,
        eviction_policy: str,
        capacity_nbytes: int,
        allocator: TensorPoolAllocator,
        block_spec: KVCacheBlockSpec,
        on_put: Functor | None = None,
        on_evict: Functor | None = None,
        on_hot_access: Functor | None = None,
        metrics: L1CacheMetrics | None = None,
    ) -> None:
        """Create a cache object.
        Args:
            eviction_policy (str): The name of the eviction policy.
            capacity_nbytes (int): The capacity of the cache in bytes.
            allocator (TensorPoolAllocator): The allocator to allocate
                                             cache block.
            on_put(Functor): The callback function to call when putting
                             new items. Defaults to None.
            on_evict(Functor): The evict function to call when evicting
                               items. Defaults to None.
            on_hot_access(Functor): The callback function to call when a
                                    cache item becomes hot. Defaults to None.
            metrics (L1CacheMetrics): The metrics of the cache.
        """
        super().__init__(metrics)
        self.capacity_nbytes: int = capacity_nbytes
        self.allocator: TensorPoolAllocator = allocator
        self.block_spec: KVCacheBlockSpec = block_spec
        self.block_shape: Tuple[int, ...] = self.block_spec.block_shape
        self.block_dtype: torch.dtype = self.block_spec.block_dtype
        self.block_ntokens: int = self.block_spec.block_ntokens
        self.block_nbytes: int = self.block_spec.block_nbytes
        self.block_shape_token_dim: int = self.block_spec.block_shape_token_dim

        self._eviction_policy: BaseEvictionPolicy = BaseEvictionPolicy.create(
            eviction_policy,
            capacity_nbytes,
            on_put=on_put,
            on_evict=on_evict,
            on_hot_access=on_hot_access,
        )

        assert self.allocator.capacity_nbytes >= self.capacity_nbytes, (
            f"Allocator capacity {self.allocator.capacity_nbytes} should not "
            f"be less than cache capacity {self.capacity_nbytes}."
        )

        logger.info("%s is initialized.", str(self))

    def __len__(self) -> int:
        """Return the usage of the cache in bytes."""
        return len(self._eviction_policy)

    def __repr__(self) -> str:
        return (
            f"L1Cache(policy={self._eviction_policy.name}"
            f", capacity_nbytes={human_readable_bytes(self.capacity_nbytes)}"
            f", size={human_readable_bytes(len(self))})"
        )

    def __str__(self) -> str:
        return self.__repr__()

    def set_on_put_callback(self, functor: Functor) -> None:
        """Set the callback function to call when putting new items."""
        self._eviction_policy.set_on_put_callback(functor)

    def set_on_evict_callback(self, on_evict: Functor) -> None:
        """Set the callback function to call when evicting items."""
        self._eviction_policy.set_on_evict_callback(on_evict)

    def set_on_hot_access_callback(self, on_hot_access: Functor) -> None:
        """Set the callback function to call when a cache item becomes hot."""
        self._eviction_policy.set_on_hot_access_callback(on_hot_access)

    def allocate(
        self,
        sizes: Sequence[int],
    ) -> Status[Sequence[MemoryRegion]]:
        """Allocate a set of memory regions.

        Args:
            sizes: The sizes of the memory regions.
        Returns:
            The memory regions.
        """
        if self._recorder:
            self._recorder.trace_usage(  # type: ignore[attr-defined]
                MetricRecorder.Resource.L1_ALLOCATOR,
                self.allocator._used_nbytes,
            )
            self._recorder.trace_usage(  # type: ignore[attr-defined]
                MetricRecorder.Resource.L1_EVICTION_POLICY,
                len(self._eviction_policy),
            )

        total = sum(sizes)

        status = self.allocator.alloc(sizes)
        while status.is_out_of_memory() and len(self) > 0:
            self._eviction_policy.evict(total)
            status = self.allocator.alloc(sizes)

        return Status(status)

    @nvtx_range("exists", "kv_cache_ol.L1Cache")
    @MeasurableBase.measure(MetricRecorder.OP.EXISTS)
    def exists(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status[int]:
        """Check if the kv cache corresponding to given prefix and
        tokens exists.

        Args:
            prefix: The prefix of the kv cache. E.g., [1, 2, 3]
            tokens: The tokens of the kv cache. E.g., [4, 5, 6, 7]
        Returns:
            Number of blocks existing in the kv cache service.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        total = 0
        for key in self._cache_block_keys(prefix, tokens):
            cache_key = TokenCacheKey(*key)
            if cache_key in self._eviction_policy:
                total += 1
            else:
                break
        return Status.ok(total) if total > 0 else Status(StatusCodes.NOT_FOUND)

    @nvtx_range("put", "kv_cache_ol.L1Cache")
    @MeasurableBase.measure(MetricRecorder.OP.PUT)
    def put(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        kv_tensors: torch.Tensor
        | Sequence[torch.Tensor]
        | Sequence[MemoryRegion],
    ) -> Status[int]:
        """Put kv tensors to the cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
            kv_tensors: The kv tensors.
        Returns:
            The status of the put operation and the number of blocks.
        """
        if isinstance(kv_tensors, torch.Tensor) or isinstance(
            kv_tensors[0], torch.Tensor
        ):
            return self._put_tensors_impl(prefix, tokens, kv_tensors)  # type: ignore[arg-type]
        else:
            return self._put_mrs_impl(prefix, tokens, kv_tensors)  # type: ignore[arg-type]

    def _put_tensors_impl(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        kv_tensors: torch.Tensor | Sequence[torch.Tensor],
    ) -> Status[int]:
        """Put kv tensors to the cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
            kv_tensors (torch.Tensor | Sequence[torch.Tensor]): The kv tensors.
        Returns:
            The status of the put operation and the number of blocks.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(
                StatusCodes.INVALID,
                (
                    f"Prefix tokens {prefix} is not aligned to block size "
                    f"{self.block_ntokens}."
                ),
            )

        if isinstance(kv_tensors, torch.Tensor):
            if len(tokens) != kv_tensors.shape[self.block_shape_token_dim]:
                return Status(
                    StatusCodes.INVALID,
                    (
                        f"Number of tokens {len(tokens)} is not equal to the "
                        f"number of tokens in key tensors "
                        f"{kv_tensors.shape[self.block_shape_token_dim]}."
                    ),
                )
        else:
            if kv_tensors[0].shape != self.block_shape:
                return Status(
                    StatusCodes.INVALID,
                    (
                        f"Key tensors shape {kv_tensors[0].shape} is not equal "
                        f"to block shape {self.block_shape}."
                    ),
                )
            if len(tokens) != len(kv_tensors) * self.block_ntokens:
                return Status(
                    StatusCodes.INVALID,
                    (
                        f"Number of tokens {len(tokens)} is not equal to the "
                        f"number of tokens in key tensors "
                        f"{len(kv_tensors) * self.block_ntokens}."
                    ),
                )

        # If it is not a full block, we don't need to cache it.
        if len(tokens) // self.block_ntokens == 0:
            return Status.ok(0)

        num_tokens = len(tokens)
        num_blocks = num_tokens // self.block_ntokens

        sizes = [
            MemoryRegion.calculate_size(
                self.block_nbytes, len(block_prefix) + len(block_tokens)
            )
            for block_prefix, block_tokens in self._cache_block_keys(
                prefix, tokens
            )
        ]

        status = self.allocate(sizes)
        if not status.is_ok():
            return Status(status)

        if isinstance(kv_tensors, torch.Tensor):
            kv_blocks = [None] * num_blocks
            offset = 0
            slices = [slice(None)] * len(self.block_shape)
            for i in range(num_blocks):
                slices[self.block_shape_token_dim] = slice(
                    offset, offset + self.block_ntokens
                )
                kv_blocks[i] = kv_tensors[tuple(slices)]  # type: ignore
                offset += self.block_ntokens
        else:
            kv_blocks = kv_tensors  # type: ignore

        block_mrs = status.get()
        block_mr_shape = [s for s in self.block_shape]
        block_mr_shape[self.block_shape_token_dim] = self.block_ntokens
        with cpu_perf_timer() as get_copy_dur_ms:
            for i, block_mr in enumerate(block_mrs):
                block_mr.block_nbytes = self.block_nbytes
                cached_tensors = block_mr.to_tensor(
                    self.block_dtype, tuple(block_mr_shape)
                )
                cached_tensors.copy_(kv_blocks[i])  # type: ignore
        log_every_n_seconds(
            logger,
            logging.INFO,
            f"Copying kv tensors takes {get_copy_dur_ms():.4f} ms",
            n_seconds=10,
        )

        put_status = self._put_mrs_impl(
            prefix, tokens, block_mrs, with_check=False
        )
        if not put_status.is_ok():
            # failed to put all the blocks, release the allocated MRs
            [mr.ref_down() for mr in block_mrs]
        else:
            # release the MRs that are not put to the cache
            [mr.ref_down() for mr in block_mrs[put_status.get() :]]
        return put_status

    def _put_mrs_impl(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        kv_mrs: Sequence[MemoryRegion],
        with_check: bool = True,
    ) -> Status[int]:
        """Put kv mrs to the cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv mrs.
            tokens (Sequence[int]): The tokens of the kv mrs.
            kv_mrs (Sequence[MemoryRegion]): The kv memory regions.
            with_check (bool): Whether to check the validity of the kv mrs.
        Returns:
            The status of the put operation and the number of blocks.
        """
        num_tokens = len(tokens)
        num_blocks = len(kv_mrs)

        if with_check:
            if prefix is not None and len(prefix) % self.block_ntokens != 0:
                return Status(
                    StatusCodes.INVALID,
                    (
                        f"Prefix tokens {prefix} is not aligned to block size "
                        f"{self.block_ntokens}."
                    ),
                )

            if num_tokens != num_blocks * self.block_ntokens:
                return Status(
                    StatusCodes.INVALID,
                    (
                        f"Number of tokens {num_tokens} is not equal to the "
                        f"number of tokens in key tensors "
                        f"{num_blocks * self.block_ntokens}."
                    ),
                )

            # If it is not a full block, we don't need to cache it.
            if num_tokens // self.block_ntokens == 0:
                return Status.ok(0)

        assert len(kv_mrs) == num_blocks

        bi = 0
        for block_prefix, block_tokens in self._cache_block_keys(
            prefix, tokens[: num_blocks * self.block_ntokens]
        ):
            if bi >= len(kv_mrs):
                break
            block_mr = kv_mrs[bi]
            if not MemoryRegion.use_compact_layout():
                block_mr.pack_tokens(prefix=block_prefix, tokens=block_tokens)
            block_mr.seal()
            block_key = TokenCacheKey(block_prefix, block_tokens)
            if not self._eviction_policy.put(block_key, block_mr).is_ok():
                break
            bi += 1

        return Status.ok(bi)

    @nvtx_range("acquire", "kv_cache_ol.L1Cache")
    @MeasurableBase.measure(MetricRecorder.OP.ACQUIRE)
    def acquire(
        self,
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
    ) -> Status[Sequence[MemoryRegion]]:
        """Acquire cache handle pointing to the kv tensors such that the
        upper layer can access these tensors in a zero-copy way.

        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        Returns:
            The memory regions corresponding to the tokens.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        mrs = []
        for key in self._cache_block_keys(prefix, tokens):
            status = self._eviction_policy.get(TokenCacheKey(*key))
            if status.is_ok():
                mrs.append(status.value)
            else:
                break

        if len(mrs) == 0:
            return Status(StatusCodes.NOT_FOUND)

        return Status.ok(mrs)  # type: ignore

    @nvtx_range("delete", "kv_cache_ol.L1Cache")
    def delete(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Status:
        """Delete kv tensors from the cache.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        """
        if prefix is not None and len(prefix) % self.block_ntokens != 0:
            return Status(StatusCodes.INVALID)

        for key in self._cache_block_keys(prefix, tokens):
            self._eviction_policy.delete(TokenCacheKey(*key))
        return Status.ok()

    def _cache_block_keys(
        self, prefix: Sequence[int] | None, tokens: Sequence[int]
    ) -> Iterator[Tuple[Tuple[int, ...], Tuple[int, ...]]]:
        """Get the cache block keys of the kv tensors.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
        Returns:
            The cache block keys of the kv tensors.
        """
        return L1Cache.cache_block_keys(prefix, tokens, self.block_ntokens)

    @staticmethod
    def cache_block_keys(
        prefix: Sequence[int] | None,
        tokens: Sequence[int],
        block_ntokens: int,
    ) -> Iterator[Tuple[Tuple[int, ...], Tuple[int, ...]]]:
        """Get the cache block keys of the kv tensors.
        Args:
            prefix (Sequence[int] | None): The prefix tokens of the kv tensors.
            tokens (Sequence[int]): The tokens of the kv tensors.
            block_ntokens (int): The number of tokens in a block.
        Returns:
            The cache block keys of the kv tensors.
        """
        not_none_prefix = tuple(prefix or [])
        all = tuple(not_none_prefix + tuple(tokens))

        cache_key_len = len(not_none_prefix)
        num_blocks = len(tokens) // block_ntokens
        for _ in range(num_blocks):
            yield (
                all[:cache_key_len],
                all[cache_key_len : cache_key_len + block_ntokens],
            )
            cache_key_len += block_ntokens
