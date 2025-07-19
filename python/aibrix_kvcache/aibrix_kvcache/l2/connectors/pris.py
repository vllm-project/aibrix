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
from typing import Any, Dict, List, Sequence, Tuple

import pris._pris as Pris
import torch
from pris.pris_client import PrisClient

from ... import envs
from ...common import AsyncBase
from ...memory import MemoryRegion
from ...status import Status, StatusCodes
from . import Connector, ConnectorFeature, ConnectorRegisterDescriptor


@dataclass
class PrisConfig:
    """Pris config.
    Args:
        remote_addr (str): remote address
        remote_port (int): remote port
        password (str): password
    """

    remote_addr: str
    remote_port: int
    password: str


@dataclass
class PrisRegisterDescriptor(ConnectorRegisterDescriptor):
    """Pris register descriptor."""

    reg_buf: int


@AsyncBase.async_wrap(
    exists="_exists",
    get="_get",
    put="_put",
    delete="_delete",
    mget="_mget",
    mput="_mput",
)
class PrisConnector(Connector[bytes, torch.Tensor], AsyncBase):
    """Pris connector."""

    def __init__(
        self,
        config: PrisConfig,
        key_suffix: str,
        executor: Executor,
    ):
        super().__init__(executor)
        self.config = config
        self.key_suffix = key_suffix
        self.conn: PrisClient | None = None
        self._register_cache: Dict[int, PrisRegisterDescriptor] = {}

    @classmethod
    def from_envs(
        cls, conn_id: str, executor: Executor, **kwargs
    ) -> "PrisConnector":
        """Create a connector from environment variables."""
        remote_addr = kwargs.get(
            "addr", envs.AIBRIX_KV_CACHE_OL_PRIS_REMOTE_ADDR
        )
        remote_port = kwargs.get(
            "port", envs.AIBRIX_KV_CACHE_OL_PRIS_REMOTE_PORT
        )

        config = PrisConfig(
            remote_addr=remote_addr,
            remote_port=remote_port,
            password=envs.AIBRIX_KV_CACHE_OL_PRIS_PASSWORD,
        )
        return cls(config, conn_id, executor)

    @property
    def name(self) -> str:
        return "PRIS"

    @property
    def feature(self) -> ConnectorFeature:
        feature = ConnectorFeature(
            rdma=True,
            mput_mget=envs.AIBRIX_KV_CACHE_OL_PRIS_USE_MPUT_MGET,
        )
        return feature

    def __del__(self) -> None:
        self.close()

    def _key(self, key: bytes) -> str:
        return key.hex() + self.key_suffix

    @Status.capture_exception
    def open(self) -> Status:
        """Open a connection."""
        if self.conn is None:
            self.conn = PrisClient(
                raddr=self.config.remote_addr,
                rport=self.config.remote_port,
                password=self.config.password,
            )
        return Status.ok()

    @Status.capture_exception
    def close(self) -> Status:
        """Close a connection."""
        if self.conn is not None:
            for _, desc in self._register_cache.items():
                self._deregister_mr(desc)
            self._register_cache.clear()

            self.conn.close()
            self.conn = None
        return Status.ok()

    @Status.capture_exception
    def register_slabs(self, slabs: List[torch.Tensor]) -> Status:
        assert self.conn is not None
        for slab in slabs:
            addr = slab.data_ptr()
            length = slab.numel()
            reg_buf = self.conn.reg_memory(addr, length)
            if reg_buf == 0:
                return Status(StatusCodes.INVALID)
            desc = PrisRegisterDescriptor(reg_buf)
            self._register_cache[addr] = desc
        return Status.ok(desc)

    def _get_register_descriptor(
        self, mr: MemoryRegion
    ) -> Status[PrisRegisterDescriptor]:
        slab = mr.slab
        addr = slab.data_ptr()
        if addr not in self._register_cache:
            return Status(
                StatusCodes.INVALID, f"Slab(addr={addr}) hasn't been registered"
            )
        return Status.ok(self._register_cache[addr])

    def _deregister_mr(self, desc: PrisRegisterDescriptor) -> None:
        assert self.conn is not None
        if desc.reg_buf != 0:
            self.conn.dereg_memory(desc.reg_buf)
        desc.reg_buf = 0

    @Status.capture_exception
    def _exists(self, key: bytes) -> Status:
        """Check if key is in the store."""
        assert self.conn is not None
        if self.conn.exists(self._key(key)) == 0:
            return Status.ok()
        return Status(StatusCodes.NOT_FOUND)

    @Status.capture_exception
    def _get(self, key: bytes, mr: MemoryRegion) -> Status:
        """Get a value."""
        assert self.conn is not None
        desc_status = self._get_register_descriptor(mr)
        if not desc_status.is_ok():
            return Status(desc_status)
        desc = desc_status.get()
        sgl = Pris.SGL(mr.data_ptr(), mr.length, desc.reg_buf)
        if self.conn.get(self._key(key), sgl, mr.length) != 0:
            return Status(StatusCodes.ERROR)
        return Status.ok()

    @Status.capture_exception
    def _put(self, key: bytes, mr: MemoryRegion) -> Status:
        """Put a key value pair"""
        assert self.conn is not None
        desc_status = self._get_register_descriptor(mr)
        if not desc_status.is_ok():
            return Status(desc_status)
        desc = desc_status.get()
        sgl = Pris.SGL(mr.data_ptr(), mr.length, desc.reg_buf)
        if self.conn.set(self._key(key), sgl) != 0:
            return Status(StatusCodes.ERROR)
        return Status.ok()

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
    def _mget(
        self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        assert self.conn is not None
        sgls: List[Pris.SGL] = []
        cache_keys: List[str] = []
        value_lens: List[int] = []
        op_status: List[Status] = [Status.ok()] * len(keys)
        for i, mr in enumerate(mrs):
            desc_status = self._get_register_descriptor(mr)
            if not desc_status.is_ok():
                for j in range(i, len(keys)):
                    op_status[j] = Status(desc_status)
                break
            desc = desc_status.get()
            sgl = Pris.SGL(mr.data_ptr(), mr.length, desc.reg_buf)
            sgls.append(sgl)
            cache_keys.append(self._key(keys[i]))
            value_lens.append(mr.length)

        if len(sgls) == 0:
            return op_status

        status, details = self.conn.mget(cache_keys, sgls, value_lens)
        if status == 0:
            return op_status
        else:
            for i, s in enumerate(details):
                if s != 0:
                    op_status[i] = Status(StatusCodes.ERROR)
            return op_status

    @Status.capture_exception
    def _mput(
        self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        assert self.conn is not None
        sgls: List[Pris.SGL] = []
        cache_keys: List[str] = []
        op_status: List[Status] = [Status.ok()] * len(keys)
        for i, mr in enumerate(mrs):
            desc_status = self._get_register_descriptor(mr)
            if not desc_status.is_ok():
                for j in range(i, len(keys)):
                    op_status[j] = Status(desc_status)
                break
            desc = desc_status.get()
            sgl = Pris.SGL(mr.data_ptr(), mr.length, desc.reg_buf)
            sgls.append(sgl)
            cache_keys.append(self._key(keys[i]))

        if len(sgls) == 0:
            return op_status

        status, details = self.conn.mset(cache_keys, sgls)
        if status == 0:
            return op_status
        else:
            for i, s in enumerate(details):
                if s != 0:
                    op_status[i] = Status(StatusCodes.ERROR)
            return op_status

    @Status.capture_exception
    def _delete(self, key: bytes) -> Status:
        """Delete a key."""
        assert self.conn is not None
        self.conn.delete(self._key(key))
        return Status.ok()
