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
import json
import uuid
from typing import Any, AsyncIterator, Dict, List, Optional, Tuple

from aibrix.batch.job_entity import BatchJob
from aibrix.batch.storage.batch_metastore import (
    delete_metadata,
    get_metadata,
    is_request_done,
    list_metastore_keys,
    lock_request,
    unlock_request,
)
from aibrix.logger import init_logger
from aibrix.storage.base import BaseStorage

logger = init_logger(__name__)


class BatchStorageAdapter:
    """Adapter that bridges the new async storage interface with the batch storage interface.

    This adapter provides batch-specific operations while using the new
    aibrix.storage module underneath. It handles the sync/async conversion and maps
    batch-specific operations to the generic object storage interface.
    """

    def __init__(self, storage: BaseStorage):
        self.storage = storage

    async def write_job_input_data(
        self, file_id: str, input_data_file_name: str
    ) -> None:
        """Write job input data from a file to storage.

        Reads the JSONL file and stores the entire file content
        """
        # Read the entire JSONL file
        with open(input_data_file_name, "r") as file:
            file_content = file.read()

        # Store the whole file content
        await self.storage.put_object(file_id, file_content, "application/jsonl")

        logger.info(f"Stored complete input file for {file_id}")

    async def read_job_input_info(self, job: BatchJob) -> Tuple[int, bool]:
        """Read job input info from storage.

        Args:
            job: BatchJob

        Returns:
            Tuple of total line number and input existence
        """
        try:
            await self.storage.head_object(job.spec.input_file_id)
            return 0, True
        except Exception:
            return 0, False

    async def read_job_next_input_data(
        self, job: BatchJob, start_index: int
    ) -> AsyncIterator[Dict[str, Any]]:
        """Read job input data line by line with request locking.

        Args:
            job: BatchJob
            start_index: Starting line index

        Returns:
            AsyncIterator of lines that were successfully locked for processing
        """
        idx = start_index
        # Use 'async for' to iterate and 'yield' each item.
        async for line in self.storage.readline_iter(
            job.spec.input_file_id, start_index
        ):
            line = line.strip()
            if len(line) == 0:
                continue

            # Try to lock this request before processing
            lock_key = self._get_request_meta_output_key(job, idx)
            try:
                # Try to acquire lock with 1 hour expiration
                locked = await lock_request(lock_key, expiration_seconds=3600)
            except Exception as e:
                # Lock operation failed (should not happen with return False requirement)
                logger.warning(
                    "Error on locking request in the job, assuming locking not supported",
                    job_id=job.job_id,
                    line_no=idx,
                    error=e,
                )  # type:ignore[call-arg]
                locked = True

            if locked:
                # Successfully locked, yield the request data
                request_data = json.loads(line)
                request_data["_request_index"] = idx  # Add index for tracking
                logger.debug(
                    "Locked and will processing request in the job",
                    job_id=job.job_id,
                    line_no=idx,
                    requset=request_data,
                )  # type:ignore[call-arg]
                yield request_data
            else:
                # Request already locked by another worker, skip it
                logger.debug(
                    "Skipping already locked request in the job",
                    job_id=job.job_id,
                    line_no=idx,
                )  # type:ignore[call-arg]

            idx += 1

    async def is_request_done(self, job: BatchJob, request_index: int) -> bool:
        """Check if a request is done.

        Args:
            job: BatchJob
            request_index: Index of the request being processed

        Returns:
            True if the request is done, False otherwise
        """
        lock_key = self._get_request_meta_output_key(job, request_index)
        return await is_request_done(lock_key)

    async def prepare_job_ouput_files(self, job: BatchJob) -> BatchJob:
        """Prepare job output and error files for batch processing.

        This method creates two separate multipart uploads:
        1. **Output file**: Stores successful request responses
        2. **Error file**: Stores failed request responses

        Both files are ALWAYS created upfront, regardless of whether errors occur.
        Workers write to the appropriate file based on request success/failure.

        File ID generation:
        - output_file_id: UUID v3 derived from job_id + "output"
        - error_file_id: UUID v3 derived from job_id + "error"
        - temp_output_file_id: Multipart upload ID for output file
        - temp_error_file_id: Multipart upload ID for error file

        Args:
            job: BatchJob object to prepare files for

        Returns:
            Updated BatchJob with file IDs populated

        Note:
            Error files will exist even if no requests fail. The file will simply
            be empty or contain no parts if all requests succeed.
        """
        if job.status.temp_output_file_id or job.status.temp_error_file_id:
            return job

        job_uuid = uuid.UUID(job.job_id)
        job.status.output_file_id, job.status.error_file_id = (
            str(uuid.uuid3(job_uuid, "output")),
            str(uuid.uuid3(job_uuid, "error")),
        )
        tasks = [
            self.storage.create_multipart_upload(
                job.status.output_file_id, "application/jsonl", small_parts=True
            ),
            self.storage.create_multipart_upload(
                job.status.error_file_id, "application/jsonl", small_parts=True
            ),
        ]
        (
            job.status.temp_output_file_id,
            job.status.temp_error_file_id,
        ) = await asyncio.gather(*tasks)

        logger.info(
            "Prepared batch job output files",
            job_id=job.job_id,
            output_file_id=job.status.output_file_id,
            error_file_id=job.status.error_file_id,
        )  # type: ignore[call-arg]

        return job

    async def write_job_output_data(
        self, job: BatchJob, request_index: int, output_data: Dict[str, Any]
    ) -> None:
        """Write job result to storage and unlock the request.

        Args:
            job: BatchJob object
            request_index: Index of the request being processed
            output_data: Single result dictionary
        """
        assert (
            job.status.output_file_id
            and job.status.error_file_id
            and job.status.temp_output_file_id
            and job.status.temp_error_file_id
        )

        json_str = json.dumps(output_data) + "\n"
        is_error = "error" in output_data and output_data["error"] is not None
        etag = await self.storage.upload_part(
            job.status.error_file_id if is_error else job.status.output_file_id,
            job.status.temp_error_file_id
            if is_error
            else job.status.temp_output_file_id,
            request_index,
            json_str,
        )

        # Unlock the request by setting completion status
        unlock_key = self._get_request_meta_output_key(job, request_index)
        completion_status = self._get_request_meta_output_val(is_error, etag)
        await unlock_request(unlock_key, completion_status)

        logger.debug(
            f"Stored result for job {job.job_id} request {request_index}, status: {completion_status}"
        )

    async def finalize_job_output_data(self, job: BatchJob) -> None:
        if (
            job.status.output_file_id is None
            or job.status.error_file_id is None
            or job.status.temp_output_file_id is None
            or job.status.temp_error_file_id is None
        ):
            # Do nothing
            return

        # 1. List all keys from metastore with the job prefix
        prefix = self._get_request_meta_output_key(job, None)
        all_keys = await list_metastore_keys(prefix)

        logger.debug(
            "Metastore keys found during job finalizing",
            job_id=job.job_id,
            prefix=prefix,
            keys=all_keys,
        )  # type: ignore[call-arg]

        # 2. Extract indices from keys and determine maximum index for total count
        indices = []
        for key in all_keys:
            # Extract index from key format: batch:{job_id}:done/{idx}
            try:
                idx_str = key[len(prefix) :]  # Get the index part
                idx = int(idx_str)
                indices.append(idx)
            except ValueError:
                logger.warning(
                    "Invalid key format found in metastore",
                    key=key,
                    job_id=job.job_id,
                )  # type: ignore[call-arg]
                continue

        # Sort indices to ensure proper ordering
        indices.sort()

        # 3. Calculate actual counts based on metastore keys
        launched = len(indices)
        total = indices[-1] + 1 if launched > 0 else 0

        logger.info(
            "Finalizing job output data using metastore keys",
            job_id=job.job_id,
            launched=launched,
            total=total,
        )  # type: ignore[call-arg]

        # Fetch metadata for all found keys
        keys = [self._get_request_meta_output_key(job, idx) for idx in indices]
        etag_results = await asyncio.gather(*[get_metadata(key) for key in keys])

        # Process results and categorize into outputs and errors
        output: List[Dict[str, str | int]] = []
        error: List[Dict[str, str | int]] = []
        completed = 0
        failed = 0
        valid_keys = []

        for idx, key, etag_result in zip(indices, keys, etag_results):
            meta_val, exist = etag_result
            if not exist:
                continue

            etag, is_error = self._parse_request_meta_output_val(meta_val)
            if etag == "":
                continue

            valid_keys.append(key)
            val: Dict[str, str | int] = {"etag": etag, "part_number": idx}

            if is_error:
                error.append(val)
                failed += 1
            else:
                output.append(val)
                completed += 1

        # 4. Update job object with calculated request counts if they differ
        if (
            job.status.request_counts.total != total
            or job.status.request_counts.launched != launched
            or job.status.request_counts.completed != completed
            or job.status.request_counts.failed != failed
        ):
            logger.info(
                "Updating job request counts based on metastore data",
                job_id=job.job_id,
                old_total=job.status.request_counts.total,
                new_total=total,
                old_launched=job.status.request_counts.launched,
                new_launched=launched,
                old_completed=job.status.request_counts.completed,
                new_completed=completed,
                old_failed=job.status.request_counts.failed,
                new_failed=failed,
            )  # type: ignore[call-arg]

            job.status.request_counts.total = total
            job.status.request_counts.launched = launched
            job.status.request_counts.completed = completed
            job.status.request_counts.failed = failed

        # Aggregate results
        await asyncio.gather(
            self.storage.complete_multipart_upload(
                job.status.output_file_id,
                job.status.temp_output_file_id,
                output,
            ),
            self.storage.complete_multipart_upload(
                job.status.error_file_id,
                job.status.temp_error_file_id,
                error,
            ),
        )

        # Delete metadata for valid keys only
        if valid_keys:
            await asyncio.gather(*[delete_metadata(key) for key in valid_keys])

    async def read_job_output_data(self, file_id: str) -> List[Dict[str, Any]]:
        """Read job results output from storage.

        Args:
            file_id: File identifier

        Returns:
            List of result dictionaries
        """
        request_results = []

        async for line in self.storage.readline_iter(file_id):
            json_obj = json.loads(line)
            request_results.append(json_obj)

        logger.info(f"Loaded complete output file for {file_id}")

        return request_results

    async def delete_job_data(self, file_id: str) -> None:
        """Delete all data for the given job ID (both input and output)."""
        # List all objects with the job prefix
        try:
            await self.storage.delete_object(file_id)

            logger.info(f"Deleted file {file_id}")
        except Exception as e:
            logger.error(f"Failed to delete data for job {file_id}: {e}")

    def _get_request_meta_output_key(self, job: BatchJob, idx: Optional[int]) -> str:
        prefix = f"batch:{job.job_id}:done/"
        if idx is None:
            return prefix
        return f"{prefix}{idx}"

    def _get_request_meta_output_val(self, is_error: bool, etag: str) -> str:
        return f"{'error' if is_error else 'output'}:{etag}"

    def _parse_request_meta_output_val(self, meta_val: str) -> Tuple[str, bool]:
        """valid output can be:
        1. output:[etag]
        2. error:[etag]
        3. processing
        """
        status = meta_val.split(":", 1)
        if len(status) == 2:
            is_error, etag = status
            return etag, is_error == "error"
        else:
            return "", False
