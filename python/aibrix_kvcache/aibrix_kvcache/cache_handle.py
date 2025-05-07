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
from typing import Sequence, Tuple

import torch

from .memory import MemoryRegion


class KVCacheHandle(ABC):
    """Cache handle to support zero-copy APIs."""

    @property
    @abstractmethod
    def memory_regions(self) -> Sequence[MemoryRegion]:
        raise NotImplementedError

    @abstractmethod
    def to_tensors(self) -> Sequence[torch.Tensor]:
        raise NotImplementedError

    @abstractmethod
    def release(self) -> None:
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

    @property
    def memory_regions(self) -> Sequence[MemoryRegion]:
        return self._mrs

    def to_tensors(self) -> Sequence[torch.Tensor]:
        return MemoryRegion.to_tensors(
            self._mrs, self._block_dtype, self._block_shape
        )

    def release(self) -> None:
        for mr in self._mrs:
            mr.ref_down()

    def __len__(self) -> int:
        return len(self._mrs)
