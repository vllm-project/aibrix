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

"""
Metadata store abstraction layer.

Provides a clean interface for metadata key-value operations
(e.g., user CRUD) instead of directly referencing a Redis client.
This allows for easier testing, backend swapping, and separation of concerns.
"""

from abc import ABC, abstractmethod
from typing import Optional

import redis.asyncio as redis

from aibrix.logger import init_logger

logger = init_logger(__name__)


class MetadataStore(ABC):
    """Abstract base class for metadata key-value storage.

    This interface defines the operations needed by the metadata service
    for storing and retrieving structured data (users, configs, etc.).
    Unlike the storage.BaseStorage which is designed for file/object storage,
    this interface is tailored for simple key-value metadata operations.
    """

    @abstractmethod
    async def get(self, key: str) -> Optional[bytes]:
        """Get value by key.

        Args:
            key: The key to look up.

        Returns:
            The value as bytes if found, None otherwise.
        """
        ...

    @abstractmethod
    async def set(self, key: str, value: str | bytes) -> bool:
        """Set a key-value pair.

        Args:
            key: The key to store.
            value: The value to store (string or bytes).

        Returns:
            True if the operation succeeded.
        """
        ...

    @abstractmethod
    async def exists(self, key: str) -> bool:
        """Check if a key exists.

        Args:
            key: The key to check.

        Returns:
            True if the key exists, False otherwise.
        """
        ...

    @abstractmethod
    async def delete(self, key: str) -> bool:
        """Delete a key.

        Args:
            key: The key to delete.

        Returns:
            True if the key was deleted, False if it didn't exist.
        """
        ...

    @abstractmethod
    async def ping(self) -> bool:
        """Check if the store backend is reachable.

        Returns:
            True if the backend is healthy.
        """
        ...

    @abstractmethod
    async def close(self) -> None:
        """Close the connection to the store backend."""
        ...


class RedisMetadataStore(MetadataStore):
    """Redis-backed implementation of the metadata store."""

    def __init__(
        self,
        host: str = "localhost",
        port: int = 6379,
        db: int = 0,
        password: Optional[str] = None,
    ):
        self._client = redis.Redis(
            host=host,
            port=port,
            db=db,
            password=password,
            decode_responses=False,
        )
        logger.info(f"Redis metadata store initialized: {host}:{port}")

    @property
    def client(self) -> redis.Redis:
        """Expose underlying Redis client for backward compatibility.

        This property allows existing code that directly accesses the
        Redis client to continue working during the migration period.
        New code should use the MetadataStore interface methods instead.
        """
        return self._client

    async def get(self, key: str) -> Optional[bytes]:
        data = await self._client.get(key)
        return data

    async def set(self, key: str, value: str | bytes) -> bool:
        result = await self._client.set(key, value)
        return bool(result)

    async def exists(self, key: str) -> bool:
        result = await self._client.exists(key)
        return bool(result)

    async def delete(self, key: str) -> bool:
        result = await self._client.delete(key)
        return bool(result)

    async def ping(self) -> bool:
        try:
            return await self._client.ping()
        except Exception:
            logger.exception("Redis metadata store ping failed")
            return False

    async def close(self) -> None:
        await self._client.aclose()
        logger.info("Redis metadata store closed")
