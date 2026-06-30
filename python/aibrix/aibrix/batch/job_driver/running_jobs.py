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

from typing import Optional, Protocol, runtime_checkable

from aibrix.batch.job_entity import BatchJob, BatchJobError, BatchJobStatus


@runtime_checkable
class RunningJobs(Protocol):
    """Driver-facing job-progress operations (by job_id)."""

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        """Get job by ID, or None if not found."""
        ...

    async def mark_job_validated(self, job_id: str, status: BatchJobStatus) -> BatchJob:
        """Persist validated job status changes for a job before execution.

        Args:
            job_id: Job identifier
            status: BatchJob with status updated

        Return:
            Updated BatchJob
        """
        ...

    async def update_job_status(self, job_id: str, status: BatchJobStatus) -> BatchJob:
        """Persist non-local job status changes without overwriting newer lifecycle transitions."""
        ...

    async def update_job_local_status(
        self, job_id: str, worker_id: str, status: BatchJobStatus
    ) -> BatchJob:
        """Persist a worker-local status snapshot for aggregation. This operation is thread-safe."""
        ...

    async def mark_job_finalizing(self, job_id: str) -> BatchJob:
        """Transition a running job into finalizing."""
        ...

    async def mark_job_done(self, job: BatchJob) -> BatchJob:
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
