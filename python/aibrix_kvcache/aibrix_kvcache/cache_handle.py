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

from abc import ABC, abstractmethod
from typing import Callable, List, Sequence, Tuple

import torch

from .memory import MemoryRegion
from .memory.external_memory_region import ExternalMemoryRegion
from .spec import KVCacheBlockLayout, KVCacheBlockSpec


class KVCacheHandle(ABC):
    """Cache handle to support zero-copy APIs."""

    @property
    @abstractmethod
    def memory_regions(
        self,
    ) -> Sequence[MemoryRegion] | Sequence[Sequence[MemoryRegion]]:
        raise NotImplementedError

    @property
    @abstractmethod
    def memory_region_type(self) -> type[MemoryRegion]:
        raise NotImplementedError

    @abstractmethod
    def to_tensors(self) -> Sequence[torch.Tensor | Sequence[torch.Tensor]]:
        raise NotImplementedError

    @abstractmethod
    def release(self) -> None:
        raise NotImplementedError

    @abstractmethod
    def truncate(self, length: int) -> None:
        raise NotImplementedError

    @abstractmethod
    def __len__(self) -> int:
        raise NotImplementedError


class MemoryRegionKVCacheHandle(KVCacheHandle):
    def __init__(
        self,
        block_dtype: torch.dtype,
        block_shape: Tuple[int, ...],
        mrs: Sequence[MemoryRegion],
    ) -> None:
        self._block_dtype = block_dtype
        self._block_shape = block_shape
        self._mrs = mrs

        assert len(mrs) > 0, "mrs should not be empty"
        self._mr_type = type(self._mrs[0])

        for mr in self._mrs[1:]:
            assert type(mr) is self._mr_type

    @property
    def memory_regions(self) -> Sequence[MemoryRegion]:
        return self._mrs

    @property
    def memory_region_type(self) -> type[MemoryRegion]:
        return self._mr_type

    def to_tensors(self) -> Sequence[torch.Tensor]:
        return MemoryRegion.to_tensors(
            self._mrs, self._block_dtype, self._block_shape
        )

    def release(self) -> None:
        for mr in self._mrs:
            mr.ref_down()

    def truncate(self, length: int) -> None:
        for mr in self._mrs[length:]:
            mr.ref_down()
        self._mrs = self._mrs[:length]

    def __len__(self) -> int:
        return len(self._mrs)

    @staticmethod
    def create(
        blocks: Sequence[torch.Tensor],
        block_spec: KVCacheBlockSpec,
        slot_mapping: Sequence[int] | torch.Tensor,
        on_mr_release: Callable[[torch.Tensor, int, int], None] | None = None,
    ) -> KVCacheHandle:
        """
        Create a external MR backed MemoryRegionKVCacheHandle for the given
        token list.

        Args:
            blocks: Blocks that have been registered via register_kvcache.
            block_spec: KVCache block spec.
            slot_mapping: Mapping from tokens to blocks.
            on_mr_release: Callback function when memory region is released.

        Returns:
            KVCacheHandle.
        """
        block_ntokens = block_spec.block_ntokens
        block_nbytes = block_spec.block_nbytes
        block_dtype = block_spec.block_dtype
        block_shape = block_spec.block_shape
        kvcache_block_layout = block_spec.block_layout

        if block_ntokens != block_spec.engine_block_ntokens:
            assert kvcache_block_layout == KVCacheBlockLayout.NCLD, (
                "Need to use NCLD layout to support using different block "
                "sizes for engine's kvcache and AIBrix kvcache"
            )

        total_blocks = len(blocks)
        assert total_blocks > 0, "blocks must not be empty"

        ntokens = (
            len(slot_mapping)
            if isinstance(slot_mapping, Sequence)
            else slot_mapping.shape[0]
        )

        num_blocks = ntokens // block_ntokens
        mrs: List[ExternalMemoryRegion] = [None for _ in range(num_blocks)]  # type: ignore
        for i in range(0, ntokens, block_ntokens):
            block_id = slot_mapping[i] // block_ntokens
            block_off = slot_mapping[i] % block_ntokens
            slab = blocks[block_id].flatten().view(torch.uint8)
            mr = ExternalMemoryRegion(
                slab=slab,
                addr=int(
                    blocks[block_id][block_off].data_ptr() - slab.data_ptr()
                ),
                length=block_nbytes,
                on_release=on_mr_release,
            )

            mrs[i // block_ntokens] = mr

        return MemoryRegionKVCacheHandle(block_dtype, block_shape, mrs)


class GDRKVCacheHandle(KVCacheHandle):
    def __init__(
        self,
        mr_dtype: torch.dtype,
        mr_shape: Tuple[int, ...],
        mrs: Sequence[Sequence[MemoryRegion]],
    ) -> None:
        self._mr_dtype = mr_dtype
        self._mr_shape = mr_shape
        self._mrs = mrs

        assert len(mrs) > 0, "mrs should not be empty"
        assert len(mrs[0]) > 0, "mrs[0] should not be empty"
        self._mr_type = type(mrs[0][0])

    @property
    def memory_regions(self) -> Sequence[Sequence[MemoryRegion]]:
        return self._mrs

    @property
    def memory_region_type(self) -> type[MemoryRegion]:
        return self._mr_type

    def to_tensors(self) -> Sequence[Sequence[torch.Tensor]]:
        return [
            MemoryRegion.to_tensors(mrs, self._mr_dtype, self._mr_shape)
            for mrs in self._mrs
        ]

    def release(self) -> None:
        for block_mrs in self._mrs:
            for mr in block_mrs:
                mr.ref_down()

    def truncate(self, length: int) -> None:
        for block_mrs in self._mrs[length:]:
            for mr in block_mrs:
                mr.ref_down()
        self._mrs = self._mrs[:length]

    def __len__(self) -> int:
        return len(self._mrs)

    @staticmethod
    def create(
        blocks: Sequence[torch.Tensor],
        block_spec: KVCacheBlockSpec,
        slot_mapping: Sequence[int] | torch.Tensor,
        on_mr_release: Callable[[torch.Tensor, int, int], None] | None = None,
    ) -> KVCacheHandle:
        """
        Create a KVCacheHandle for the given token list.

        Args:
            blocks: Blocks to be scattered/gathered. Dim: (num_layers, 2,
                    num_blocks, block_size, num_kv_heads, head_size)
            block_spec: KVCache block spec.
            slot_mapping: Mapping from tokens to blocks.
            on_mr_release: Callback function when memory region is released.

        Returns:
            KVCacheHandle.
        """
        assert block_spec.engine_block_ntokens == block_spec.block_ntokens, (
            "engine KVCache and AIBrix KVCache should use the same "
            "block_size to enable GDR"
        )
        block_ntokens = block_spec.block_ntokens
        block_nbytes = block_spec.block_nbytes
        block_dtype = block_spec.block_dtype
        block_shape = block_spec.block_shape
        kvcache_block_layout = block_spec.block_layout

        num_layers = len(blocks)
        assert num_layers > 0, "blocks must not be empty"

        ntokens = (
            len(slot_mapping)
            if isinstance(slot_mapping, Sequence)
            else slot_mapping.shape[0]
        )
        assert ntokens % block_ntokens == 0, (
            "size of slot_mapping must be divisible by block_ntokens"
        )

        num_blocks = ntokens // block_ntokens
        if kvcache_block_layout == KVCacheBlockLayout.LCND:
            layer_nbytes_per_cache_type = block_nbytes // num_layers // 2
            mrs: List[List[ExternalMemoryRegion]] = [
                [
                    None  # type: ignore
                    for _ in range(num_layers * 2)
                ]  # num_layers * 2 (k & v)
                for _ in range(num_blocks)
            ]
            for i in range(num_layers):
                slab = blocks[i].flatten().view(torch.uint8)
                for j in range(0, ntokens, block_ntokens):
                    block_id = slot_mapping[j] // block_ntokens
                    kmr = ExternalMemoryRegion(
                        slab=slab,
                        addr=int(
                            blocks[i][0][block_id].data_ptr() - slab.data_ptr()
                        ),
                        length=layer_nbytes_per_cache_type,
                        on_release=on_mr_release,
                    )

                    vmr = ExternalMemoryRegion(
                        slab=slab,
                        addr=int(
                            blocks[i][1][block_id].data_ptr() - slab.data_ptr()
                        ),
                        length=layer_nbytes_per_cache_type,
                        on_release=on_mr_release,
                    )
                    mrs[j // block_ntokens][i * 2] = kmr
                    mrs[j // block_ntokens][i * 2 + 1] = vmr

            mr_shape = block_shape[2:]
            return GDRKVCacheHandle(block_dtype, mr_shape, mrs)
        else:
            raise ValueError("Only LCND is supported for now")
