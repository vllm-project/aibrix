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

from typing import Optional, Tuple

from aibrix import envs
from aibrix.logger import init_logger
from aibrix.storage import BaseStorage, StorageType, create_storage
from aibrix.storage.base import PutObjectOptionsBuilder

logger = init_logger(__name__)

p_metastore: Optional[BaseStorage] = None
NUM_REQUESTS_PER_READ = 1024
STATUS_REQUEST_LOCKING = "processing"


def initialize_batch_metastore(storage_type=StorageType.AUTO, params={}):
    """Initialize storage type. Now it supports local files, S3, and TOS.

    For some storage type, user needs to pass in other parameters to params.

    Args:
        storage_type: Legacy storage type enum
        params: Storage-specific parameters
    """
    global p_metastore

    if storage_type == StorageType.AUTO and envs.STORAGE_REDIS_HOST:
        storage_type = StorageType.REDIS

    # Create new storage instance and wrap with adapter
    try:
        logger.info(
            "Initializing batch metastore", storage_type=storage_type, params=params
        )  # type: ignore[call-arg]
        p_metastore = create_storage(storage_type, base_path=".metastore", **params)
    except Exception as e:
        logger.error("Failed to initialize metastore", error=str(e))  # type: ignore[call-arg]
        raise


def get_metastore_type() -> StorageType:
    """Get the type of metastore.

    Returns:
        Type of type of storage that backs metastore

    Raises:
        RuntimeError: If metastore has not been initialized
    """
    if p_metastore is None:
        raise RuntimeError(
            "Batch metastore not initialized. Call initialize_batch_metastore() first."
        )
    return p_metastore.get_type()


async def set_metadata(
    key: str,
    value: str,
    expiration_seconds: Optional[int] = None,
    if_not_exists: bool = False,
) -> bool:
    """Set metadata to metastore with advanced options.

    Args:
        key: Metadata key
        value: Metadata value
        expiration_seconds: TTL in seconds for the key (Redis only)
        if_not_exists: Only set if key doesn't exist (Redis NX operation)

    Returns:
        True if metadata was set, False if conditional operation failed

    Raises:
        RuntimeError: If metastore has not been initialized
        ValueError: If unsupported options are used with non-Redis storage
    """
    if p_metastore is None:
        raise RuntimeError(
            "Batch metastore not initialized. Call initialize_batch_metastore() first."
        )

    # Build options if needed
    options = None
    if expiration_seconds is not None or if_not_exists:
        builder = PutObjectOptionsBuilder()
        if expiration_seconds is not None:
            builder.ttl_seconds(expiration_seconds)
        if if_not_exists:
            builder.if_not_exists()
        options = builder.build()

    return await p_metastore.put_object(key, value, options=options)


async def get_metadata(key: str) -> Tuple[str, bool]:
    """Get metadata from metastore.

    Args:
        key: Metadata key

    Returns:
        Metadata value

    Raises:
        RuntimeError: If metastore has not been initialized
    """
    if p_metastore is None:
        raise RuntimeError(
            "Batch metastore not initialized. Call initialize_batch_metastore() first."
        )

    try:
        data = await p_metastore.get_object(key)
        return data.decode("utf-8"), True
    except FileNotFoundError:
        return "", False


async def delete_metadata(key: str) -> None:
    """Delete metadata from metastore.

    Args:
        key: Metadata key

    Raises:
        RuntimeError: If metastore has not been initialized
    """
    if p_metastore is None:
        raise RuntimeError(
            "Batch metastore not initialized. Call initialize_batch_metastore() first."
        )
    await p_metastore.delete_object(key)


async def lock_request(key: str, expiration_seconds: int = 3600) -> bool:
    """Lock a request for processing.

    Args:
        key: Request key to lock
        expiration_seconds: TTL for the lock (default 1 hour)

    Returns:
        True if lock was acquired, False if already locked

    Raises:
        ValueError: If storage doesn't support locking operations
    """
    return await set_metadata(
        key,
        STATUS_REQUEST_LOCKING,
        expiration_seconds=expiration_seconds,
        if_not_exists=True,
    )


async def is_request_done(key: str) -> bool:
    """Check if a request is done.

    Args:
        key: Request key to check

    Returns:
        True if the request is done, False otherwise
    """
    status, got = await get_metadata(key)
    return got and status != STATUS_REQUEST_LOCKING


async def unlock_request(key: str, status: str) -> bool:
    """Unlock a request by setting completion status.

    Args:
        key: Request key to unlock
        status: Completion status (e.g., "output:etag" or "error:etag")

    Returns:
        True if status was set successfully
    """
    return await set_metadata(key, status)


async def list_metastore_keys(prefix: str) -> list[str]:
    """List all keys from metastore matching the given prefix.

    Args:
        prefix: Key prefix to filter

    Returns:
        List of keys matching the prefix

    Raises:
        RuntimeError: If metastore has not been initialized
    """
    if p_metastore is None:
        raise RuntimeError(
            "Batch metastore not initialized. Call initialize_batch_metastore() first."
        )

    keys = []
    continuation_token = None

    while True:
        batch_keys, continuation_token = await p_metastore.list_objects(
            prefix=prefix,
            continuation_token=continuation_token,
            limit=1000,  # Process in batches of 1000
        )
        keys.extend(batch_keys)

        if continuation_token is None:
            break

    return keys
