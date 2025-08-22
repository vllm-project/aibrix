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

import torch

from .memory_region import MemoryRegion


class ExternalMemoryRegion(MemoryRegion):
    """ExternalMemoryRegion represents an external continuous memory buffer.
    Right now we only use it to support GDR.
    """

    def __init__(
        self,
        slab: torch.Tensor,
        addr: int,
        len: int,
    ) -> None:
        super().__init__(slab, addr, len)

    def __repr__(self) -> str:
        return (
            f"ExternalMemoryRegion(addr={self.slab.data_ptr() + self.addr}, "
            f"length={self.length}, capacity={self.capacity})"
        )

    def destroy_unsafe(self):
        pass
