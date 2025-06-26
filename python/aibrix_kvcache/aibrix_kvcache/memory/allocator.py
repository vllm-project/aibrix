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

from dataclasses import dataclass
from threading import Lock
from typing import List, Sequence, Tuple

import numpy as np
import torch
from sortedcontainers import SortedDict, SortedList

from .. import envs
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
        self._init_meta()

    def _init_meta(self) -> None:
        self._block_nbytes = -1
        self._is_sealed = False
        self._prefix: Tuple[int, ...] | None = None
        self._tokens: Tuple[int, ...] = tuple()

    def __len__(self) -> int:
        return self.length

    def __repr__(self) -> str:
        return (
            f"MemoryRegion(addr={self.slab.data_ptr() + self.addr}, "
            f"length={self.length}, ref={self.ref_count}, "
            f"sealed={self._is_sealed})"
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

            assert (
                magic == MemoryRegion.MAGIC
            ), "Magic mismatch, MUST pack tokens before sealing."

            start = stop
            stop = start + MemoryRegionFooter.nbytes()
            footer = MemoryRegionFooter.from_numpy(
                self.slab[start:stop].view(torch.int32).numpy()
            )
            ntokens = footer.prefix_length + footer.tokens_length
            actual_length = self.calculate_size(self.block_nbytes, ntokens)
            assert (
                actual_length <= self.length
            ), f"{actual_length} > {self.length}"

            if actual_length < self.length:
                # return the rest of the MR
                self.allocator._finalize_mr(
                    self.slab,
                    self.addr + actual_length,
                    self.length - actual_length,
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
        self.allocator._finalize_mr(self.slab, self.addr, self.length)

    def to_tensor(
        self,
        mr_dtype: torch.dtype | None = None,
        mr_shape: Tuple[int, ...] | None = None,
    ) -> torch.Tensor:
        """Convert MR to tensor"""
        ret = self.slab[self.addr : self.addr + self.block_nbytes]
        if mr_dtype is not None:
            ret = ret.view(mr_dtype)
        if mr_shape is not None:
            ret = ret.view(*mr_shape)
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
        tokens: Tuple[int, ...],
        prefix: Tuple[int, ...] | None = None,
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
        assert (
            ntokens <= ntokens_limit
        ), f"tokens ({ntokens}) must not exceed the limit ({ntokens_limit})"

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
        all = np.array((prefix or tuple()) + tokens, dtype=np.int32)
        start = stop
        stop = start + bytes_per_token * len(all)
        self.slab[start:stop].copy_(torch.from_numpy(all).view(torch.uint8))

    def unpack_tokens(self) -> Tuple[Tuple[int, ...] | None, Tuple[int, ...]]:
        """Unpack tokens from the MR.
        Returns:
            The prefix and tokens.
        """
        if len(self._tokens) > 0 or MR_USE_COMPACT_LAYOUT:
            return self._prefix, self._tokens

        bytes_per_token = np.dtype(np.int32).itemsize
        start = self.addr + self.block_nbytes
        stop = start + bytes_per_token
        magic = self.slab[start:stop].view(torch.int32).numpy()[0]

        if magic != MemoryRegion.MAGIC:
            # corrupted mr or current mr is not packed with tokens
            return None, tuple()

        start = stop
        stop = start + MemoryRegionFooter.nbytes()
        footer = MemoryRegionFooter.from_numpy(
            self.slab[start:stop].view(torch.int32).numpy()
        )

        if footer.prefix_length <= 0:
            prefix = None
        else:
            start = stop
            stop = start + bytes_per_token * footer.prefix_length
            prefix = tuple(
                self.slab[start:stop].view(torch.int32).numpy().tolist()
            )

        if footer.tokens_length <= 0:
            return None, tuple()

        start = stop
        stop = start + bytes_per_token * footer.tokens_length
        tokens = tuple(self.slab[start:stop].view(torch.int32).numpy().tolist())

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
            tokens_nbytes = np.dtype(np.int32).itemsize * ntokens
            size = int(
                block_nbytes + magic_nbytes + footer_nbytes + tokens_nbytes
            )
            return round_up(size, TensorPoolAllocator.ALLOC_SIZE_ALIGNMENT)


class TensorPoolAllocator:
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

        self._mr_list = SortedList([], key=lambda x: x.data_ptr())
        # Each item is a list of memory regions having the same length
        self._lookup_table = SortedDict()

        self._lock: Lock = Lock()

        # Fill slabs
        self._slabs: List[torch.Tensor] = []
        self._grow(capacity_nbytes)

    def __len__(self) -> int:
        """Return nbytes allocated by the allocator."""
        with self._lock:
            return self._used_nbytes

    def __repr__(self) -> str:
        return (
            f"TensorPoolAllocator(capacity_nbytes={self.capacity_nbytes}, "
            f"used={self._used_nbytes}, device={self.device}, "
            f"pin_memory={self.pin_memory})"
        )

    def __str__(self) -> str:
        return self.__repr__()

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
            for i in range(nslabs):
                slab = torch.empty(
                    slab_nbytes,
                    dtype=torch.uint8,
                    device=self.device,
                    pin_memory=self.pin_memory,
                )
                self._slabs.append(slab)
                self._used_nbytes += slab.numel()
                self._finalize_mr_unsafe(slab, 0, slab.numel())

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
                else:
                    if len(mrs) == 0:
                        return status
                    return Status.ok(mrs)
            return Status.ok(mrs)

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
        self._used_nbytes += allocated
        return Status.ok(mrs)  # type: ignore

    def _finalize_mr(self, slab: torch.Tensor, addr: int, length: int) -> None:
        if length <= 0:
            return

        with self._lock:
            return self._finalize_mr_unsafe(slab, addr, length)

    def _finalize_mr_unsafe(
        self, slab: torch.Tensor, addr: int, length: int
    ) -> None:
        if length <= 0:
            return

        mr = MemoryRegionIntl(slab, addr, length)
        self._used_nbytes -= mr.length
        assert self._used_nbytes >= 0, "double free memory region"
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
                assert (
                    mr_i.length in self._lookup_table
                ), f"len={mr_i.length} not in lookup_table"
                assert (
                    mr_i in self._lookup_table[mr_i.length]
                ), f"{mr_i} not in lookup_table[{mr_i.length}]"
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
            assert (
                lookup_table_total_nbytes == mr_list_total_nbytes
            ), f"{lookup_table_total_nbytes} != {mr_list_total_nbytes}"
