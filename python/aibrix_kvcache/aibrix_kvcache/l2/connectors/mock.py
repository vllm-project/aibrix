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

from concurrent.futures import Executor
from dataclasses import dataclass
from threading import Lock
from typing import Any, Dict, List, Sequence, Tuple

import torch

from ... import envs
from ...common import AsyncBase
from ...memory import MemoryRegion
from ...status import Status, StatusCodes
from . import Connector, ConnectorFeature, ConnectorRegisterDescriptor


@dataclass
class MockConfig:
    use_rdma: bool = False
    use_mput_mget: bool = False


@dataclass
class MockRegisterDescriptor(ConnectorRegisterDescriptor):
    addr: int


@AsyncBase.async_wrap(
    exists="_exists", get="_get", put="_put", delete="_delete"
)
class MockConnector(Connector[bytes, torch.Tensor], AsyncBase):
    """Mock connector."""

    def __init__(
        self,
        config: MockConfig,
        executor: Executor,
    ):
        super().__init__(executor)
        self.config = config
        self.lock = Lock()
        self.store: Dict[bytes, bytes] | None = None
        self._register_cache: Dict[int, int] = {}

    @classmethod
    def from_envs(
        cls, conn_id: str, executor: Executor, **kwargs
    ) -> "MockConnector":
        """Create a connector from environment variables."""
        config = MockConfig(
            use_rdma=envs.AIBRIX_KV_CACHE_OL_MOCK_USE_RDMA,
            use_mput_mget=envs.AIBRIX_KV_CACHE_OL_MOCK_USE_MPUT_MGET,
        )
        return cls(config, executor)

    @property
    def name(self) -> str:
        return "Mock"

    @property
    def feature(self) -> ConnectorFeature:
        feature = ConnectorFeature()
        if self.config.use_mput_mget:
            feature.mput_mget = True
        if self.config.use_rdma:
            feature.rdma = True
        return feature

    def __del__(self) -> None:
        self.close()

    @Status.capture_exception
    def open(self) -> Status:
        """Open a connection."""
        if self.store is None:
            self.store = {}
        return Status.ok()

    @Status.capture_exception
    def close(self) -> Status:
        """Close a connection."""
        if self.store is not None:
            self.store.clear()
            self.store = None
        return Status.ok()

    @Status.capture_exception
    def register_slabs(self, slabs: List[torch.Tensor]) -> Status:
        for slab in slabs:
            addr = slab.data_ptr()
            length = slab.numel()
            self._register_cache[addr] = length
        return Status.ok()

    def _check_mr_registered(self, mr: MemoryRegion) -> None:
        slab = mr.slab
        addr = slab.data_ptr()
        length = slab.numel()
        assert addr in self._register_cache
        assert length <= self._register_cache[addr]

    def get_batches(
        self,
        keys: Sequence[Any],
        mrs: Sequence[MemoryRegion],
        batch_size: int,
    ) -> Sequence[Sequence[Tuple[bytes, MemoryRegion]]]:
        lists: List[List[Tuple[bytes, MemoryRegion]]] = []
        for key, mr in zip(keys, mrs):
            if len(lists) == 0 or len(lists[-1]) >= batch_size:
                lists.append([(key, mr)])
            else:
                lists[-1].append((key, mr))
        return lists

    @Status.capture_exception
    async def mget(
        self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        assert self.store is not None
        statuses = []
        for i, mr in enumerate(mrs):
            if self.feature.rdma:
                self._check_mr_registered(mr)
            statuses.append(self._get(keys[i], mr))
        return statuses

    @Status.capture_exception
    async def mput(
        self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        assert self.store is not None
        statuses = []
        for i, mr in enumerate(mrs):
            if self.feature.rdma:
                self._check_mr_registered(mr)
            statuses.append(self._put(keys[i], mr))
        return statuses

    @Status.capture_exception
    def _exists(self, key: bytes) -> Status:
        """Check if key is in the store."""
        assert self.store is not None
        with self.lock:
            if key in self.store:
                return Status.ok()
        return Status(StatusCodes.NOT_FOUND)

    @Status.capture_exception
    def _get(self, key: bytes, mr: MemoryRegion) -> Status:
        """Get a value."""
        assert self.store is not None
        with self.lock:
            val = self.store.get(key, None)
        if val is None:
            return Status(StatusCodes.NOT_FOUND)
        if self.feature.rdma:
            self._check_mr_registered(mr)
        mr.fill(val)
        return Status.ok()

    @Status.capture_exception
    def _put(self, key: bytes, mr: MemoryRegion) -> Status:
        """Put a key value pair"""
        assert self.store is not None
        if self.feature.rdma:
            self._check_mr_registered(mr)
        with self.lock:
            self.store[key] = mr.tobytes()
        return Status.ok()

    @Status.capture_exception
    def _delete(self, key: bytes) -> Status:
        """Delete a key."""
        assert self.store is not None
        with self.lock:
            self.store.pop(key, None)
        return Status.ok()
