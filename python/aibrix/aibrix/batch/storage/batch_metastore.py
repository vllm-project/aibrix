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
from datetime import datetime
from typing import Callable, Optional, Tuple

from aibrix import envs
from aibrix.batch.job_entity import (
    BatchJobState,
    BatchJobStatusCopy,
    aggregate_batch_job_status,
)
from aibrix.batch.job_entity.batch_job import BatchJob
from aibrix.logger import init_logger
from aibrix.storage import BaseStorage, StorageType, create_storage
from aibrix.storage.base import PutObjectOptionsBuilder

logger = init_logger(__name__)

p_metastore: Optional[BaseStorage] = None
NUM_REQUESTS_PER_READ = 1024
STATUS_REQUEST_LOCKING = "processing"
METASTORE_LIST_PAGE_SIZE = 1000
OLDEST_UNFINISHED_JOB_CREATED_AT_KEY = "batchjob_meta:oldest_unfinished_created_at"


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


async def put_batch_job(batch_id: str, job: BatchJob) -> None:
    """Persist a BatchJob document under ``batchjob:<id>``.

    Last-writer-wins. When the metastore is Redis-backed this is a
    single SET; on file-backed metastores it is a small object write.
    """
    payload = job.model_dump_json(by_alias=True, exclude_none=True)
    await set_metadata(f"batchjob:{batch_id}", payload)


async def get_batch_job(batch_id: str) -> Optional[BatchJob]:
    """Fetch a BatchJob document or ``None`` if absent."""
    raw, exists = await get_metadata(f"batchjob:{batch_id}")
    if not exists:
        return None
    job = BatchJob.model_validate_json(raw)
    if job.status.state in (
        BatchJobState.CREATED,
        BatchJobState.VALIDATING,
        BatchJobState.FINALIZED,
    ):
        return job

    # Load updated status copies
    status_copies = {}
    prefix = f"batchstatus_copies:{batch_id}:"
    for key in await list_metastore_all_keys(prefix):
        storage_key = key if key.startswith(prefix) else f"{prefix}{key}"
        worker_id = storage_key[len(prefix) :]
        status_json, exists = await get_metadata(storage_key)
        if exists:
            status_copies[worker_id] = BatchJobStatusCopy.model_validate_json(
                status_json
            )
    if len(status_copies) == 0:
        return job

    job.status.status_copies = status_copies
    job.status = aggregate_batch_job_status(job.status, False)
    return job


async def delete_batch_job(batch_id: str) -> None:
    """Remove the BatchJob document. Silent no-op if absent."""
    try:
        prefix = f"batchstatus_copies:{batch_id}:"
        for key in await list_metastore_all_keys(prefix):
            try:
                storage_key = key if key.startswith(prefix) else f"{prefix}{key}"
                await delete_metadata(storage_key)
            except FileNotFoundError:
                continue

        await delete_metadata(f"batchjob:{batch_id}")
    except FileNotFoundError:
        return


async def get_oldest_unfinished_job_created_at() -> Optional[datetime]:
    raw, exists = await get_metadata(OLDEST_UNFINISHED_JOB_CREATED_AT_KEY)
    if not exists:
        return None
    return datetime.fromisoformat(raw)


async def set_oldest_unfinished_job_created_at(
    created_at: Optional[datetime],
) -> None:
    if created_at is None:
        try:
            await delete_metadata(OLDEST_UNFINISHED_JOB_CREATED_AT_KEY)
        except FileNotFoundError:
            return
        return
    await set_metadata(
        OLDEST_UNFINISHED_JOB_CREATED_AT_KEY,
        created_at.isoformat(),
    )


async def list_batch_jobs(
    after: Optional[str] = None,
    limit: int = METASTORE_LIST_PAGE_SIZE,
    cached_job_getter: Optional[Callable[[str], Optional[BatchJob]]] = None,
) -> list[BatchJob]:
    """List BatchJob documents using public after/limit pagination."""
    keys, _ = await list_metastore_keys(
        "batchjob:",
        after_key=f"batchjob:{after}" if after is not None else None,
        limit=limit,
    )
    job_ids = [key.removeprefix("batchjob:") for key in keys]
    jobs: list[Optional[BatchJob]] = [None] * len(job_ids)
    uncached_indices: list[int] = []
    uncached_job_ids: list[str] = []
    for index, job_id in enumerate(job_ids):
        cached_job = (
            cached_job_getter(job_id) if cached_job_getter is not None else None
        )
        if cached_job is not None:
            # Do not copy here, reuse cached_job_getter returns
            jobs[index] = cached_job
            continue
        uncached_indices.append(index)
        uncached_job_ids.append(job_id)
    if uncached_job_ids:
        fetched_jobs = await asyncio.gather(
            *(get_batch_job(job_id) for job_id in uncached_job_ids)
        )
        for index, job in zip(uncached_indices, fetched_jobs):
            jobs[index] = job
    return [job for job in jobs if job is not None]


async def list_metastore_keys(
    prefix: str,
    continuation_token: Optional[str] = None,
    limit: int = METASTORE_LIST_PAGE_SIZE,
    after_key: Optional[str] = None,
) -> tuple[list[str], Optional[str]]:
    """List metastore keys matching the given prefix with optional pagination.

    Args:
        prefix: Key prefix to filter
        continuation_token: Pagination token from the previous page
        limit: Maximum number of keys to return
        after_key: Key to resume after when no continuation token is supplied

    Returns:
        Tuple of (keys, next_continuation_token)

    Raises:
        RuntimeError: If metastore has not been initialized
    """
    if p_metastore is None:
        raise RuntimeError(
            "Batch metastore not initialized. Call initialize_batch_metastore() first."
        )

    return await p_metastore.list_objects(
        prefix=prefix,
        continuation_token=continuation_token,
        limit=limit,
        after_key=after_key,
    )


async def list_metastore_all_keys(prefix: str) -> list[str]:
    """List all metastore keys matching the given prefix.


    Args:
        prefix: Key prefix to filter

    Returns:
        keys

    Raises:
        RuntimeError: If metastore has not been initialized
    """
    if p_metastore is None:
        raise RuntimeError(
            "Batch metastore not initialized. Call initialize_batch_metastore() first."
        )

    keys: list[str] = []
    continuation_token: Optional[str] = None

    while True:
        batch_keys, continuation_token = await p_metastore.list_objects(
            prefix=prefix,
            continuation_token=continuation_token,
            limit=METASTORE_LIST_PAGE_SIZE,  # Process in batches of 1000
        )
        keys.extend(batch_keys)

        if continuation_token is None:
            break

    return keys
