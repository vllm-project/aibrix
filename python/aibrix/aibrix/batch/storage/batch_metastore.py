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
import os
from dataclasses import replace
from datetime import datetime
from typing import Callable, Optional, Tuple
from urllib.parse import quote, unquote

from aibrix import envs
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobState,
    BatchJobStatusCopy,
    aggregate_batch_job_status,
)
from aibrix.logger import init_logger
from aibrix.storage import BaseStorage, StorageConfig, StorageType, create_storage
from aibrix.storage.base import PutObjectOptionsBuilder
from aibrix.storage.local import LOCAL_STORAGE_PATH_VAR
from aibrix.storage.types import StorageListOrdering

logger = init_logger(__name__)

p_metastore: Optional[BaseStorage] = None
NUM_REQUESTS_PER_READ = 1024
STATUS_REQUEST_LOCKING = "processing"
METASTORE_LIST_PAGE_SIZE = 1000
JOB_KEY_PREFIX = "batchjob"
JOB_STATUS_COPIES_PREFIX = "batchstatus_copies:%s"
OLDEST_UNFINISHED_JOB_CREATED_AT_KEY = "batchjob_meta:oldest_unfinished_created_at"


def _job_key(batch_id: str) -> str:
    return f"{JOB_KEY_PREFIX}/{batch_id}"


def _job_list_prefix() -> str:
    return f"{JOB_KEY_PREFIX}/"


def _status_copies_parent_key(batch_id: str) -> str:
    return JOB_STATUS_COPIES_PREFIX % batch_id


def _status_copies_list_prefix(batch_id: str) -> str:
    return f"{_status_copies_parent_key(batch_id)}/"


def _normalize_status_copy_worker_id(worker_id: str) -> str:
    return quote(worker_id, safe="._-@+()[]{}~")


def _decode_status_copy_worker_id(storage_worker_id: str) -> str:
    return unquote(storage_worker_id)


def _status_copy_key(batch_id: str, worker_id: str) -> str:
    encoded_worker_id = _normalize_status_copy_worker_id(worker_id)
    return f"{_status_copies_list_prefix(batch_id)}{encoded_worker_id}"


def initialize_batch_metastore(storage_type=StorageType.AUTO, params={}):
    """Initialize the batch metastore. Supported backends are LOCAL and REDIS.

    For some storage type, user needs to pass in other parameters to params.

    Args:
        storage_type: Legacy storage type enum
        params: Storage-specific parameters
    """
    global p_metastore

    if storage_type == StorageType.AUTO and envs.STORAGE_REDIS_AVAILABLE:
        storage_type = StorageType.REDIS

    # The LOCAL metastore lives under the configured storage root, so a dev/test
    # run that isolates STORAGE_LOCAL_PATH isolates the metastore too (rather
    # than sharing one cwd-relative ``.metastore``). Other substrates (redis /
    # s3 / tos) ignore base_path.
    local_root = os.environ.get(LOCAL_STORAGE_PATH_VAR)
    base_path = os.path.join(local_root, ".metastore") if local_root else ".metastore"

    # Create new storage instance and wrap with adapter
    try:
        create_params = dict(params or {})
        requested_config = create_params.pop("config", None)
        if requested_config is None:
            requested_config = StorageConfig()
        elif not isinstance(requested_config, StorageConfig):
            raise TypeError("params['config'] must be a StorageConfig instance")
        if requested_config.list_ordering is None:
            requested_config = replace(
                requested_config,
                list_ordering=StorageListOrdering.CREATED_AT_DESC,
            )
        elif requested_config.list_ordering != StorageListOrdering.CREATED_AT_DESC:
            logger.warning(
                "Metastore backend must use created_at-desc list ordering; overriding requested ordering.",
                storage=storage_type.value,
                old_ordering=requested_config.list_ordering.value,
                new_ordering=StorageListOrdering.CREATED_AT_DESC.value,
            )
            requested_config = replace(
                requested_config,
                list_ordering=StorageListOrdering.CREATED_AT_DESC,
            )

        logger.info(
            "Initializing batch metastore",
            storage_type=storage_type,
            config=requested_config,
            params=create_params,
        )  # type: ignore[call-arg]
        # Metastore scans require created_at-desc ordering. Backends that do not
        # support that contract reject the selected StorageConfig during
        # create_storage()/storage initialization.
        storage = create_storage(
            storage_type,
            config=requested_config,
            base_path=base_path,
            **create_params,
        )
        p_metastore = storage
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


def is_request_locking_supported() -> bool:
    """Whether the active metastore can lock requests.

    Request locking relies on both TTL (to auto-expire stale locks) and
    set-if-not-exists (NX) semantics. Backends that lack either -- notably the
    LOCAL filesystem metastore used for dev/test runs -- cannot lock requests,
    so callers should process every request without dedup instead of attempting
    a lock per request (which would raise ``ValueError`` every time).

    Returns:
        True if the metastore supports request locking, False otherwise

    Raises:
        RuntimeError: If metastore has not been initialized
    """
    if p_metastore is None:
        raise RuntimeError(
            "Batch metastore not initialized. Call initialize_batch_metastore() first."
        )
    return (
        p_metastore.is_ttl_supported() and p_metastore.is_set_if_not_exists_supported()
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
    """Persist a BatchJob document under ``JOB_KEY_PREFIX/<id>``.

    Last-writer-wins. When the metastore is Redis-backed this is a
    single SET; on file-backed metastores it is a small object write.
    """
    job_to_store = job.copy()
    status_copies = job_to_store.status.status_copies or {}
    job_to_store.status.status_copies = None
    # Store updated status copies
    for worker_id, status_copy in status_copies.items():
        if not status_copy.updated:
            continue

        status_json = status_copy.model_dump_json(by_alias=True, exclude_none=True)
        await set_metadata(_status_copy_key(batch_id, worker_id), status_json)
    # Store the main status document
    payload = job_to_store.model_dump_json(by_alias=True, exclude_none=True)
    await set_metadata(_job_key(batch_id), payload)


async def get_batch_job(batch_id: str) -> Optional[BatchJob]:
    """Fetch a BatchJob document or ``None`` if absent."""
    raw, exists = await get_metadata(_job_key(batch_id))
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
    prefix = _status_copies_list_prefix(batch_id)
    # Keep the trailing "/" in the listing prefix so every backend treats this
    # as "list children under this namespace". Redis normalizes either form,
    # but local/object-style listings would otherwise collapse to a common prefix.
    keys = await list_metastore_all_keys(prefix, delimiter="/")
    metadata_results = await asyncio.gather(
        *(get_metadata(storage_key) for storage_key in keys)
    )
    for storage_key, (status_json, exists) in zip(keys, metadata_results):
        if exists:
            worker_id = _decode_status_copy_worker_id(storage_key[len(prefix) :])
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
        prefix = _status_copies_list_prefix(batch_id)
        for key in await list_metastore_all_keys(prefix, delimiter="/"):
            try:
                await delete_metadata(key)
            except FileNotFoundError:
                continue

        await delete_metadata(_job_key(batch_id))
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
    """List BatchJob documents using backend-native created-time ordering."""
    keys, _ = await list_metastore_keys(
        _job_list_prefix(),
        delimiter="/",
        after_key=_job_key(after) if after is not None else None,
        limit=limit,
    )
    job_ids = [key.removeprefix(_job_list_prefix()) for key in keys]
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
    delimiter: Optional[str] = None,
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
        delimiter=delimiter,
        continuation_token=continuation_token,
        limit=limit,
        after_key=after_key,
    )


async def list_metastore_all_keys(
    prefix: str, delimiter: Optional[str] = None
) -> list[str]:
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
            delimiter=delimiter,
            continuation_token=continuation_token,
            limit=METASTORE_LIST_PAGE_SIZE,  # Process in batches of 1000
        )
        keys.extend(batch_keys)

        if continuation_token is None:
            break

    return keys
