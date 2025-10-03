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

import uuid
from typing import Any, AsyncIterator, Dict, List, Optional, Tuple

from aibrix.batch.job_entity import BatchJob
from aibrix.batch.storage.adapter import BatchStorageAdapter
from aibrix.logger import init_logger
from aibrix.storage import StorageType, create_storage

logger = init_logger(__name__)

p_storage: Optional[BatchStorageAdapter] = None


def initialize_storage(storage_type=StorageType.AUTO, params={}):
    """Initialize storage type. Now it supports local files, S3, and TOS.

    For some storage type, user needs to pass in other parameters to params.

    Args:
        storage_type: Legacy storage type enum
        params: Storage-specific parameters
    """
    global p_storage

    # Create new storage instance and wrap with adapter
    try:
        logger.info(
            "Initializing batch storage", storage_type=storage_type, params=params
        )  # type: ignore[call-arg]
        storage = create_storage(storage_type, **params)
        p_storage = BatchStorageAdapter(storage)
    except Exception as e:
        logger.error("Failed to initialize storage", error=str(e))  # type: ignore[call-arg]
        raise


def get_storage_type() -> StorageType:
    """Get the type of storage.

    Returns:
        Type of storage.
    """
    assert p_storage is not None
    return p_storage.storage.get_type()


async def upload_input_data(inputDataFileName: str) -> str:
    """Upload job input data file to storage.

    Args:
        inputDataFileName (str): an input file string.
    """
    assert p_storage is not None
    job_id = str(uuid.uuid1())
    await p_storage.write_job_input_data(job_id, inputDataFileName)

    return job_id


async def read_job_input_info(job: BatchJob) -> Tuple[int, bool]:
    """Read job input info from storage.

    Args:
        job: BatchJob

    Returns:
        Tuple of total line number and input existence
    """
    assert p_storage is not None
    return await p_storage.read_job_input_info(job)


async def read_job_next_request(
    job: BatchJob, start_index: int = 0
) -> AsyncIterator[Dict[str, Any]]:
    """Read next request from job input data.

    Args:
        job_id: Job identifier

    Returns:
        Next request dictionary
    """
    assert p_storage is not None
    async for data in p_storage.read_job_next_input_data(job, start_index):
        yield data


async def is_request_done(job: BatchJob, request_index: int) -> bool:
    """Check if a request is done.

    Args:
        job: BatchJob
        request_index: Index of the request being processed

    Returns:
        True if the request is done, False otherwise
    """
    assert p_storage is not None
    return await p_storage.is_request_done(job, request_index)


async def prepare_job_ouput_files(job: BatchJob) -> BatchJob:
    """Prepare job output files, including output and error file ids"""
    assert p_storage is not None
    return await p_storage.prepare_job_ouput_files(job)


async def write_job_output_data(
    job: BatchJob, request_index: int, output_data: Dict[str, Any]
) -> None:
    """Write job result to storage and unlock the request.

    Args:
        job: BatchJob object
        request_index: Index of the request being processed
        output_data: Single result dictionary
    """
    assert p_storage is not None
    await p_storage.write_job_output_data(job, request_index, output_data)


async def finalize_job_output_data(job: BatchJob) -> None:
    """Finalize job output files, aggregate output and error files"""
    assert p_storage is not None
    await p_storage.finalize_job_output_data(job)


async def download_output_data(file_id: str) -> List[Dict[str, Any]]:
    """Get job output data from storage.

    Args:
        file_id: File identifier

    Returns:
        List of result dictionaries
    """
    assert p_storage is not None
    return await p_storage.read_job_output_data(file_id)


async def remove_job_data(file_id: str) -> None:
    """Remove file data."""
    assert p_storage is not None
    await p_storage.delete_job_data(file_id)
