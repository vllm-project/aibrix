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

from typing import List, Optional, Protocol, Tuple

from .job_entity import BatchJob, BatchJobError, BatchJobStatus


class JobProgressManager(Protocol):
    """Protocol for managing job progress and status tracking.

    This protocol defines the interface that JobDriver uses to interact with
    job management functionality, providing a clean separation of concerns
    between job execution logic and job lifecycle management.
    """

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        """Get job by ID.

        Args:
            job_id: Job identifier

        Returns:
            BatchJob if found, None otherwise
        """
        ...

    async def get_job_status(self, job_id: str) -> Optional[BatchJobStatus]:
        """Get the current status of a job."""
        ...

    async def start_execute_job(self, job_id) -> bool:
        """
        This interface should be called by scheduler.
        User is not allowed to choose a job to be scheduled.
        """
        ...

    async def mark_job_total(self, job_id: str, total_requests: int) -> BatchJob:
        """Mark the total number of requests for a job.

        Args:
            job_id: Job identifier
            total_requests: Total number of requests in the job

        Returns:
            Updated BatchJob

        Raises:
            JobUnexpectedStateError: If job is not in progress
        """
        ...

    async def mark_job_done(self, job_id: str) -> BatchJob:
        """Mark job as completed.

        Args:
            job_id: Job identifier

        Returns:
            Updated BatchJob

        Raises:
            JobUnexpectedStateError: If job is not in finalizing state
        """
        ...

    async def mark_job_failed(self, job_id: str, ex: BatchJobError) -> BatchJob:
        """Mark job as failed.

        Args:
            job_id: Job identifier
            ex: BatchJobError that cause the failure

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        ...

    async def mark_jobs_progresses(
        self, job_id: str, executed_requests: List[int]
    ) -> BatchJob:
        """Mark multiple requests as completed.

        Args:
            job_id: Job identifier
            executed_requests: List of request IDs that have been completed

        Returns:
            Updated BatchJob

        Raises:
            JobUnexpectedStateError: If job is not in progress
        """
        ...

    async def get_job_next_request(self, job_id: str) -> Tuple[BatchJob, int]:
        """Get the next request ID to execute.

        Args:
            job_id: Job identifier

        Returns:
            Tuple of (BatchJob, next_request_id) or (BatchJob, -1) if job is done

        Raises:
            JobUnexpectedStateError: If job is not in progress
        """
        ...

    async def mark_job_progress_and_get_next_request(
        self, job_id: str, req_id: int
    ) -> Tuple[BatchJob, int]:
        """Mark a request as completed and get the next request ID.

        Args:
            job_id: Job identifier
            req_id: Request ID that was completed

        Returns:
            Tuple of (BatchJob, next_request_id) or (BatchJob, -1) if job is done

        Raises:
            JobUnexpectedStateError: If job is not in progress
        """
        ...
