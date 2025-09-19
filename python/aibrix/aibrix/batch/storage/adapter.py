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
from typing import Any, AsyncIterator, Dict, List, Tuple

from aibrix.batch.job_entity import BatchJob
from aibrix.batch.storage.batch_metastore import (
    delete_metadata,
    get_metadata,
    set_metadata,
)
from aibrix.metadata.logger import init_logger
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
        """Read job input data line by line.

        Args:
            job: BatchJob

        Returns:
            AsyncIterator of lines
        """
        # Use 'async for' to iterate and 'yield' each item.
        async for line in self.storage.readline_iter(
            job.spec.input_file_id, start_index
        ):
            line = line.strip()
            if len(line) == 0:
                continue
            yield json.loads(line)

    async def prepare_job_ouput_files(self, job: BatchJob) -> None:
        """Get job output file id.

        Args:
            job_id: Job identifier

        Returns:
            Job output file id
        """
        if job.status.temp_output_file_id or job.status.temp_error_file_id:
            return

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

    async def write_job_output_data(
        self, job: BatchJob, start_index: int, output_list: List[Dict[str, Any]]
    ) -> None:
        """Write job results to storage.

        Args:
            job_id: Job identifier
            start_index: Starting index for the results
            output_list: List of result dictionaries
        """
        assert (
            job.status.output_file_id
            and job.status.error_file_id
            and job.status.temp_output_file_id
            and job.status.temp_error_file_id
        )
        for i, result_data in enumerate(output_list):
            idx = start_index + i
            json_str = json.dumps(result_data) + "\n"
            is_error = "error" in result_data
            etag = await self.storage.upload_part(
                job.status.error_file_id if is_error else job.status.output_file_id,
                job.status.temp_error_file_id
                if is_error
                else job.status.temp_output_file_id,
                idx,
                json_str,
            )
            # Store metadata
            await set_metadata(
                self._get_request_meta_output_key(job, idx),
                self._get_request_meta_output_val(is_error, etag),
            )

        logger.debug(
            f"Stored {len(output_list)} results for job {job.job_id} starting at index {start_index}"
        )

    async def finalize_job_output_data(self, job: BatchJob) -> None:
        assert (
            job.status.output_file_id
            and job.status.error_file_id
            and job.status.temp_output_file_id
            and job.status.temp_error_file_id
        )

        output: List[Dict[str, str | int]] = []
        error: List[Dict[str, str | int]] = []
        keys = []
        for i in range(job.status.request_counts.launched):
            keys.append(self._get_request_meta_output_key(job, i))

        etag_results = await asyncio.gather(*[get_metadata(key) for key in keys])
        exists = 0
        for i, etag_result in enumerate(etag_results):
            meta_val, exist = etag_result
            if not exist:
                continue

            # Compact keys
            keys[exists] = keys[i]
            exists += 1

            etag, is_error = self._parse_request_meta_output_val(meta_val)
            val: Dict[str, str | int] = {"etag": etag, "part_number": i}
            if is_error:
                error.append(val)
            else:
                output.append(val)
        keys = keys[:exists]

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

        # Delete metadata
        await asyncio.gather(*[delete_metadata(key) for key in keys])

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

    def _get_request_meta_output_key(self, job: BatchJob, idx: int) -> str:
        return f"batch:{job.job_id}:output:{idx}"

    def _get_request_meta_output_val(self, is_error: bool, etag: str) -> str:
        return f"{'error' if is_error else 'output'}:{etag}"

    def _parse_request_meta_output_val(self, meta_val: str) -> Tuple[str, bool]:
        is_error, etag = meta_val.split(":", 1)
        return etag, is_error == "error"
