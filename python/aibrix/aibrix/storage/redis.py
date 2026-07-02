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

import asyncio
import time
from typing import BinaryIO, Optional, TextIO, Union

import aibrix.client.redis as redis
from aibrix.storage.base import (
    PutObjectOptions,
    StorageConfig,
    StorageType,
)
from aibrix.storage.base2 import BaseStorage2
from aibrix.storage.reader import Reader
from aibrix.storage.types import StorageListOrdering
from aibrix.storage.utils import ObjectMetadata


class RedisStorage(BaseStorage2):
    """Redis storage implementation.

    This implementation uses Redis as a key-value store with the following features:
    - No content_type or metadata support for put_object
    - No head_object or object_exists support
    - Hierarchical key support using Redis sets (e.g., "xxx/yyy" creates set "xxx:index")
    - Simple get/put/delete operations
    - List operations that work with Redis structures
    - Timestamp-ordered listing for both created_at ascending and descending

    Timestamp Tracking:
    - All keys are tracked in "timestamps:all" sorted set with creation time as score
    - Hierarchical keys are also tracked in "timestamps:{parent}" sorted sets
    - Version 1 stored positive timestamps (oldest-first ordering)
    - Version 2 stored negative timestamps so the existing ascending zset
      pagination returned newest-first results
    - Version 3 stores positive timestamps and supports both created_at
      ascending and descending order
    - Listing chooses ZRANGE/ZRANK for ascending order and
      ZREVRANGE/ZREVRANK for descending order
    - Timestamps are cleaned up automatically when objects are deleted
    """

    def __init__(
        self,
        config: Optional[StorageConfig] = None,
        **kwargs,
    ):
        super().__init__(config)
        self._redis_clients: dict[int, redis.AsyncRedis] = {}
        self._kwargs = kwargs

    def get_type(self) -> StorageType:
        """Get the type of storage.

        Returns:
            Type of storage, set to StorageType.REDIS
        """
        return StorageType.REDIS

    @classmethod
    def get_supported_list_orderings(cls) -> tuple[StorageListOrdering, ...]:
        return (
            StorageListOrdering.CREATED_AT_DESC,
            StorageListOrdering.CREATED_AT_ASC,
        )

    def _is_created_at_desc_order(self) -> bool:
        return self.get_list_ordering() == StorageListOrdering.CREATED_AT_DESC

    async def _zrange_by_list_order(
        self,
        redis_client: redis.AsyncRedis,
        key: str,
        start: int,
        end: int,
        withscores: bool = False,
    ):
        if self._is_created_at_desc_order():
            return await redis_client.zrevrange(key, start, end, withscores=withscores)
        return await redis_client.zrange(key, start, end, withscores=withscores)

    async def _zrank_by_list_order(
        self,
        redis_client: redis.AsyncRedis,
        key: str,
        member: str,
    ) -> Optional[int]:
        if self._is_created_at_desc_order():
            return await redis_client.zrevrank(key, member)
        return await redis_client.zrank(key, member)

    async def _get_redis(self) -> redis.AsyncRedis:
        """Get Redis connection, creating it if necessary."""
        loop_id = id(asyncio.get_running_loop())
        redis_client = self._redis_clients.get(loop_id)
        if redis_client is None:
            client_kwargs = {**self._kwargs, "decode_responses": False}  # Keep as bytes
            redis_client = redis.get_redis_client(**client_kwargs)
            self._redis_clients[loop_id] = redis_client
        return redis_client

    async def close(self):
        """Close Redis connection."""
        for redis_client in self._redis_clients.values():
            await redis_client.aclose()
        self._redis_clients.clear()

    def _parse_hierarchical_key(self, key: str) -> tuple[Optional[str], str]:
        """Parse hierarchical key into parent and item.

        Args:
            key: Key like "xxx/yyy" or "simple_key"

        Returns:
            Tuple of (parent_key, item_key). For "xxx/yyy" returns ("xxx", "yyy").
            For "simple_key" returns (None, "simple_key").
        """
        if "/" in key:
            parts = key.split("/", 1)
            return parts[0], parts[1]
        return None, key

    async def put_object(
        self,
        key: str,
        data: Union[bytes, str, BinaryIO, TextIO, Reader],
        content_type: Optional[str] = None,  # Ignored for Redis
        metadata: Optional[dict[str, str]] = None,  # Ignored for Redis
        options: Optional[PutObjectOptions] = None,
    ) -> bool:
        """Put an object to Redis storage with advanced options.

        If key contains "/", creates a Redis list for the parent part.
        For example, "xxx/yyy" will create a list "xxx" and add "yyy" to it,
        then store the actual data under the full key "xxx/yyy".

        Args:
            key: Object key/path
            data: Data to store
            content_type: Ignored for Redis
            metadata: Ignored for Redis
            options: Advanced options for put operation

        Returns:
            True if object was stored, False if conditional operation failed
        """
        # Validate options
        self._validate_put_options(options)

        redis_client = await self._get_redis()

        # Convert data to bytes
        if isinstance(data, str):
            data_bytes = data.encode("utf-8")
        elif isinstance(data, bytes):
            data_bytes = data
        else:
            # File-like object or Reader
            reader = self._wrap_data(data)
            data_bytes = reader.read_all()

        # Parse hierarchical key
        parent_key, item_key = self._parse_hierarchical_key(key)

        # Prepare Redis SET options from PutObjectOptions
        redis_ex = None
        redis_px = None
        redis_nx = False
        redis_xx = False

        if options:
            if options.ttl_seconds is not None:
                redis_ex = options.ttl_seconds
            elif options.ttl_milliseconds is not None:
                redis_px = options.ttl_milliseconds

            redis_nx = options.set_if_not_exists
            redis_xx = options.set_if_exists

        # Store the actual data with options
        result = await redis_client.set(
            key, data_bytes, ex=redis_ex, px=redis_px, nx=redis_nx, xx=redis_xx
        )

        # Check if the SET operation succeeded
        if result is None:
            # Conditional SET failed (NX or XX condition not met)
            return False

        # Store canonical positive created_at timestamps. Listing direction is
        # selected at read time via ZRANGE/ZREVRANGE based on StorageConfig.
        # Use ZADD NX so first insert wins without a preceding ZSCORE, then
        # fall back to reading the canonical global score only when a parent
        # index needs to mirror it on overwrite.
        timestamp = time.time()
        global_timestamp_added = await redis_client.zadd(
            "timestamps:all", {key: timestamp}, nx=True
        )

        # If hierarchical, add to parent list and track timestamp
        if parent_key is not None:
            if global_timestamp_added == 0:
                # Always rewrite the parent timestamp entry on put. ``SADD`` only
                # proves the membership set already contains ``item_key``; it
                # does not guarantee ``timestamps:{parent}`` still has the member
                # or that it still carries the canonical created-at score from
                # ``timestamps:all``. Re-applying the parent ZADD keeps the
                # hierarchical ordering index aligned and repairs partial drift
                # from interrupted writes or manual Redis changes.
                existing_timestamp = await redis_client.zscore("timestamps:all", key)
                if existing_timestamp is None:
                    raise RuntimeError(
                        f"Redis timestamps:all lost score for existing key {key!r}"
                    )
                timestamp = float(existing_timestamp)
            # Add item to parent list (only if not already present)
            await redis_client.sadd(f"{parent_key}:index", item_key)
            # Also track timestamp for hierarchical objects
            await redis_client.zadd(f"timestamps:{parent_key}", {item_key: timestamp})

        return True

    async def get_object(
        self,
        key: str,
        range_start: Optional[int] = None,
        range_end: Optional[int] = None,
    ) -> bytes:
        """Get an object from Redis storage.

        Args:
            key: Object key/path
            range_start: Start byte position for range get
            range_end: End byte position for range get (inclusive)

        Returns:
            Object data as bytes

        Raises:
            FileNotFoundError: If object does not exist
        """
        redis_client = await self._get_redis()

        data = await redis_client.get(key)
        if data is None:
            raise FileNotFoundError(f"Object not found: {key}")
        if isinstance(data, str):
            data = data.encode("utf-8")

        # Handle range requests
        if range_start is not None:
            if range_end is not None:
                return data[range_start : range_end + 1]
            else:
                return data[range_start:]

        return data

    async def delete_object(self, key: str) -> None:
        """Delete an object from Redis storage.

        Also removes the key from parent list if it's hierarchical.

        Args:
            key: Object key/path
        """
        redis_client = await self._get_redis()

        # Parse hierarchical key
        parent_key, item_key = self._parse_hierarchical_key(key)

        # Delete the actual data
        await redis_client.delete(key)

        # Clean up timestamp tracking
        await redis_client.zrem("timestamps:all", key)

        # If hierarchical, remove from parent list and timestamps
        if parent_key is not None:
            await redis_client.srem(f"{parent_key}:index", item_key)
            await redis_client.zrem(f"timestamps:{parent_key}", item_key)

    async def list_objects(
        self,
        prefix: str = "",
        delimiter: Optional[str] = None,
        limit: Optional[int] = None,
        continuation_token: Optional[str] = None,
        after_key: Optional[str] = None,
    ) -> tuple[list[str], Optional[str]]:
        """List objects with given prefix ordered by creation timestamp.

        For Redis, this works with both direct keys and hierarchical structures:
        - If prefix corresponds to a Redis list (has :index suffix), returns list members ordered by creation time
        - Otherwise, scans for keys matching the prefix pattern and orders by creation time
        - Supports efficient token-based pagination using Redis ZRANGE

        Args:
            prefix: Key prefix to filter objects
            delimiter: Delimiter for hierarchical listing (typically "/")
            limit: Maximum number of objects to return (None for no limit)
            continuation_token: Offset position as string (e.g., "10" for offset 10)

        Returns:
            Tuple of (object_keys, next_continuation_token)
            - object_keys: List of object keys ordered by created_at descending.
            - next_continuation_token: String offset for next page (None if no more pages)
        """
        redis_client = await self._get_redis()

        # Parse continuation token as offset (default to 0)
        offset = 0
        if continuation_token:
            try:
                offset = int(continuation_token)
            except (ValueError, TypeError):
                offset = 0

        # Allow callers to pass either the parent prefix ("batchjob") or the
        # slash-qualified listing prefix ("batchjob/") for hierarchical keys.
        hierarchical_prefix = prefix
        parent_prefix = prefix
        if delimiter and prefix.endswith(delimiter):
            parent_prefix = prefix[: -len(delimiter)]
        elif delimiter and prefix:
            hierarchical_prefix = f"{prefix}{delimiter}"

        # Check if this prefix corresponds to a list index
        list_key = f"{parent_prefix}:index"
        if await redis_client.exists(list_key):
            # Get members ordered by timestamp from the sorted set
            timestamp_key = f"timestamps:{parent_prefix}"
            members: list[str]
            if await redis_client.exists(timestamp_key):
                if continuation_token is None and after_key:
                    if delimiter and after_key.startswith(hierarchical_prefix):
                        after_member = after_key[len(hierarchical_prefix) :]
                    else:
                        after_member = after_key
                    rank = await self._zrank_by_list_order(
                        redis_client, timestamp_key, after_member
                    )
                    if rank is None:
                        return [], None
                    offset = rank + 1
                # Calculate pagination bounds. When paginating, read one extra
                # member so ``has_more`` can be derived without a second Redis
                # round trip, then trim the page back to ``limit`` below.
                start = offset
                end = offset + limit if limit is not None else -1

                # Read the zset in the configured created_at direction while
                # keeping pagination based on logical rank offsets.
                members_with_scores = await self._zrange_by_list_order(
                    redis_client, timestamp_key, start, end, withscores=False
                )
                members = [member.decode("utf-8") for member in members_with_scores]

                # Trim the extra member after probing for ``has_more``.
                has_more = limit is not None and len(members) > limit
                if has_more:
                    members = members[:limit]
            else:
                # Fallback to unordered members if no timestamps
                members_raw = await redis_client.smembers(list_key)
                all_members: list[str] = [
                    member.decode("utf-8") for member in members_raw
                ]
                if continuation_token is None and after_key:
                    if delimiter and after_key.startswith(hierarchical_prefix):
                        after_member = after_key[len(hierarchical_prefix) :]
                    else:
                        after_member = after_key
                    try:
                        offset = all_members.index(after_member) + 1
                    except ValueError:
                        return [], None

                # Apply pagination to unordered list
                if offset > 0:
                    all_members = all_members[offset:]

                members = all_members[:limit] if limit is not None else all_members
                has_more = limit is not None and len(all_members) > limit

            if delimiter:
                # Return hierarchical format with delimiter
                result_keys = [f"{hierarchical_prefix}{member}" for member in members]
            else:
                # Return just the member names
                result_keys = members

            # Generate next token
            next_token = str(offset + len(members)) if has_more else None
            return result_keys, next_token

        # Fallback to key scanning with timestamp ordering
        if prefix:
            # For prefix searches, get all keys from timestamp sorted set and filter
            all_keys_with_timestamps = await self._zrange_by_list_order(
                redis_client, "timestamps:all", 0, -1, withscores=False
            )

            filtered_keys = []
            for key_bytes in all_keys_with_timestamps:
                key_str = key_bytes.decode("utf-8")
                # Filter by prefix and exclude internal keys
                if (
                    key_str.startswith(prefix)
                    and not key_str.endswith(":index")
                    and not key_str.startswith("timestamps:")
                ):
                    filtered_keys.append(key_str)

            if continuation_token is None and after_key:
                try:
                    offset = filtered_keys.index(after_key) + 1
                except ValueError:
                    return [], None

            # Apply pagination after filtering
            if offset > 0:
                filtered_keys = filtered_keys[offset:]

            keys = filtered_keys[:limit] if limit is not None else filtered_keys
            has_more = limit is not None and len(filtered_keys) > limit
        else:
            # For no prefix, get all keys first, then filter and paginate
            # This ensures consistent pagination even when internal keys are present
            all_keys_with_timestamps = await self._zrange_by_list_order(
                redis_client, "timestamps:all", 0, -1, withscores=False
            )
            all_user_keys = []
            for key_bytes in all_keys_with_timestamps:
                key_str = key_bytes.decode("utf-8")
                # Exclude internal keys
                if not key_str.endswith(":index") and not key_str.startswith(
                    "timestamps:"
                ):
                    all_user_keys.append(key_str)

            if continuation_token is None and after_key:
                try:
                    offset = all_user_keys.index(after_key) + 1
                except ValueError:
                    return [], None

            # Apply pagination to filtered keys
            paginated_keys = all_user_keys[offset:]

            keys = paginated_keys[:limit] if limit is not None else paginated_keys
            has_more = limit is not None and len(paginated_keys) > limit

        # Apply delimiter filtering if specified
        if delimiter and prefix:
            filtered_keys = []
            for key in keys:
                if key.startswith(prefix):
                    remaining = key[len(prefix) :]
                    if delimiter in remaining:
                        # Extract the next level only
                        next_part = remaining.split(delimiter, 1)[0]
                        hierarchical_key = f"{prefix}{next_part}{delimiter}"
                        if hierarchical_key not in filtered_keys:
                            filtered_keys.append(hierarchical_key)
                    else:
                        filtered_keys.append(key)
            keys = filtered_keys
            # Note: has_more might not be accurate after delimiter filtering

        # Generate next token
        next_token = str(offset + len(keys)) if has_more else None
        return keys, next_token

    async def object_exists(self, key: str) -> bool:
        """Check if object exists.

        Note: Not directly supported as per requirements, but implemented
        for compatibility with base class.

        Args:
            key: Object key/path

        Returns:
            True if object exists, False otherwise
        """
        redis_client = await self._get_redis()
        return bool(await redis_client.exists(key))

    async def get_object_size(self, key: str) -> int:
        """Get object size in bytes.

        Args:
            key: Object key/path

        Returns:
            Object size in bytes

        Raises:
            FileNotFoundError: If object does not exist
        """
        redis_client = await self._get_redis()

        size = await redis_client.strlen(key)
        if size == 0:
            # Check if key actually exists
            if not await redis_client.exists(key):
                raise FileNotFoundError(f"Object not found: {key}")

        return size

    async def head_object(self, key: str) -> ObjectMetadata:
        """Get object metadata.

        Note: Not supported for Redis as per requirements.

        Args:
            key: Object key/path

        Raises:
            NotImplementedError: Redis storage doesn't support metadata
        """
        raise NotImplementedError("head_object not supported for Redis storage")

    async def _native_create_multipart_upload(
        self,
        key: str,
        content_type: Optional[str] = None,
        metadata: Optional[dict[str, str]] = None,
    ) -> str:
        """Create a multipart upload session.

        Note: Not needed for Redis as per requirements.

        Raises:
            NotImplementedError: Multipart upload not needed for Redis
        """
        raise NotImplementedError("Multipart upload not needed for Redis storage")

    async def _native_upload_part(
        self,
        key: str,
        upload_id: str,
        part_number: int,
        data: Union[str, bytes, BinaryIO, TextIO, Reader],
    ) -> str:
        """Upload a part in a multipart upload.

        Note: Not needed for Redis as per requirements.

        Raises:
            NotImplementedError: Multipart upload not needed for Redis
        """
        raise NotImplementedError("Multipart upload not needed for Redis storage")

    async def _native_complete_multipart_upload(
        self,
        key: str,
        upload_id: str,
        parts: list[dict[str, Union[str, int]]],
    ) -> None:
        """Complete a multipart upload.

        Note: Not needed for Redis as per requirements.

        Raises:
            NotImplementedError: Multipart upload not needed for Redis
        """
        raise NotImplementedError("Multipart upload not needed for Redis storage")

    async def _native_abort_multipart_upload(
        self,
        key: str,
        upload_id: str,
    ) -> None:
        """Abort a multipart upload.

        Note: Not needed for Redis as per requirements.

        Raises:
            NotImplementedError: Multipart upload not needed for Redis
        """
        raise NotImplementedError("Multipart upload not needed for Redis storage")

    # Feature Support Methods
    def is_ttl_supported(self) -> bool:
        """Check if TTL (Time To Live) is supported.

        Returns:
            True - Redis supports TTL
        """
        return True

    def is_set_if_not_exists_supported(self) -> bool:
        """Check if conditional SET IF NOT EXISTS is supported.

        Returns:
            True - Redis supports NX option
        """
        return True

    def is_set_if_exists_supported(self) -> bool:
        """Check if conditional SET IF EXISTS is supported.

        Returns:
            True - Redis supports XX option
        """
        return True
