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
"""SchedulableJobs: the role interface a ``BatchScheduler`` uses.

This is the *scheduler's* view of the job manager: read a job/status and admit
it (``admit`` promotes a pending job to in-progress and builds its driver)
before running it. The scheduler depends only on this narrow interface, not on
the execution drivers or the per-request progress surface.
"""

from typing import TYPE_CHECKING, List, Optional, Protocol, runtime_checkable

from aibrix.batch.job_entity import BatchJob, BatchJobStatus

if TYPE_CHECKING:
    from aibrix.batch.job_driver import JobDriver


@runtime_checkable
class SchedulableJobs(Protocol):
    """Scheduler-facing job admission/read operations."""

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        """Get job by ID, or None if not found."""
        ...

    async def get_job_status(self, job_id: str) -> Optional[BatchJobStatus]:
        """Get the current status of a job, or None if not found."""
        ...

    async def admit(self, job_id: str) -> Optional["JobDriver"]:
        """Admit a job: validate it, promote pending -> in-progress, and build
        its driver. Returns the driver to run, or None if not admitted. Called
        by the scheduler; not a user-facing choice."""
        ...

    async def expire_job(self, job_id: str) -> bool:
        """Expire a past-due pending job: finalize it (condition: expired) and
        move it to done. Called by the scheduler's cleanup loop so the registry
        — not a scheduler-side set — owns expired state. Returns whether the job
        was expired."""
        ...

    async def list_pending(self) -> List[BatchJob]:
        """Snapshot of pending (awaiting-scheduling) jobs. Lets the scheduler
        derive past-due jobs from the registry's pending pool instead of keeping
        a duplicated due-time list."""
        ...
