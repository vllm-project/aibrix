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
import json
import threading
from abc import ABC, abstractmethod
from dataclasses import dataclass
from typing import Any, Dict, List, Sequence, Tuple, TypeVar

from sortedcontainers import SortedList

from ...common.absl_logging import getLogger
from ...memory import MemoryRegion
from ...meta_service import MetaService
from ...status import Status, StatusCodes
from ..connectors import (
    Connector,
    ConnectorConfig,
    ConnectorFeature,
    ConnectorRegisterDescriptor,
)

K = TypeVar("K")
V = TypeVar("V")


logger = getLogger(__name__)


@dataclass
class Member(ABC):
    meta: Dict[str, Any]
    slots: SortedList[int]
    conn: Connector

    def __str__(self) -> str:
        return f"Member(meta={self.meta}, conn={self.conn.name if self.conn is not None else None})"

    def __repr__(self) -> str:
        return self.__str__()


@dataclass
class PlacementConfig:
    placement_policy: str
    conn_config: ConnectorConfig
    meta_service: MetaService | None = None
    refresh_interval_s: int = 0


class Placement(Connector[K, V]):
    @staticmethod
    def create(config: PlacementConfig) -> "Placement":  # type: ignore
        """Create a Placement object based on the placement policy"""
        if config.placement_policy == "SIMPLE":
            from .simple_placement import SimplePlacement

            return SimplePlacement(config=config)
        else:
            raise ValueError(
                f"Unknown placement policy: {config.placement_policy}"
            )

    @abstractmethod
    def construct_cluster(self, json_str: str) -> Status:
        """Parse the JSON representation of the cluster and build the meta"""
        raise NotImplementedError()

    @abstractmethod
    def select(self, key: K) -> Status[Member]:
        """Select a member from the cluster"""
        raise NotImplementedError()


class BasePlacement(Placement[K, V]):
    def __init__(self, *, config: PlacementConfig):
        self.lock: threading.Lock = threading.Lock()
        self.members: List[Member] = []  # List of Member objects
        self.slots = SortedList()  # All slots in the cluster
        self.total_slots = 0

        self._name = config.placement_policy
        self.conn_config = config.conn_config
        self.meta_service = config.meta_service
        self.refresh_interval_s = config.refresh_interval_s
        self._refresh_thread: threading.Thread | None = None
        self._stop_event = threading.Event()

    @property
    def name(self) -> str:
        return f"{self.conn_config.backend_name}[{self._name}]"

    @property
    def feature(self) -> ConnectorFeature:
        feat = self.members[0].conn.feature
        # TODO: support mput_mget
        feat.mput_mget = False
        return feat

    @classmethod
    def from_envs(cls, conn_id, executor, **kwargs):
        """An abstract method of Connector that is discarded"""
        raise NotImplementedError

    def open(self) -> Status:
        if self.meta_service is not None and self.refresh_interval_s > 0:
            # Initial sync
            status = self._refresh_cluster()
            if not status.is_ok():
                return Status(status)
            # Start background refresh
            self._start_background_refresh()
        return Status.ok()

    def _refresh_cluster(self) -> Status:
        """Refresh the cluster information from metadata service."""
        assert self.meta_service is not None
        status = self.meta_service.get_cluster_metadata()
        if not status.is_ok():
            return Status(status)
        return self.construct_cluster(status.get())

    def _start_background_refresh(self) -> None:
        """Start the background refresh thread."""
        if self._refresh_thread is not None and self._refresh_thread.is_alive():
            self._stop_event.set()
            self._refresh_thread.join()

        self._stop_event.clear()
        self._refresh_thread = threading.Thread(
            target=self._background_refresh,
            daemon=True,
            name="ClusterRefreshThread",
        )
        self._refresh_thread.start()

    def _background_refresh(self) -> None:
        """Background thread for periodic cluster updates."""
        while not self._stop_event.is_set():
            self._refresh_cluster()
            self._stop_event.wait(self.refresh_interval_s)

    def close(self) -> Status:
        # Stop the background refresh thread
        if self._refresh_thread is not None:
            self._stop_event.set()
            self._refresh_thread.join()
            self._refresh_thread = None
        return Status.ok()

    def construct_cluster(self, json_str: str) -> Status:
        """Parse the JSON representation of the cluster and build the meta.

        Supported JSON format:
        {
            "nodes":[
                {
                    "addr":"33.207.94.132",
                    "port":18512,
                    "slots":[
                        {"start":0,"end":0},
                        ...
                    ]
                }
                ...
            ]
        }
        """
        try:
            data = json.loads(json_str)
            temp_members = []
            temp_slots = SortedList(key=lambda x: x[0])

            for node in data["nodes"]:
                slots = node.pop("slots")
                member = Member(
                    meta=node,
                    slots=SortedList(),
                    conn=Connector.create(self.conn_config, **node),
                )
                status = member.conn.open()
                if not status.is_ok():
                    logger.warning(
                        "Failed to open connection to %s, status: %s",
                        member.meta,
                        status,
                    )
                    continue

                for slot_range in slots:
                    start = slot_range["start"]
                    end = slot_range["end"]
                    for slot in range(start, end + 1):
                        member.slots.add(slot)
                        temp_slots.add((slot, member))

                temp_members.append(member)

            if len(temp_members) == 0:
                return Status(
                    StatusCodes.INVALID, "No valid members in the cluster"
                )

            temp_total_slots = len(temp_slots)

            if self.slots != temp_slots:
                logger.info("New cluster members: %s", temp_members)
                with self.lock:
                    self.members = temp_members
                    self.slots = temp_slots
                    self.total_slots = temp_total_slots

        except json.JSONDecodeError as e:
            return Status(StatusCodes.INVALID, f"Invalid JSON: {e}")
        except KeyError as e:
            return Status(
                StatusCodes.INVALID, f"Missing required field in JSON: {e}"
            )
        return Status.ok()

    async def prefetch(self, keys: Sequence[K]) -> None:
        """Prefetch a list of keys.
        Args:
            keys: The keys of the kv tensors.
        """
        pass

    async def exists(self, key: K) -> Status:
        """Check if key is in the store."""
        status = self.select(key)
        if not status.is_ok():
            return Status(status)
        member = status.get()
        return await member.conn.exists(key)

    async def get(self, key: K, mr: MemoryRegion) -> Status:
        """Get a value.
        Args:
            key: The key of the kv tensor.
            mr: The memory region to place the fetched kv tensor.
        Returns:
            The status of the get operation.
        """
        status = self.select(key)
        if not status.is_ok():
            return Status(status)
        member = status.get()
        return await member.conn.get(key, mr)

    async def put(self, key: K, mr: MemoryRegion) -> Status:
        """Put a key value pair.
        Args:
            key: The key of the kv cache.
            mr: The memory region holding the kv tensors.
        Returns:
            The status of the put operation.
        """
        status = self.select(key)
        if not status.is_ok():
            return Status(status)
        member = status.get()
        return await member.conn.put(key, mr)

    def register_mr(
        self, addr: int, length: int
    ) -> Status[ConnectorRegisterDescriptor]:
        """Register an memory region with backend-specific register function.
        Args:
            addr: memory region's address
            length: memory region's length
        Returns:
            Status of the register operation.
            The register descriptor.
        """
        return self.members[0].conn.register_mr(addr, length)

    def deregister_mr(self, desc: ConnectorRegisterDescriptor) -> Status:
        """Deregister an memory region.
        Args:
            desc: the register descriptor returned by `register_mr`.
        Returns:
            Status of the deregister operation.
        """
        return self.members[0].conn.deregister_mr(desc)

    def get_batches(
        self,
        keys: Sequence[K],
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

    async def delete(self, key: K) -> Status:
        """Delete a key.
        Args:
            key: The key of the kv cache.
        Returns:
            The status of the delete operation.
        """
        status = self.select(key)
        if not status.is_ok():
            return Status(status)
        member = status.get()
        return await member.conn.delete(key)
