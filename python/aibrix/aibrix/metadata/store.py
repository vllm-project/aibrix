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

import time
from abc import ABC, abstractmethod
from typing import Optional

import aibrix.client.redis as redis
from aibrix.logger import init_logger
from aibrix.metadata.core.metrics import (
    Emitter,
    T,
    duration_ms,
    metrics_names,
)

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

    def __init__(self):
        self._client = redis.get_redis_client()
        logger.info("Redis metadata store initialized")

    @property
    def client(self) -> redis.AsyncRedis:
        """Expose underlying Redis client for backward compatibility.

        This property allows existing code that directly accesses the
        Redis client to continue working during the migration period.
        New code should use the MetadataStore interface methods instead.
        """
        return self._client

    @staticmethod
    def _tags(operation: str):
        return (T("backend", "redis"), T("operation", operation))

    async def get(self, key: str) -> Optional[bytes]:
        start_time = time.perf_counter()
        tags = self._tags("get")
        try:
            return await self._client.get(key)
        except Exception:
            Emitter.counter(metrics_names.METRIC_METADATA_STORE_ERROR, 1, *tags)
            raise
        finally:
            duration_ms(
                Emitter, metrics_names.METRIC_METADATA_STORE_DURATION, start_time, *tags
            )

    async def set(self, key: str, value: str | bytes) -> bool:
        start_time = time.perf_counter()
        tags = self._tags("set")
        try:
            result = await self._client.set(key, value)
            return bool(result)
        except Exception:
            Emitter.counter(metrics_names.METRIC_METADATA_STORE_ERROR, 1, *tags)
            raise
        finally:
            duration_ms(
                Emitter, metrics_names.METRIC_METADATA_STORE_DURATION, start_time, *tags
            )

    async def exists(self, key: str) -> bool:
        start_time = time.perf_counter()
        tags = self._tags("exists")
        try:
            result = await self._client.exists(key)
            return bool(result)
        except Exception:
            Emitter.counter(metrics_names.METRIC_METADATA_STORE_ERROR, 1, *tags)
            raise
        finally:
            duration_ms(
                Emitter, metrics_names.METRIC_METADATA_STORE_DURATION, start_time, *tags
            )

    async def delete(self, key: str) -> bool:
        start_time = time.perf_counter()
        tags = self._tags("delete")
        try:
            result = await self._client.delete(key)
            return bool(result)
        except Exception:
            Emitter.counter(metrics_names.METRIC_METADATA_STORE_ERROR, 1, *tags)
            raise
        finally:
            duration_ms(
                Emitter, metrics_names.METRIC_METADATA_STORE_DURATION, start_time, *tags
            )

    async def ping(self) -> bool:
        start_time = time.perf_counter()
        tags = self._tags("ping")
        try:
            return await self._client.ping()
        except Exception:
            Emitter.counter(metrics_names.METRIC_METADATA_STORE_ERROR, 1, *tags)
            logger.exception("Redis metadata store ping failed")
            return False
        finally:
            duration_ms(
                Emitter, metrics_names.METRIC_METADATA_STORE_DURATION, start_time, *tags
            )

    async def close(self) -> None:
        start_time = time.perf_counter()
        tags = self._tags("close")
        try:
            await self._client.aclose()  # type: ignore[attr-defined]
            logger.info("Redis metadata store closed")
        except Exception:
            Emitter.counter(metrics_names.METRIC_METADATA_STORE_ERROR, 1, *tags)
            raise
        finally:
            duration_ms(
                Emitter, metrics_names.METRIC_METADATA_STORE_DURATION, start_time, *tags
            )
