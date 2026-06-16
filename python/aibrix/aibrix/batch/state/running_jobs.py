# Copyright 2026 The Aibrix Team.
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
"""RunningJobs: the role interface a ``JobDriver`` uses to drive one job.

This is the *driver's* view of the job manager — pull the next request, report
completions, and mark the job's terminal outcome, all by ``job_id``. The driver
depends only on this narrow interface (not the concrete BatchManager), which
breaks the driver<->manager import cycle.

Relationship to ``JobProgressTracker``: the tracker is the per-job *state* (the
launch/complete bitmap for one job). ``RunningJobs`` is the *operations*
interface the driver calls; the manager answers the per-request calls below from
each job's tracker. A driver never touches a tracker directly.
"""

from typing import List, Optional, Protocol, Tuple, runtime_checkable

from aibrix.batch.job_entity import BatchJob, BatchJobError


@runtime_checkable
class RunningJobs(Protocol):
    """Driver-facing job-progress operations (by job_id)."""

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        """Get job by ID, or None if not found."""
        ...

    async def get_job_next_request(self, job_id: str) -> Tuple[BatchJob, int]:
        """Get the next request ID to execute.

        Returns:
            (BatchJob, next_request_id) or (BatchJob, -1) if the job is done.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        ...

    async def complete_job_request(
        self, job_id: str, req_id: int, failed: bool = False
    ) -> BatchJob:
        """Mark one request completed.

        Used by concurrent single-worker execution, where completions can arrive
        out of order and no next-request cursor should be returned.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        ...

    async def mark_job_progress_and_get_next_request(
        self, job_id: str, req_id: int
    ) -> Tuple[BatchJob, int]:
        """Mark a request completed and return the next request ID.

        Returns:
            (BatchJob, next_request_id) or (BatchJob, -1) if the job is done.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        ...

    async def mark_jobs_progresses(
        self, job_id: str, executed_requests: List[int]
    ) -> BatchJob:
        """Mark multiple requests completed.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        ...

    async def mark_job_total(self, job_id: str, total_requests: int) -> BatchJob:
        """Mark the total number of requests for a job.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        ...

    async def mark_job_done(self, job_id: str) -> BatchJob:
        """Mark job completed.

        Raises:
            JobUnexpectedStateError: If job is not in finalizing state.
        """
        ...

    async def mark_job_failed(self, job_id: str, ex: BatchJobError) -> BatchJob:
        """Mark job failed.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        ...
