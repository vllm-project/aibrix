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

from typing import Sequence, Tuple

import torch

from .. import envs
from .ref_counted_obj import RefCountedObj

MR_USE_COMPACT_LAYOUT = not envs.AIBRIX_KV_CACHE_OL_TOKEN_VALIDATION_ENABLED


class MemoryRegion(RefCountedObj):
    """Memory region represents a continuous memory buffer."""

    def __init__(
        self,
        slab: torch.Tensor,
        addr: int,
        len: int,
    ) -> None:
        super().__init__()
        self.slab = slab
        self.addr = addr
        self.length = len
        self.capacity = len
        self._cached_tensor_view: torch.Tensor | None = None

    def __len__(self) -> int:
        return self.length

    def __repr__(self) -> str:
        return (
            f"MemoryRegion(addr={self.slab.data_ptr() + self.addr}, "
            f"length={self.length}, capacity={self.capacity})"
        )

    def __str__(self) -> str:
        return self.__repr__()

    @property
    def block_nbytes(self) -> int:
        """Get the size of the cache block in bytes."""
        return self.length

    @staticmethod
    def use_compact_layout() -> bool:
        return MR_USE_COMPACT_LAYOUT

    def tobytes(self) -> bytes:
        tensor = self.slab[self.addr : self.addr + self.length]
        if tensor.is_cuda:
            tensor = tensor.cpu()
        return tensor.numpy().tobytes()

    def data_ptr(self) -> int:
        return self.slab.data_ptr() + self.addr

    def fill(self, data: bytes) -> None:
        assert len(data) == self.length
        self.slab[self.addr : self.addr + len(data)].copy_(
            torch.frombuffer(data, dtype=torch.uint8)
        )

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
        """Convert MRs to tensors."""
        if mrs is None or len(mrs) == 0:
            return []

        return [mr.to_tensor(mr_dtype, mr_shape) for mr in mrs]
