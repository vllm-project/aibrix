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

import torch
from sortedcontainers import SortedList

from ...common.absl_logging import getLogger
from ...memory import MemoryRegion
from ...meta_service import MetaService
from ...status import Status, StatusCodes
from ..connectors import (
    Connector,
    ConnectorConfig,
    ConnectorFeature,
)

K = TypeVar("K")
V = TypeVar("V")


logger = getLogger(__name__)


@dataclass
class Member(ABC):
    meta: Dict[str, Any]
    slots: SortedList[int]
    conn: Connector

    def __post_init__(self):
        self.hashable_meta = tuple(self.meta.items())

    def __hash__(self):
        return self.hashable_meta.__hash__()

    def __eq__(self, other):
        if not isinstance(other, Member):
            return False
        return self.hashable_meta.__eq__(other.hashable_meta)

    def __str__(self) -> str:
        conn_name = self.conn.name if self.conn is not None else None
        return f"Member(meta={self.meta}, conn={conn_name})"

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
        self.conn_feature: ConnectorFeature | None = None

        self._name = config.placement_policy
        self.conn_config = config.conn_config
        self.meta_service = config.meta_service
        self.refresh_interval_s = config.refresh_interval_s
        self._refresh_thread: threading.Thread | None = None
        self._stop_event = threading.Event()
        self._slabs: List[torch.Tensor] | None = None

    @property
    def name(self) -> str:
        return f"{self.conn_config.backend_name}[{self._name}]"

    @property
    def feature(self) -> ConnectorFeature:
        if self.conn_feature is None:
            return ConnectorFeature()

        feat = self.conn_feature
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
            temp_members: List[Member] = []
            temp_slots = SortedList(key=lambda x: x[0])

            for node in data["nodes"]:
                slots = node.pop("slots")
                member = Member(
                    meta=node,
                    slots=SortedList(),
                    # Delay conn establishment
                    conn=None,  # type: ignore
                )

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
                if self.conn_feature is not None:
                    logger.info("Cluster members changed, updating...")

                members = {m.hashable_meta: m for m in self.members}
                num_established_members = 0
                for mem in temp_members:
                    if mem.hashable_meta in members:
                        # reuse existing connection
                        mem.conn = members[mem.hashable_meta].conn
                    else:
                        mem.conn = Connector.create(
                            self.conn_config, **mem.meta
                        )
                        # open
                        status = mem.conn.open()
                        if not status.is_ok():
                            mem.conn = None  # type: ignore
                            logger.warning(
                                "Failed to open connection to %s, status: %s",
                                mem.meta,
                                status,
                            )
                            continue
                        # register
                        if self._slabs is not None:
                            reg_status = mem.conn.register_slabs(self._slabs)
                            if not reg_status.is_ok():
                                mem.conn.close()
                                mem.conn = None  # type: ignore
                                logger.warning(
                                    "Failed to register slabs w/ connection %s"
                                    ", status: %s",
                                    mem.meta,
                                    status,
                                )
                                continue

                    num_established_members += 1

                if num_established_members == 0:
                    return Status(
                        StatusCodes.INVALID, "No valid members in the cluster"
                    )

                logger.info("New cluster members: %s", temp_members)
                members_to_close = set(self.members) - set(temp_members)
                with self.lock:
                    self.members = temp_members
                    self.slots = temp_slots
                    self.total_slots = temp_total_slots
                    self.conn_feature = [
                        m.conn for m in temp_members if m.conn
                    ][0].feature

                for member in members_to_close:
                    logger.info("Closing connection to %s", member.meta)
                    status = member.conn.close()
                    if not status.is_ok():
                        logger.warning(
                            "Failed to close connection to %s, status: %s",
                            member.meta,
                            status,
                        )

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
        if member.conn is None:
            return Status(StatusCodes.ERROR, "Connection not established")
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
        if member.conn is None:
            return Status(StatusCodes.ERROR, "Connection not established")
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
        if member.conn is None:
            return Status(StatusCodes.ERROR, "Connection not established")
        return await member.conn.put(key, mr)

    def register_slabs(self, slabs: List[torch.Tensor]) -> Status:
        """Register slabs with backend-specific register function.
        Args:
            slabs: slabs to be registered.
        Returns:
            Status of the register operation.
        """
        if self._slabs is None:
            self._slabs = slabs
        first_error_status: Status | None = None
        if len(self.members) > 0:
            for m in self.members:
                if m.conn is None:
                    continue
                status = m.conn.register_slabs(slabs)
                if status.is_ok():
                    logger.info(
                        "Conn[%s] successfully registered slabs", m.meta
                    )
                else:
                    logger.error("Conn[%s] failed to register slabs", m.meta)
                    first_error_status = status
        return first_error_status or Status.ok()

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
        if member.conn is None:
            return Status(StatusCodes.ERROR, "Connection not established")
        return await member.conn.delete(key)
