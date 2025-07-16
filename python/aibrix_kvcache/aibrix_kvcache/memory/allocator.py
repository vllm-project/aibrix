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
from collections import deque
from dataclasses import dataclass
from threading import Lock
from typing import List, Sequence, Tuple

import numpy as np
import torch
from sortedcontainers import SortedDict, SortedList
from tqdm.auto import tqdm

from .. import envs
from ..cache_hashable import TokenListView
from ..status import Status, StatusCodes
from ..utils import round_up
from .ref_counted_obj import RefCountedObj

MR_USE_COMPACT_LAYOUT = not envs.AIBRIX_KV_CACHE_OL_TOKEN_VALIDATION_ENABLED


@dataclass
class MemoryRegionIntl:
    slab: torch.Tensor
    addr: int
    length: int

    def data_ptr(self) -> int:
        return self.slab.data_ptr() + self.addr

    @staticmethod
    def is_appendable(src: "MemoryRegionIntl", dst: "MemoryRegionIntl") -> bool:
        """
        Check if the src MR can be appended to the dst MR.
        """
        if src is None or dst is None:
            return False

        return (
            src.slab.data_ptr() == dst.slab.data_ptr()
            and src.addr == dst.addr + dst.length
        )

    @staticmethod
    def expand(mr: "MemoryRegionIntl", expand_size: int) -> "MemoryRegionIntl":
        """Expand the MR by the given size.
        Args:
            expand_size (int): The size to be expanded.
        Returns:
            The expanded MR.
        """
        if mr is None:
            return mr

        mr.length += expand_size
        return mr


@dataclass
class MemoryRegionFooter:
    prefix_length: int
    tokens_length: int

    def __init__(self, prefix_length: int, tokens_length: int):
        self.prefix_length = prefix_length
        self.tokens_length = tokens_length
        self._storage = np.array(
            [self.prefix_length, self.tokens_length], dtype=np.int32
        )

    def __post_init__(self):
        if self.prefix_length < 0:
            raise ValueError("prefix_length must be non-negative")
        if self.tokens_length < 0:
            raise ValueError("tokens_length must be non-negative")

    def to_numpy(self) -> np.ndarray:
        return self._storage

    @staticmethod
    def from_numpy(storage: np.ndarray) -> "MemoryRegionFooter":
        return MemoryRegionFooter(
            prefix_length=int(storage[0]), tokens_length=int(storage[1])
        )

    @staticmethod
    def nbytes() -> int:
        return np.dtype(np.int32).itemsize * 2


class MemoryRegion(RefCountedObj):
    """A memory region representation used by Allocator.
    Layout: [cache block, magic, footer, tokens]
    """

    MAGIC: int = 0x3A7F1C42

    def __init__(
        self,
        allocator: "TensorPoolAllocator",
        slab: torch.Tensor,
        addr: int,
        len: int,
    ) -> None:
        super().__init__()
        assert allocator is not None
        self.allocator = allocator
        self.slab = slab
        self.addr = addr
        self.length = len
        self.capacity = len
        self._cached_tensor_view: torch.Tensor | None = None
        self._init_meta()

    def _init_meta(self) -> None:
        self._block_nbytes = -1
        self._is_sealed = False
        self._prefix: TokenListView | None = None
        self._tokens: TokenListView | None = None

    def __len__(self) -> int:
        return self.length

    def __repr__(self) -> str:
        return (
            f"MemoryRegion(addr={self.slab.data_ptr() + self.addr}, "
            f"length={self.length}, capacity={self.capacity}, "
            f"ref={self.ref_count}, sealed={self._is_sealed})"
        )

    def __str__(self) -> str:
        return self.__repr__()

    def __memoryview__(self) -> memoryview:
        """Memoryview protocol support"""
        return memoryview(
            self.slab[self.addr : self.addr + self.length].numpy().data  # type: ignore
        )

    @property
    def block_nbytes(self) -> int:
        """Get the size of the cache block in bytes."""
        assert self._block_nbytes > 0, "block_nbytes must be set"
        return self._block_nbytes

    @block_nbytes.setter
    def block_nbytes(self, block_nbytes: int) -> None:
        """Set the size of the cache block in bytes."""
        assert block_nbytes > 0, "block_nbytes must be positive"
        self._block_nbytes = block_nbytes

    @property
    def is_sealed(self) -> bool:
        """Check if the MR is sealed."""
        return self._is_sealed

    def seal(self) -> None:
        if self._is_sealed:
            return

        if not MR_USE_COMPACT_LAYOUT:
            bytes_per_int = np.dtype(np.int32).itemsize
            start = self.addr + self.block_nbytes
            stop = start + bytes_per_int
            magic = self.slab[start:stop].view(torch.int32).numpy()[0]

            assert magic == MemoryRegion.MAGIC, (
                "Magic mismatch, MUST pack tokens before sealing."
            )

            start = stop
            stop = start + MemoryRegionFooter.nbytes()
            footer = MemoryRegionFooter.from_numpy(
                self.slab[start:stop].view(torch.int32).numpy()
            )
            ntokens = footer.prefix_length + footer.tokens_length
            actual_length = self.calculate_size(self.block_nbytes, ntokens)
            assert actual_length <= self.length, (
                f"{actual_length} > {self.length}"
            )

            self.length = actual_length

        self._is_sealed = True

    def fill(self, data: bytes) -> None:
        assert len(data) == self.length
        self.slab[self.addr : self.addr + len(data)].copy_(
            torch.frombuffer(data, dtype=torch.uint8)
        )

    def tobytes(self) -> bytes:
        tensor = self.slab[self.addr : self.addr + self.length]
        if tensor.is_cuda:
            tensor = tensor.cpu()
        return tensor.numpy().tobytes()

    def data_ptr(self) -> int:
        return self.slab.data_ptr() + self.addr

    def destroy_unsafe(self):
        self._init_meta()
        self.allocator._finalize_mr(self)

    def to_tensor(
        self,
        mr_dtype: torch.dtype | None = None,
        mr_shape: Tuple[int, ...] | None = None,
    ) -> torch.Tensor:
        """Convert MR to tensor"""
        # We use a cached view to reduce the overhead of tensor slicing and
        # view()
        if (
            self._cached_tensor_view is not None
            and mr_dtype is not None
            and self._cached_tensor_view.dtype != mr_dtype
        ):
            self._cached_tensor_view = None
        if (
            self._cached_tensor_view is not None
            and mr_shape is not None
            and self._cached_tensor_view.shape != mr_shape
        ):
            self._cached_tensor_view = None
        if self._cached_tensor_view is not None:
            return self._cached_tensor_view

        ret = self.slab[self.addr : self.addr + self.block_nbytes]
        if mr_dtype is not None:
            ret = ret.view(mr_dtype)
        if mr_shape is not None:
            ret = ret.view(*mr_shape)
        self._cached_tensor_view = ret
        return ret

    @staticmethod
    def to_tensors(
        mrs: Sequence["MemoryRegion"],
        mr_dtype: torch.dtype | None = None,
        mr_shape: Tuple[int, ...] | None = None,
    ) -> Sequence[torch.Tensor]:
        """Convert MRs to tensors. Contiguous MRs are supposed to form
        a single tensor.
        """
        if mrs is None or len(mrs) == 0:
            return []

        return [mr.to_tensor(mr_dtype, mr_shape) for mr in mrs]

    def pack_tokens(
        self,
        *,
        tokens: TokenListView,
        prefix: TokenListView | None = None,
    ) -> None:
        """Pack tokens into the MR.
        Args:
            prefix: The prefix tokens.
            tokens: The tokens to be set.
        """
        ntokens = len(tokens)
        assert ntokens > 0, "tokens must not be empty"

        if MR_USE_COMPACT_LAYOUT:
            self._prefix = prefix
            self._tokens = tokens
            return

        bytes_per_token = np.dtype(np.int32).itemsize

        ntokens_limit = (
            self.length - self.block_nbytes - MemoryRegionFooter.nbytes()
        ) // bytes_per_token - 1
        assert ntokens <= ntokens_limit, (
            f"tokens ({ntokens}) must not exceed the limit ({ntokens_limit})"
        )

        self._prefix = prefix
        self._tokens = tokens

        # Write magic
        start = self.addr + self.block_nbytes
        stop = start + bytes_per_token
        self.slab[start:stop].copy_(
            torch.from_numpy(
                np.array([MemoryRegion.MAGIC], dtype=np.int32)
            ).view(torch.uint8)
        )
        # Write footer
        prefix_length = len(prefix) if prefix is not None else 0
        footer = MemoryRegionFooter(prefix_length, len(tokens))
        start = stop
        stop = start + MemoryRegionFooter.nbytes()
        self.slab[start:stop].copy_(
            torch.from_numpy(footer.to_numpy()).view(torch.uint8)
        )
        # Pack tokens
        if prefix is not None:
            all = prefix + tokens
        else:
            all = tokens
        start = stop
        stop = start + all.nbytes()
        self.slab[start:stop].copy_(
            torch.from_numpy(all.to_numpy()).view(torch.uint8)
        )

    def unpack_tokens(
        self,
    ) -> Tuple[TokenListView | None, TokenListView | None]:
        """Unpack tokens from the MR.
        Returns:
            The prefix and tokens.
        """
        if self._tokens is not None or MR_USE_COMPACT_LAYOUT:
            return self._prefix, self._tokens

        bytes_per_token = np.dtype(np.int32).itemsize
        start = self.addr + self.block_nbytes
        stop = start + bytes_per_token
        magic = self.slab[start:stop].view(torch.int32).numpy()[0]

        if magic != MemoryRegion.MAGIC:
            # corrupted mr or current mr is not packed with tokens
            return None, None

        start = stop
        stop = start + MemoryRegionFooter.nbytes()
        footer = MemoryRegionFooter.from_numpy(
            self.slab[start:stop].view(torch.int32).numpy()
        )

        if footer.prefix_length <= 0:
            prefix = None
        else:
            prefix = TokenListView.from_numpy(
                self.slab[start:stop].view(torch.int32).numpy()
            )

        if footer.tokens_length <= 0:
            return None, None

        start = stop
        stop = start + bytes_per_token * footer.tokens_length
        tokens = TokenListView.from_numpy(
            self.slab[start:stop].view(torch.int32).numpy()
        )

        self._prefix = prefix
        self._tokens = tokens
        return self._prefix, self._tokens

    @staticmethod
    def use_compact_layout() -> bool:
        return MR_USE_COMPACT_LAYOUT

    @staticmethod
    def calculate_size(block_nbytes: int, ntokens: int) -> int:
        """Calculate the size of the MR.
        Args:
            block_nbytes: The size of the cache block in bytes.
            ntokens: The number of tokens.
        Returns:
            The size of the MR in bytes.
        """
        if MR_USE_COMPACT_LAYOUT:
            return block_nbytes
        else:
            # Layout: [cache block, magic, footer, tokens]
            magic_nbytes = np.dtype(np.int32).itemsize
            footer_nbytes = MemoryRegionFooter.nbytes()
            tokens_nbytes = TokenListView.calculate_size(ntokens)
            size = int(
                block_nbytes + magic_nbytes + footer_nbytes + tokens_nbytes
            )
            return round_up(size, TensorPoolAllocator.ALLOC_SIZE_ALIGNMENT)


class TensorPoolAllocator(ABC):
    SLAB_MAX_NBYTES = 1 * 1024**3  # 1GB in bytes
    ALLOC_SIZE_ALIGNMENT = 16

    def __init__(
        self,
        *,
        capacity_nbytes: int,
        device: str = "cpu",
        pin_memory: bool = False,
    ) -> None:
        """Initialize the tensor pool allocator.
        Args:
            capacity_nbytes: The capacity of the allocator in bytes.
            device: The device to allocate the memory on.
            pin_memory: Whether to pin the memory.
        """
        self.capacity_nbytes: int = 0
        self._used_nbytes: int = 0
        self.device: str = "cpu" if device is None else device
        self.pin_memory: bool = pin_memory

        self._lock: Lock = Lock()

        # Fill slabs
        self._slabs: List[torch.Tensor] = []
        self._grow(capacity_nbytes)

    @staticmethod
    def create(
        *,
        capacity_nbytes: int,
        device: str = "cpu",
        pin_memory: bool = False,
    ) -> "TensorPoolAllocator":
        """Create an tensor pool allocator.
        Args:
            capacity_nbytes: The capacity of the allocator in bytes.
            device: The device to allocate the memory on.
            pin_memory: Whether to pin the memory.

        Returns:
            The tensor pool allocator.
        """
        if MR_USE_COMPACT_LAYOUT:
            return ObjectPoolAllocator(
                capacity_nbytes=capacity_nbytes,
                device=device,
                pin_memory=pin_memory,
            )
        else:
            return CoalescingPoolAllocator(
                capacity_nbytes=capacity_nbytes,
                device=device,
                pin_memory=pin_memory,
            )

    def __len__(self) -> int:
        """Return nbytes allocated by the allocator."""
        with self._lock:
            return self._used_nbytes

    @property
    def slabs(self) -> List[torch.Tensor]:
        return self._slabs

    def _grow(self, size_nbytes: int) -> None:
        assert size_nbytes > 0, "size_nbytes must be greater than 0"
        slab_nbytes = self.SLAB_MAX_NBYTES

        size_nbytes = round_up(size_nbytes, slab_nbytes)
        nslabs = size_nbytes // slab_nbytes
        with self._lock:
            self.capacity_nbytes += nslabs * slab_nbytes
            for _ in tqdm(range(nslabs), "Allocating slabs"):
                slab = torch.empty(
                    slab_nbytes,
                    dtype=torch.uint8,
                    device=self.device,
                    pin_memory=self.pin_memory,
                )
                self._slabs.append(slab)
                self._grow_unsafe(slab)

    @abstractmethod
    def _grow_unsafe(self, slab: torch.Tensor) -> None:
        raise NotImplementedError

    def alloc(
        self, sizes: int | Sequence[int]
    ) -> Status[Sequence[MemoryRegion]]:
        if isinstance(sizes, int):
            sizes = (sizes,)

        if len(sizes) == 0:
            return Status(StatusCodes.INVALID)

        num_mrs = len(sizes)
        offset = 0
        with self._lock:
            mrs: List[MemoryRegion] = []
            while len(mrs) < num_mrs:
                status = self._alloc_unsafe(sizes[offset:])
                if status.is_ok():
                    value = status.get()
                    mrs.extend(value)
                    offset += len(value)
                    self._used_nbytes += sum([mr.length for mr in value])
                else:
                    if len(mrs) == 0:
                        return status
                    return Status.ok(mrs)
            return Status.ok(mrs)

    @abstractmethod
    def _alloc_unsafe(
        self, sizes: Sequence[int]
    ) -> Status[Sequence[MemoryRegion]]:
        raise NotImplementedError

    def _finalize_mr(self, mr: MemoryRegion) -> None:
        if mr.capacity <= 0:
            return

        with self._lock:
            self._finalize_mr_unsafe(mr)
            self._used_nbytes -= mr.capacity
            assert self._used_nbytes >= 0, "double free memory region"

    @abstractmethod
    def _finalize_mr_unsafe(self, mr: MemoryRegion) -> None:
        raise NotImplementedError

    @abstractmethod
    def assert_consistency(self) -> None:
        """Assert that the allocator is consistent. For test purpose."""
        raise NotImplementedError

    def __repr__(self) -> str:
        return (
            f"Allocator(capacity_nbytes={self.capacity_nbytes}, "
            f"used={self._used_nbytes}, device={self.device}, "
            f"pin_memory={self.pin_memory})"
        )

    def __str__(self) -> str:
        return self.__repr__()


class CoalescingPoolAllocator(TensorPoolAllocator):
    def __init__(
        self,
        *,
        capacity_nbytes: int,
        device: str = "cpu",
        pin_memory: bool = False,
    ) -> None:
        self._mr_list = SortedList([], key=lambda x: x.data_ptr())
        # Each item is a list of memory regions having the same length
        self._lookup_table = SortedDict()

        super().__init__(
            capacity_nbytes=capacity_nbytes,
            device=device,
            pin_memory=pin_memory,
        )

    def _grow_unsafe(self, slab: torch.Tensor) -> None:
        self._finalize_slab_slice_unsafe(slab, 0, slab.numel())

    def _alloc_unsafe(
        self, sizes: Sequence[int]
    ) -> Status[Sequence[MemoryRegion]]:
        if len(self._lookup_table) == 0:
            return Status(StatusCodes.OUT_OF_MEMORY)

        upper_bound = sum(sizes)
        # Find the first length that is greater than or equal to upper_bound
        idx = self._lookup_table.bisect_left(upper_bound)
        if idx >= len(self._lookup_table):
            # Could not find an MR w/ a size >= upper_bound, just use a
            # smaller one
            idx -= 1

        target_mr_len = self._lookup_table.keys()[idx]
        # Check if the length is valid for at least one size
        if target_mr_len < sizes[0]:
            return Status(StatusCodes.OUT_OF_MEMORY)

        target_mr_list = self._lookup_table[target_mr_len]
        # Get the first memory region from the list
        target_mr = target_mr_list.pop()
        self._mr_list.discard(target_mr)

        # Remove the list if it is empty
        if len(target_mr_list) == 0:
            del self._lookup_table[target_mr_len]

        # Calculate allocated size
        allocated = 0
        nmrs = 0
        for size in sizes:
            if size > target_mr_len - allocated:
                break
            allocated += size
            nmrs += 1

        # Split the memory region if needed
        if target_mr_len > allocated:
            left_over_mr = MemoryRegionIntl(
                slab=target_mr.slab,
                addr=target_mr.addr + allocated,
                length=target_mr.length - allocated,
            )
            self._mr_list.add(left_over_mr)
            self._lookup_table.setdefault(
                left_over_mr.length, SortedList(key=lambda x: x.data_ptr())
            ).add(left_over_mr)

        offset = 0
        mrs = [None] * nmrs
        for i in range(nmrs):
            mrs[i] = MemoryRegion(  # type: ignore
                self, target_mr.slab, target_mr.addr + offset, sizes[i]
            )
            offset += sizes[i]
        return Status.ok(mrs)  # type: ignore

    def _finalize_mr_unsafe(self, mr: MemoryRegion) -> None:
        self._finalize_slab_slice_unsafe(mr.slab, mr.addr, mr.capacity)

    def _finalize_slab_slice_unsafe(
        self, slab: torch.Tensor, addr: int, length: int
    ) -> None:
        if length <= 0:
            return

        mr = MemoryRegionIntl(slab, addr, length)
        # Find the index of the memory region in the list
        idx = self._mr_list.bisect_right(mr)
        prev = self._mr_list[idx - 1] if idx > 0 else None
        next = self._mr_list[idx] if idx < len(self._mr_list) else None

        curr = mr
        # 1. append mr to prev if possible
        if MemoryRegionIntl.is_appendable(curr, prev):
            # Remove prev from the list and lookup table
            self._mr_list.discard(prev)
            prev_len_list = self._lookup_table[prev.length]
            prev_len_list.discard(prev)
            # Remove the list if it is empty
            if len(prev_len_list) == 0:
                del self._lookup_table[prev.length]
            # Append curr to prev
            curr = MemoryRegionIntl.expand(prev, curr.length)

        # curr = prev + mr if append happened
        # 2. append next to curr if possible
        if MemoryRegionIntl.is_appendable(next, curr):
            # Remove next from the list and lookup table
            self._mr_list.discard(next)
            next_len_list = self._lookup_table[next.length]
            next_len_list.discard(next)
            # Remove the list if it is empty
            if len(next_len_list) == 0:
                del self._lookup_table[next.length]
            # Append next to curr
            curr = MemoryRegionIntl.expand(curr, next.length)

        # 3. insert curr into the list and lookup table
        self._mr_list.add(curr)
        self._lookup_table.setdefault(
            curr.length, SortedList(key=lambda x: x.data_ptr())
        ).add(curr)

    @property
    def num_memory_regions(self) -> int:
        """Return the number of memory regions."""
        with self._lock:
            return len(self._mr_list)

    def assert_consistency(self) -> None:
        """Assert that the allocator is consistent. For test purpose."""
        with self._lock:
            # 1. check mr list
            mr_list_total_nbytes = 0
            for i in range(len(self._mr_list)):
                mr_i = self._mr_list[i]
                mr_list_total_nbytes += mr_i.length
                assert mr_i.length > 0, f"{mr_i.length} <= 0"
                assert mr_i.length in self._lookup_table, (
                    f"len={mr_i.length} not in lookup_table"
                )
                assert mr_i in self._lookup_table[mr_i.length], (
                    f"{mr_i} not in lookup_table[{mr_i.length}]"
                )
                if i > 0:
                    mr_i_prev = self._mr_list[i - 1]
                    if (
                        mr_i_prev.slab.data_ptr()
                        + mr_i_prev.addr
                        + mr_i_prev.length
                        == mr_i.slab.data_ptr() + mr_i.addr
                    ):
                        assert mr_i_prev.slab.data_ptr() != mr_i.slab.data_ptr()
                    else:
                        assert (
                            mr_i_prev.slab.data_ptr()
                            + mr_i_prev.addr
                            + mr_i_prev.length
                            < mr_i.slab.data_ptr() + mr_i.addr
                        ), f"{mr_i_prev} and {mr_i} are not disjoint"
            assert (
                mr_list_total_nbytes == self.capacity_nbytes - self._used_nbytes
            ), (
                f"{mr_list_total_nbytes} != {self.capacity_nbytes} - "
                f"{self._used_nbytes}"
            )
            # 2. check lookup table
            lookup_table_total_nbytes = 0
            for mr_len, mr_list in self._lookup_table.items():
                assert mr_len > 0, f"{mr_len} <= 0"
                for mr in mr_list:
                    assert mr_len == mr.length, f"{mr_len} != {mr.length}"
                    assert mr in self._mr_list, f"{mr} not in mr_list"
                lookup_table_total_nbytes += mr_len * len(mr_list)
            assert lookup_table_total_nbytes == mr_list_total_nbytes, (
                f"{lookup_table_total_nbytes} != {mr_list_total_nbytes}"
            )


class ObjectPoolAllocator(TensorPoolAllocator):
    def __init__(
        self,
        *,
        capacity_nbytes: int,
        device: str = "cpu",
        pin_memory: bool = False,
    ) -> None:
        self._free_pool: deque[MemoryRegionIntl] = deque()
        self._reuse_pool: deque[MemoryRegion] = deque()
        self._frag_nbytes: int = 0

        super().__init__(
            capacity_nbytes=capacity_nbytes,
            device=device,
            pin_memory=pin_memory,
        )

    def _grow_unsafe(self, slab: torch.Tensor) -> None:
        self._free_pool.append(MemoryRegionIntl(slab, 0, slab.numel()))

    def _alloc_unsafe(
        self, sizes: Sequence[int]
    ) -> Status[Sequence[MemoryRegion]]:
        if len(self._free_pool) == 0 and len(self._reuse_pool) == 0:
            return Status(StatusCodes.OUT_OF_MEMORY)

        # 1. try to allocate from _free_pool if it is not empty
        if len(self._free_pool) > 0:
            status = self._alloc_unsafe_from_free_pool(sizes)
            if status.is_ok():
                return status

        # 2. try to allocate from _pool
        if len(self._reuse_pool) > 0:
            return self._alloc_unsafe_from_reuse_pool(sizes)

        return Status(StatusCodes.OUT_OF_MEMORY)

    def _alloc_unsafe_from_free_pool(
        self, sizes: Sequence[int]
    ) -> Status[Sequence[MemoryRegion]]:
        # all mrs have uniform size
        mr_size = sizes[0]

        # Get the first memory region from the list
        target_mr = self._free_pool.popleft()
        target_mr_len = target_mr.length

        # Calculate allocated size
        allocated = 0
        nmrs = 0
        for size in sizes:
            if size > target_mr_len - allocated:
                break
            allocated += size
            nmrs += 1

        # Split the memory region if needed
        if (target_mr_len - allocated) >= mr_size:
            left_over_mr = MemoryRegionIntl(
                slab=target_mr.slab,
                addr=target_mr.addr + allocated,
                length=target_mr.length - allocated,
            )
            self._free_pool.appendleft(left_over_mr)
        elif target_mr_len > allocated:
            self._frag_nbytes += target_mr_len - allocated

        offset = 0
        mrs = [None] * nmrs
        for i in range(nmrs):
            mrs[i] = MemoryRegion(  # type: ignore
                self, target_mr.slab, target_mr.addr + offset, sizes[i]
            )
            offset += sizes[i]
        return Status.ok(mrs)  # type: ignore

    def _alloc_unsafe_from_reuse_pool(
        self, sizes: Sequence[int]
    ) -> Status[Sequence[MemoryRegion]]:
        nmrs = min(len(self._reuse_pool), len(sizes))
        mrs: List[MemoryRegion] = []
        for _ in range(nmrs):
            target = self._reuse_pool.popleft()
            target.allocator = self
            target.ref_up()
            mrs.append(target)
        return Status.ok(mrs)

    def _finalize_mr_unsafe(self, mr: MemoryRegion) -> None:
        if mr.capacity <= 0:
            return

        mr.allocator = None  # type: ignore
        self._reuse_pool.append(mr)

    def assert_consistency(self) -> None:
        with self._lock:
            free_pool_nbytes = sum(mr.length for mr in self._free_pool)
            pool_nbytes = sum(mr.length for mr in self._reuse_pool)
            free_nbytes = free_pool_nbytes + pool_nbytes
            expected_free_nbytes = (
                self.capacity_nbytes - self._used_nbytes - self._frag_nbytes
            )
            assert free_nbytes == expected_free_nbytes, (
                f"Free memory ({free_nbytes}) does not match "
                f"un-used capacity ({expected_free_nbytes})"
            )
