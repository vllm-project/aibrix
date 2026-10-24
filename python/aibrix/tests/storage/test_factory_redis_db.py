# Copyright 2026 The Aibrix Team.
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

import asyncio
from unittest.mock import AsyncMock, Mock

import pytest

import aibrix.client.redis as redis_client
from aibrix import envs
from aibrix.storage.factory import create_storage
from aibrix.storage.types import StorageType


@pytest.mark.parametrize("storage_type", [StorageType.REDIS, "REDIS"])
@pytest.mark.parametrize(
    "overrides, expected_db",
    [({}, 7), ({"db": None}, 7), ({"db": 0}, 0), ({"db": 3}, 3)],
    ids=["omitted", "none", "zero", "nonzero"],
)
def test_factory_redis_database_selection(
    monkeypatch, storage_type, overrides, expected_db
):
    monkeypatch.setattr(envs, "STORAGE_REDIS_DB", 7)
    client = Mock(aclose=AsyncMock())
    client_factory = Mock(return_value=client)
    monkeypatch.setattr(redis_client, "get_redis_client", client_factory)
    storage = create_storage(
        storage_type,
        host="redis.example.test",
        port=6380,
        password="test-password",
        **overrides,
    )

    async def initialize_client():
        try:
            await storage._get_redis()
        finally:
            await storage.close()

    asyncio.run(initialize_client())

    client_factory.assert_called_once_with(
        host="redis.example.test",
        port=6380,
        db=expected_db,
        password="test-password",
        decode_responses=False,
    )
    client.aclose.assert_awaited_once()
