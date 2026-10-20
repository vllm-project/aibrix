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

"""Behavior tests for the bounded memory connector."""

from concurrent.futures import ThreadPoolExecutor

import pytest

from aibrix_kvcache.l2.connectors import Connector, ConnectorConfig
from aibrix_kvcache.l2.connectors.memory import MemoryConnector


@pytest.mark.asyncio
async def test_reader_lease_prevents_eviction():
    connector = MemoryConnector(capacity_bytes=8)
    assert connector.open().is_ok()
    assert (await connector.put_bytes(b"a", b"1234")).is_ok()

    async with connector.lease_bytes(b"a") as payload:
        assert payload == b"1234"
        status = await connector.put_bytes(b"b", b"56789")
        assert status.is_out_of_memory()

    assert (await connector.put_bytes(b"b", b"56789")).is_ok()
    assert (await connector.get_bytes(b"a")).is_not_found()


@pytest.mark.asyncio
async def test_get_updates_lru_and_returned_bytes_survive_eviction():
    connector = MemoryConnector(capacity_bytes=8)
    connector.open().raise_if_not_ok()
    await connector.put_bytes(b"a", bytearray(b"aaaa"))
    await connector.put_bytes(b"b", b"bbbb")

    fetched = await connector.get_bytes(b"a")
    assert fetched.get() == b"aaaa"
    assert (await connector.put_bytes(b"c", b"cccc")).is_ok()

    assert fetched.get() == b"aaaa"
    assert (await connector.get_bytes(b"a")).is_ok()
    assert (await connector.get_bytes(b"b")).is_not_found()


@pytest.mark.asyncio
async def test_duplicate_key_is_idempotent_but_conflict_is_rejected():
    connector = MemoryConnector(capacity_bytes=16)
    connector.open().raise_if_not_ok()

    assert (await connector.put_bytes(b"key", b"same")).is_ok()
    assert (await connector.put_bytes(b"key", b"same")).is_ok()
    assert (await connector.put_bytes(b"key", b"other")).is_invalid()
    assert (await connector.get_bytes(b"key")).get() == b"same"
    assert connector.used_bytes == 4


@pytest.mark.asyncio
async def test_delete_rejects_leased_entry_then_succeeds():
    connector = MemoryConnector(capacity_bytes=8)
    connector.open().raise_if_not_ok()
    await connector.put_bytes(b"key", b"data")

    async with connector.lease_bytes(b"key"):
        assert (await connector.delete(b"key")).is_denied()

    assert (await connector.delete(b"key")).is_ok()
    assert (await connector.exists(b"key")).is_not_found()


@pytest.mark.asyncio
async def test_close_rejects_active_lease_then_succeeds():
    connector = MemoryConnector(capacity_bytes=8)
    connector.open().raise_if_not_ok()
    await connector.put_bytes(b"key", b"data")

    async with connector.lease_bytes(b"key"):
        assert connector.close().is_denied()
        assert (await connector.get_bytes(b"key")).get() == b"data"

    assert connector.close().is_ok()


def test_factory_requires_positive_memory_capacity(monkeypatch):
    config = ConnectorConfig(
        backend_name="MEMORY",
        namespace="test",
        partition_id="partition",
        executor=ThreadPoolExecutor(1),
    )
    monkeypatch.setenv("AIBRIX_KV_CACHE_OL_MEMORY_CAPACITY_BYTES", "0")
    with pytest.raises(ValueError, match="positive"):
        Connector.create(config)

    monkeypatch.setenv("AIBRIX_KV_CACHE_OL_MEMORY_CAPACITY_BYTES", "9")
    connector = Connector.create(config)
    assert connector.name == "Memory"
    assert connector.capacity_bytes == 9
    assert connector.feature.variable_payload


def test_read_only_buffer_is_stored_without_copy(monkeypatch):
    import asyncio

    import numpy as np

    monkeypatch.setenv("AIBRIX_KV_CACHE_OL_MEMORY_CAPACITY_BYTES", "64")
    connector = Connector.create(
        ConnectorConfig(
            backend_name="MEMORY",
            namespace="ro",
            partition_id="p",
            executor=ThreadPoolExecutor(1),
        )
    )
    assert connector.open().is_ok()
    array = np.arange(16, dtype=np.uint8)
    array.flags.writeable = False
    view = memoryview(array)
    assert asyncio.run(connector.put_bytes(b"k", view)).is_ok()
    stored = asyncio.run(connector.get_bytes(b"k")).get()
    assert stored is view and bytes(stored) == bytes(range(16))

    # Writable buffers are still copied so callers cannot mutate entries.
    data = bytearray(b"abc")
    assert asyncio.run(connector.put_bytes(b"w", data)).is_ok()
    data[0] = 0
    assert bytes(asyncio.run(connector.get_bytes(b"w")).get()) == b"abc"


@pytest.mark.asyncio
async def test_capacity_rejection_preserves_unleased_entries_and_lru():
    connector = MemoryConnector(capacity_bytes=12)
    connector.open().raise_if_not_ok()
    for key in (b"a", b"b", b"c"):
        (await connector.put_bytes(key, key * 4)).raise_if_not_ok()
    async with connector.lease_bytes(b"b"):
        assert (await connector.put_bytes(b"large", b"x" * 9)).is_out_of_memory()
        assert connector.used_bytes == 12
        for key in (b"a", b"b", b"c"):
            assert (await connector.exists(key)).is_ok()
        (await connector.put_bytes(b"fits", b"y" * 8)).raise_if_not_ok()
        assert (await connector.exists(b"a")).is_not_found()
        assert (await connector.exists(b"c")).is_not_found()
        assert (await connector.get_bytes(b"b")).get() == b"bbbb"
        assert connector.used_bytes == 12
