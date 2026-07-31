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
    use_gdr_put: bool = False
    use_gdr_get: bool = False
    use_noop: bool = False


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
        self.store: Dict[bytes, bytes | Sequence[bytes]] | None = None
        self._register_cache: Dict[int, int] = {}

    @classmethod
    def from_envs(
        cls, conn_id: str, executor: Executor, **kwargs
    ) -> "MockConnector":
        """Create a connector from environment variables."""
        config = MockConfig(
            use_rdma=envs.AIBRIX_KV_CACHE_OL_MOCK_USE_RDMA,
            use_mput_mget=envs.AIBRIX_KV_CACHE_OL_MOCK_USE_MPUT_MGET,
            use_gdr_get=envs.AIBRIX_KV_CACHE_OL_MOCK_USE_GDR_GET,
            use_gdr_put=envs.AIBRIX_KV_CACHE_OL_MOCK_USE_GDR_PUT,
            use_noop=envs.AIBRIX_KV_CACHE_OL_MOCK_USE_NOOP,
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
        if self.config.use_gdr_get:
            feature.gdr_get = True
        if self.config.use_gdr_put:
            feature.gdr_put = True
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
            length = slab.numel() * slab.itemsize
            self._register_cache[addr] = length
        return Status.ok()

    def _check_mr_registered(self, mr: MemoryRegion | Sequence[MemoryRegion]):
        def check(m: MemoryRegion):
            slab = m.slab
            addr = slab.data_ptr()
            length = slab.numel() * slab.itemsize
            assert addr in self._register_cache
            assert length <= self._register_cache[addr], (
                f"addr: {addr}, length: {length}, "
                f"registered_length: {self._register_cache[addr]}"
            )

        if isinstance(mr, Sequence):
            for m in mr:
                check(m)
        else:
            check(mr)

    def get_batches(
        self,
        keys: Sequence[Any],
        mrs: Sequence[MemoryRegion | Sequence[MemoryRegion]],
        batch_size: int,
    ) -> Sequence[
        Sequence[Tuple[bytes, MemoryRegion | Sequence[MemoryRegion]]]
    ]:
        lists: List[
            List[Tuple[bytes, MemoryRegion | Sequence[MemoryRegion]]]
        ] = []

        for key, mr in zip(keys, mrs):
            if len(lists) == 0 or len(lists[-1]) >= batch_size:
                lists.append([(key, mr)])
            else:
                lists[-1].append((key, mr))
        return lists

    @Status.capture_exception
    async def mget(
        self,
        keys: Sequence[bytes],
        mrs: Sequence[MemoryRegion | Sequence[MemoryRegion]],
    ) -> Sequence[Status]:
        assert self.store is not None
        statuses = []

        if self.config.use_gdr_get:
            assert isinstance(mrs[0], Sequence)
        else:
            assert isinstance(mrs[0], MemoryRegion)

        if self.config.use_noop:
            return [Status(StatusCodes.NOT_FOUND)] * len(keys)

        for i, mr in enumerate(mrs):
            if self.config.use_rdma:
                self._check_mr_registered(mr)
            statuses.append(self._get(keys[i], mr))
        return statuses

    @Status.capture_exception
    async def mput(
        self,
        keys: Sequence[bytes],
        mrs: Sequence[MemoryRegion | Sequence[MemoryRegion]],
    ) -> Sequence[Status]:
        assert self.store is not None
        statuses = []

        if self.config.use_gdr_put:
            assert isinstance(mrs[0], Sequence)
        else:
            assert isinstance(mrs[0], MemoryRegion)

        if self.config.use_noop:
            return [Status.ok()] * len(keys)

        for i, mr in enumerate(mrs):
            if self.config.use_rdma:
                self._check_mr_registered(mr)
            statuses.append(self._put(keys[i], mr))
        return statuses

    @Status.capture_exception
    def _exists(self, key: bytes) -> Status:
        """Check if key is in the store."""
        assert self.store is not None

        if self.config.use_noop:
            return Status(StatusCodes.NOT_FOUND)

        with self.lock:
            if key in self.store:
                return Status.ok()
        return Status(StatusCodes.NOT_FOUND)

    @Status.capture_exception
    def _get(
        self,
        key: bytes,
        mr: MemoryRegion | Sequence[MemoryRegion],
    ) -> Status:
        """Get a value."""
        assert self.store is not None

        if self.config.use_gdr_get:
            assert isinstance(mr, Sequence)
        else:
            assert isinstance(mr, MemoryRegion)

        if self.config.use_noop:
            return Status(StatusCodes.NOT_FOUND)

        with self.lock:
            val = self.store.get(key, None)

        if val is None:
            return Status(StatusCodes.NOT_FOUND)

        if self.config.use_rdma or self.config.use_gdr_get:
            self._check_mr_registered(mr)

        if isinstance(mr, Sequence) and isinstance(val, List):
            assert len(val) == len(mr)
            for i in range(len(mr)):
                mr[i].fill(val[i])
        elif isinstance(mr, Sequence):
            assert isinstance(val, bytes)
            assert len(val) % len(mr) == 0
            chunk_sz = len(val) // len(mr)
            chunks = [
                val[i : i + chunk_sz] for i in range(0, len(val), chunk_sz)
            ]
            for i in range(len(mr)):
                mr[i].fill(chunks[i])
        elif isinstance(val, List):
            mr.fill(b"".join(val))
        else:
            assert isinstance(val, bytes)
            mr.fill(val)
        return Status.ok()

    @Status.capture_exception
    def _put(
        self,
        key: bytes,
        mr: MemoryRegion | Sequence[MemoryRegion],
    ) -> Status:
        """Put a key value pair"""
        assert self.store is not None

        if self.config.use_gdr_put:
            assert isinstance(mr, Sequence)
        else:
            assert isinstance(mr, MemoryRegion)

        if self.config.use_noop:
            return Status.ok()

        if self.config.use_rdma or self.config.use_gdr_put:
            self._check_mr_registered(mr)

        with self.lock:
            if isinstance(mr, Sequence):
                self.store[key] = [m.tobytes() for m in mr]
            else:
                self.store[key] = mr.tobytes()
        return Status.ok()

    @Status.capture_exception
    def _delete(self, key: bytes) -> Status:
        """Delete a key."""
        assert self.store is not None

        if self.config.use_noop:
            return Status.ok()

        with self.lock:
            self.store.pop(key, None)
        return Status.ok()
