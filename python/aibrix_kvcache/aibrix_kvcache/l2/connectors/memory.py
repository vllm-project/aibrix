# Copyright 2024 The Aibrix Team.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0

"""Bounded in-process connector for variable-length byte payloads."""

from collections import OrderedDict
from concurrent.futures import Executor
from contextlib import asynccontextmanager
from dataclasses import dataclass
from threading import RLock
from typing import Any, AsyncIterator, Sequence

import torch

from ... import envs
from ...memory import MemoryRegion
from ...status import Status, StatusCodes
from . import Connector, ConnectorFeature


@dataclass
class _Entry:
    payload: bytes | memoryview
    readers: int = 0


class MemoryConnector(Connector[bytes, torch.Tensor]):
    """Process-local LRU byte store with reader-safe eviction.

    Capacity counts payload bytes, not Python object overhead. A read-only
    memoryview is borrowed without copying: its owner must keep the backing
    buffer unchanged while the record is stored or read. Other inputs are
    copied. Set AIBRIX_KV_CACHE_OL_MEMORY_CAPACITY_BYTES for the factory;
    the default is 10 GiB per connector instance.
    """

    def __init__(self, capacity_bytes: int):
        if type(capacity_bytes) is not int or capacity_bytes <= 0:
            raise ValueError("memory connector capacity must be positive")
        self.capacity_bytes = capacity_bytes
        self._entries: OrderedDict[bytes, _Entry] | None = None
        self._used_bytes = 0
        self._lock = RLock()

    @classmethod
    def from_envs(
        cls, conn_id: str, executor: Executor, **kwargs
    ) -> "MemoryConnector":
        del conn_id, executor, kwargs
        return cls(envs.AIBRIX_KV_CACHE_OL_MEMORY_CAPACITY_BYTES)

    @property
    def name(self) -> str:
        return "Memory"

    @property
    def feature(self) -> ConnectorFeature:
        return ConnectorFeature(variable_payload=True)

    @property
    def used_bytes(self) -> int:
        with self._lock:
            return self._used_bytes

    def open(self) -> Status:
        with self._lock:
            if self._entries is None:
                self._entries = OrderedDict()
        return Status.ok()

    def close(self) -> Status:
        with self._lock:
            if self._entries is not None:
                if any(entry.readers for entry in self._entries.values()):
                    return Status(StatusCodes.DENIED)
                self._entries.clear()
                self._entries = None
            self._used_bytes = 0
        return Status.ok()

    def _store(self) -> OrderedDict[bytes, _Entry]:
        if self._entries is None:
            raise RuntimeError("memory connector is not open")
        return self._entries

    async def exists(self, key: bytes) -> Status:
        with self._lock:
            store = self._store()
            return (
                Status.ok() if key in store else Status(StatusCodes.NOT_FOUND)
            )

    async def get_bytes(self, key: bytes) -> Status[bytes | memoryview]:
        with self._lock:
            store = self._store()
            entry = store.get(key)
            if entry is None:
                return Status(StatusCodes.NOT_FOUND)
            store.move_to_end(key)
            return Status.ok(entry.payload)

    async def put_bytes(
        self, key: bytes, payload: bytes | memoryview
    ) -> Status:
        # A read-only memoryview cannot be mutated through itself; keep it
        # as is so large payloads are not copied while holding the GIL.
        immutable: bytes | memoryview
        if isinstance(payload, memoryview) and payload.readonly:
            immutable = payload
        else:
            immutable = bytes(payload)
        with self._lock:
            store = self._store()
            current = store.get(key)
            if current is not None:
                if current.payload != immutable:
                    return Status(StatusCodes.INVALID)
                store.move_to_end(key)
                return Status.ok()
            if len(immutable) > self.capacity_bytes:
                return Status(StatusCodes.OUT_OF_MEMORY)
            needed = self._used_bytes + len(immutable) - self.capacity_bytes
            victims: list[bytes] = []
            for victim_key, entry in store.items():
                if needed <= 0:
                    break
                if entry.readers == 0:
                    victims.append(victim_key)
                    needed -= len(entry.payload)
            if needed > 0:
                return Status(StatusCodes.OUT_OF_MEMORY)
            for victim in victims:
                removed = store.pop(victim)
                self._used_bytes -= len(removed.payload)
            store[key] = _Entry(immutable)
            self._used_bytes += len(immutable)
        return Status.ok()

    @asynccontextmanager
    async def lease_bytes(
        self, key: bytes
    ) -> AsyncIterator[bytes | memoryview]:
        with self._lock:
            store = self._store()
            entry = store.get(key)
            if entry is None:
                raise KeyError(key)
            entry.readers += 1
            store.move_to_end(key)
            payload = entry.payload
        try:
            yield payload
        finally:
            with self._lock:
                current = self._store().get(key)
                if current is entry:
                    current.readers -= 1

    async def delete(self, key: bytes) -> Status:
        with self._lock:
            store = self._store()
            entry = store.get(key)
            if entry is None:
                return Status.ok()
            if entry.readers:
                return Status(StatusCodes.DENIED)
            self._used_bytes -= len(entry.payload)
            del store[key]
        return Status.ok()

    async def get(
        self, key: bytes, mr: MemoryRegion | Sequence[MemoryRegion]
    ) -> Status:
        if not isinstance(mr, MemoryRegion):
            return Status(StatusCodes.INVALID)
        status = await self.get_bytes(key)
        if not status.is_ok():
            return Status(status)
        mr.fill(bytes(status.get()))
        return Status.ok()

    async def put(
        self, key: bytes, mr: MemoryRegion | Sequence[MemoryRegion]
    ) -> Status:
        if not isinstance(mr, MemoryRegion):
            return Status(StatusCodes.INVALID)
        return await self.put_bytes(key, mr.tobytes())

    def register_slabs(self, slabs: list[torch.Tensor]) -> Status:
        del slabs
        return Status.ok()

    def get_batches(
        self,
        keys: Sequence[Any],
        mrs: Sequence[MemoryRegion | Sequence[MemoryRegion]],
        batch_size: int,
    ) -> Sequence[
        Sequence[tuple[bytes, MemoryRegion | Sequence[MemoryRegion]]]
    ]:
        pairs = list(zip(keys, mrs))
        return [
            pairs[i : i + batch_size] for i in range(0, len(pairs), batch_size)
        ]
