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
from contextlib import contextmanager
from queue import Queue
from typing import Any, List, Sequence, Tuple

import infinistore
import torch
from validators import ipv4, ipv6

from ... import envs
from ...common import AsyncBase
from ...common.absl_logging import getLogger
from ...memory import MemoryRegion
from ...status import Status, StatusCodes
from ...transport import HAS_RDMA_TRANSPORT_SUPPORT
from . import Connector, ConnectorFeature

logger = getLogger(__name__)


class InfiniStoreConnector(Connector[bytes, torch.Tensor], AsyncBase):
    """InfiniStore connector."""

    def __init__(
        self,
        config: infinistore.ClientConfig,
        key_suffix: str,
        executor: Executor,
    ):
        super().__init__(executor)
        self.config = config
        self.key_suffix = key_suffix

        # rdma connection
        self.rdma_conn: infinistore.InfinityConnection | None = None

        # tcp connections
        self.tcp_conns: Queue[infinistore.InfinityConnection] | None = None

    @classmethod
    def from_envs(
        cls, conn_id: str, executor: Executor, **kwargs
    ) -> "InfiniStoreConnector":
        """Create a connector from environment variables."""
        host_addr = kwargs.get(
            "addr", envs.AIBRIX_KV_CACHE_OL_INFINISTORE_HOST_ADDR
        )
        service_port = kwargs.get(
            "port", envs.AIBRIX_KV_CACHE_OL_INFINISTORE_SERVICE_PORT
        )
        assert ipv4(host_addr) or ipv6(host_addr), "Invalid host_addr"
        dev_list = envs.AIBRIX_KV_CACHE_OL_INFINISTORE_VISIBLE_DEV_LIST
        connection_type = envs.AIBRIX_KV_CACHE_OL_INFINISTORE_CONNECTION_TYPE
        ib_port = envs.AIBRIX_KV_CACHE_OL_INFINISTORE_IB_PORT
        link_type = envs.AIBRIX_KV_CACHE_OL_INFINISTORE_LINK_TYPE

        if connection_type == "TCP":
            config = infinistore.ClientConfig(
                host_addr=host_addr,
                service_port=service_port,
                connection_type=connection_type,
                link_type=link_type,
            )
            return cls(config, conn_id, executor)

        if HAS_RDMA_TRANSPORT_SUPPORT:
            from ...transport.rdma import (
                AddrFamily,
                DeviceRequest,
                GIDType,
                RDMATransport,
            )

            # RDMA
            addr_family = (
                AddrFamily.AF_INET if ipv4(host_addr) else AddrFamily.AF_INET6
            )
            gid_type = (
                GIDType.ROCE_V2 if link_type != "IB" else GIDType.IB_ROCE_V1
            )
            if len(dev_list) == 0:
                logger.info(
                    "AIBRIX_KV_CACHE_OL_INFINISTORE_VISIBLE_DEV_LIST is not "
                    "set, trying to auto-detect visible devices"
                )
                addr_range = envs.AIBRIX_KV_CACHE_OL_TRANSPORT_RDMA_ADDR_RANGE
                rdma = RDMATransport(
                    addr_range=addr_range,
                    addr_family=addr_family,
                    gid_type=gid_type,
                )
                status = rdma.get_device_list()
                assert status.is_ok(), f"Failed to get device list: {status}"

                devices = status.get()
                for d in devices:
                    dev_list.append(f"{d.device_name}:{d.port_attrs.gid_index}")
            else:
                requests: List[DeviceRequest] = []
                for dev_name in dev_list:
                    if ":" in dev_name:
                        splits = dev_name.split(":")
                        request = DeviceRequest(
                            device_name=splits[0],
                            gid_index=int(splits[1]),
                        )
                    else:
                        request = DeviceRequest(device_name=dev_name)
                    requests.append(request)
                rdma = RDMATransport(
                    request=requests,
                    addr_family=addr_family,
                    gid_type=gid_type,
                )
                status = rdma.get_device_list()
                # Only update dev_list if we got a new list
                if status.is_ok():
                    devices = status.get()
                    dev_list.clear()
                    for d in devices:
                        dev_list.append(
                            f"{d.device_name}:{d.port_attrs.gid_index}"
                        )
        else:
            assert len(dev_list) > 0, "RDMA auto-detect is not supported, "
            "AIBRIX_KV_CACHE_OL_INFINISTORE_VISIBLE_DEV_LIST MUST be set"

        num_visible_gpus = torch.cuda.device_count()

        dev_list = [
            dev_list[i % len(dev_list)] for i in range(num_visible_gpus)
        ]

        # For InfiniStore RDMA, we need to map the GPU index to the RNIC
        # index to support multi-GPU per RNIC. For example, if we have 8
        # GPUs and 2 RNICs, then GPU 0 to 3 are mapped to RNIC 0, and GPU
        # 4 to 7 are mapped to RNIC 1.
        factor = num_visible_gpus // len(dev_list)
        gpu_idx = torch.cuda.current_device()
        rnic_idx = gpu_idx // factor
        dev_name = dev_list[rnic_idx]
        hint_gid_index_str = ""

        logger.info(f"InfiniStore selects {dev_name}")

        # If dev_name is in the format of "mlx5_i:xxx", then we need to
        # extract the dev_name and hint_gid_index from the dev_name.
        if ":" in dev_name:
            splits = dev_name.split(":")
            dev_name = splits[0]
            hint_gid_index_str = splits[1]

        config = infinistore.ClientConfig(
            host_addr=host_addr,
            service_port=service_port,
            connection_type=connection_type,
            ib_port=ib_port,
            link_type=link_type,
            dev_name=dev_name,
        )

        if hasattr(config, "hint_gid_index") and hint_gid_index_str != "":
            try:
                config.hint_gid_index = int(hint_gid_index_str)
            except ValueError:
                raise ValueError(
                    f"Invalid hint_gid_index: {hint_gid_index_str}"
                )

        return cls(config, conn_id, executor)

    @property
    def name(self) -> str:
        return "InfiniStore"

    @property
    def feature(self) -> ConnectorFeature:
        feature = ConnectorFeature()
        if (
            self.config is not None
            and self.config.connection_type == infinistore.TYPE_RDMA
        ):
            # InfiniStore has a 4MB size limit
            # feature.mput_mget = True
            feature.rdma = True
        return feature

    def __del__(self) -> None:
        self.close()

    def _key(self, key: bytes) -> str:
        return key.hex() + self.key_suffix

    @contextmanager
    def _tcp_conn(self):
        assert self.config.connection_type == infinistore.TYPE_TCP
        assert self.tcp_conns is not None
        conn = None
        try:
            conn = self.tcp_conns.get()
            yield conn
        finally:
            if conn is not None:
                self.tcp_conns.put(conn)

    @Status.capture_exception
    def open(self) -> Status:
        """Open a connection."""
        if self.config.connection_type == infinistore.TYPE_RDMA:
            if self.rdma_conn is None:
                self.rdma_conn = infinistore.InfinityConnection(self.config)
                self.rdma_conn.connect()
        else:
            assert hasattr(self._executor, "_max_workers")
            assert self.tcp_conns is None

            self.tcp_conns = Queue(
                maxsize=self._executor._max_workers  # type: ignore
            )
            for _ in range(self.tcp_conns.maxsize):
                tcp_conn = infinistore.InfinityConnection(self.config)
                tcp_conn.connect()
                self.tcp_conns.put(tcp_conn)
        return Status.ok()

    @Status.capture_exception
    def close(self) -> Status:
        """Close a connection."""
        if self.rdma_conn is not None:
            self.rdma_conn.close()
            self.rdma_conn = None

        if self.config.connection_type == infinistore.TYPE_TCP:
            if self.tcp_conns is not None:
                while not self.tcp_conns.empty():
                    tcp_conn = self.tcp_conns.get()
                    if tcp_conn is not None:
                        tcp_conn.close()
                del self.tcp_conns
                self.tcp_conns = None

        return Status.ok()

    @Status.capture_exception
    def register_slabs(self, slabs: List[torch.Tensor]) -> Status:
        assert self.rdma_conn is not None
        for slab in slabs:
            addr = slab.data_ptr()
            length = slab.numel()
            ret = self.rdma_conn.register_mr(addr, length)
            if ret != 0:
                return Status(StatusCodes.INVALID)
        return Status.ok()

    @Status.capture_exception
    async def exists(self, key: bytes) -> Status:
        """Check if key is in the store."""
        if self.config.connection_type == infinistore.TYPE_RDMA:
            return await self.event_loop.run_in_executor(
                self._executor, self._rdma_exists, key
            )
        else:
            return await self.event_loop.run_in_executor(
                self._executor, self._tcp_exists, key
            )

    def _rdma_exists(self, key: bytes) -> Status:
        assert self.rdma_conn is not None
        if self.rdma_conn.check_exist(self._key(key)):
            return Status.ok()
        return Status(StatusCodes.NOT_FOUND)

    def _tcp_exists(self, key: bytes) -> Status:
        with self._tcp_conn() as conn:
            assert conn is not None
            if conn.check_exist(self._key(key)):
                return Status.ok()
        return Status(StatusCodes.NOT_FOUND)

    def get_batches(
        self,
        keys: Sequence[Any],
        mrs: Sequence[MemoryRegion],
        batch_size: int,
    ) -> Sequence[Sequence[Tuple[bytes, MemoryRegion]]]:
        lists: List[List[Tuple[bytes, MemoryRegion]]] = []
        for key, mr in zip(keys, mrs):
            if (
                len(lists) == 0
                or lists[-1][0][1].data_ptr() != mr.slab.data_ptr()
                or len(lists[-1]) >= batch_size
            ):
                lists.append([(key, mr)])
            else:
                lists[-1].append((key, mr))
        return lists

    @Status.capture_exception
    async def mget(
        self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        assert self.rdma_conn is not None
        base_addr = mrs[0].slab.data_ptr()
        block_size = mrs[0].length
        blocks = [None] * len(mrs)
        for i, mr in enumerate(mrs):
            blocks[i] = (self._key(keys[i]), mr.addr)  # type: ignore

        try:
            await self.rdma_conn.rdma_read_cache_async(
                blocks, block_size, base_addr
            )
        except infinistore.InfiniStoreKeyNotFound:
            return [Status(StatusCodes.NOT_FOUND)] * len(mrs)
        return [Status.ok()] * len(mrs)

    @Status.capture_exception
    async def mput(
        self, keys: Sequence[bytes], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        assert self.rdma_conn is not None
        base_addr = mrs[0].slab.data_ptr()
        block_size = mrs[0].length
        blocks = [None] * len(mrs)
        for i, mr in enumerate(mrs):
            blocks[i] = (self._key(keys[i]), mr.addr)  # type: ignore

        await self.rdma_conn.rdma_write_cache_async(
            blocks, block_size, base_addr
        )
        return [Status.ok()] * len(mrs)

    @Status.capture_exception
    async def get(self, key: bytes, mr: MemoryRegion) -> Status[torch.Tensor]:
        """Get a value."""
        if self.config.connection_type == infinistore.TYPE_RDMA:
            return await self._rdma_get(key, mr)
        else:
            return await self.event_loop.run_in_executor(
                self._executor, self._tcp_get, key, mr
            )

    def _tcp_get(self, key: bytes, mr: MemoryRegion) -> Status:
        """Get a value via TCP."""
        with self._tcp_conn() as conn:
            assert conn is not None
            val = conn.tcp_read_cache(self._key(key))
            if val is None or len(val) == 0:
                return Status(StatusCodes.NOT_FOUND)
            mr.fill(val)
            return Status.ok()

    async def _rdma_get(self, key: bytes, mr: MemoryRegion) -> Status:
        """Get a value via RDMA."""
        assert self.rdma_conn is not None
        try:
            await self.rdma_conn.rdma_read_cache_async(
                [(self._key(key), mr.addr)], mr.length, mr.slab.data_ptr()
            )
        except infinistore.InfiniStoreKeyNotFound:
            return Status(StatusCodes.NOT_FOUND)
        return Status.ok()

    @Status.capture_exception
    async def put(self, key: bytes, mr: MemoryRegion) -> Status:
        """Put a key value pair"""
        if self.config.connection_type == infinistore.TYPE_RDMA:
            return await self._rdma_put(key, mr)
        else:
            return await self.event_loop.run_in_executor(
                self._executor, self._tcp_put, key, mr
            )

    async def _rdma_put(self, key: bytes, mr: MemoryRegion) -> Status:
        """Put a value via RDMA."""
        assert self.rdma_conn is not None
        await self.rdma_conn.rdma_write_cache_async(
            [(self._key(key), mr.addr)], mr.length, mr.slab.data_ptr()
        )
        return Status.ok()

    def _tcp_put(self, key: bytes, mr: MemoryRegion) -> Status:
        """Put a value via TCP."""
        with self._tcp_conn() as conn:
            assert conn is not None
            conn.tcp_write_cache(self._key(key), mr.data_ptr(), mr.length)
            return Status.ok()

    @Status.capture_exception
    async def delete(self, key: bytes) -> Status:
        """Delete a key."""
        if self.config.connection_type == infinistore.TYPE_RDMA:
            return await self.event_loop.run_in_executor(
                self._executor, self._rdma_delete, key
            )
        else:
            return await self.event_loop.run_in_executor(
                self._executor, self._tcp_delete, key
            )

    def _rdma_delete(self, key: bytes) -> Status:
        assert self.rdma_conn is not None
        self.rdma_conn.delete_keys([self._key(key)])
        return Status.ok()

    def _tcp_delete(self, key: bytes) -> Status:
        with self._tcp_conn() as conn:
            assert conn is not None
            conn.delete_keys([self._key(key)])
            return Status.ok()
