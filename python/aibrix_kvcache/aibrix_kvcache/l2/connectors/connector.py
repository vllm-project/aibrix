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

from abc import abstractmethod
from concurrent.futures import Executor
from dataclasses import dataclass
from typing import Any, Generic, List, Sequence, Tuple, TypeVar

import torch

from ...memory import MemoryRegion
from ...status import Status

K = TypeVar("K")
V = TypeVar("V")


@dataclass
class ConnectorFeature:
    """The features of the kv cache connector.
    Args:
        mput_mget: Whether the kv cache connector supports mput/mget
        prefetch: Whether the kv cache connector supports prefetch.
        rdma: Whether the kv cache connector supports RDMA.
    """

    mput_mget: bool = False
    prefetch: bool = False
    rdma: bool = False


@dataclass
class ConnectorConfig:
    """The config of the kv cache connector."""

    backend_name: str
    namespace: str
    partition_id: str
    executor: Executor
    key_builder_signature: str = ""
    layout_signature: str = ""


@dataclass
class ConnectorRegisterDescriptor:
    """The register descriptor"""

    pass


class Connector(Generic[K, V]):
    """Connector interface."""

    @staticmethod
    def create(
        config: ConnectorConfig,
        **kwargs,
    ) -> "Connector":
        """Create a connector."""
        backend_name = config.backend_name
        namespace = config.namespace
        partition_id = config.partition_id
        executor = config.executor
        key_builder_signature = config.key_builder_signature
        layout_signature = config.layout_signature

        conn_id = f"{namespace}_{partition_id}"
        signatures = f"{key_builder_signature}{layout_signature}"
        if len(signatures) > 0:
            conn_id = f"{conn_id}_{signatures}"

        if backend_name == "ROCKSDB":
            from .rocksdb import RocksDBConnector

            return RocksDBConnector.from_envs(conn_id, executor, **kwargs)
        elif backend_name == "INFINISTORE":
            from .infinistore import InfiniStoreConnector

            return InfiniStoreConnector.from_envs(conn_id, executor, **kwargs)
        elif backend_name == "HPKV":
            from .hpkv import HPKVConnector

            return HPKVConnector.from_envs(conn_id, executor, **kwargs)
        elif backend_name == "MOCK":
            from .mock import MockConnector

            return MockConnector.from_envs(conn_id, executor, **kwargs)
        else:
            raise ValueError(f"Unknown connector type: {backend_name}")

    @classmethod
    @abstractmethod
    def from_envs(cls, conn_id: str, executor: Executor, **kwargs):
        """Create a connector from environment variables."""
        raise NotImplementedError

    @property
    @abstractmethod
    def name(self) -> str:
        raise NotImplementedError

    @property
    @abstractmethod
    def feature(self) -> ConnectorFeature:
        """Get the feature of the connector.
        Returns:
            The feature of the kv cache service.
        """
        raise NotImplementedError

    @abstractmethod
    def open(self) -> Status:
        """Open a connection."""
        raise NotImplementedError

    @abstractmethod
    def close(self) -> Status:
        """Close a connection."""
        raise NotImplementedError

    async def prefetch(self, keys: Sequence[K]) -> None:
        """Prefetch a list of keys.
        Args:
            keys: The keys of the kv tensors.
        """
        pass

    @abstractmethod
    async def exists(self, key: K) -> Status:
        """Check if key is in the store."""
        raise NotImplementedError

    @abstractmethod
    async def get(self, key: K, mr: MemoryRegion) -> Status:
        """Get a value.
        Args:
            key: The key of the kv tensor.
            mr: The memory region to place the fetched kv tensor.
        Returns:
            The status of the get operation.
        """
        raise NotImplementedError

    @abstractmethod
    async def put(self, key: K, mr: MemoryRegion) -> Status:
        """Put a key value pair.
        Args:
            key: The key of the kv cache.
            mr: The memory region holding the kv tensors.
        Returns:
            The status of the put operation.
        """
        raise NotImplementedError

    def register_slabs(self, slabs: List[torch.Tensor]) -> Status:
        """Register slabs with backend-specific register function.
        Args:
            slabs: slabs to be registered.
        Returns:
            Status of the register operation.
        """
        raise NotImplementedError

    def get_batches(
        self,
        keys: Sequence[Any],
        mrs: Sequence[MemoryRegion],
        batch_size: int,
    ) -> Sequence[Sequence[Tuple[K, MemoryRegion]]]:
        """Get a list of key MR batches that is used for mput and mget
        operations.

        Args:
            keys: The keys of the kv tensors.
            mrs: Memory regions holding the kv tensors.
            batch_size: The maximum number of key MR pairs in a batch.
        Returns:
            List of key MR batches.
        """
        raise NotImplementedError

    async def mget(
        self, keys: Sequence[K], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        """MGet a list of values. This function is optional and only connectors
        have mput_mget feature enabled can implement this function.
        Args:
            keys: The keys of the kv tensors.
            mrs: Memory regions to hold the fetched kv tensors.
        Returns:
            List of statuses.
        """
        raise NotImplementedError

    async def mput(
        self, keys: Sequence[K], mrs: Sequence[MemoryRegion]
    ) -> Sequence[Status]:
        """MPut a list of key value pairs. This function is optional and only
        connectors have mput_mget feature enabled can implement this function.
        Args:
            keys: The keys of the kv tensors.
            mrs: Memory regions holding the kv tensors.
        Returns:
            List of statuses.
        """
        raise NotImplementedError

    @abstractmethod
    async def delete(self, key: K) -> Status:
        """Delete a key.
        Args:
            key: The key of the kv cache.
        Returns:
            The status of the delete operation.
        """
        raise NotImplementedError
