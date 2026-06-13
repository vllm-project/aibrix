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
"""JobMetaInfo: a BatchJob augmented with runtime-only execution state.

Bundles a ``BatchJob`` with a per-request ``JobProgressTracker`` (the
launch/complete bitmap) and an asyncio lock. It is the active-job state object
shared by the orchestrator (``BatchManager`` holds one per in-progress job in
its registry) and the single-job worker path — neither reimplements the
progress bitmap; both delegate to the tracker here.
"""

from typing import Optional

from aibrix.batch.job_driver.driver import JobDriver
from aibrix.batch.job_entity import BatchJob
from aibrix.batch.state.job_progress_tracker import JobProgressTracker


class JobMetaInfo(BatchJob):
    """A BatchJob augmented with runtime-only state for an active job: an
    asyncio lock and a per-request JobProgressTracker. Progress operations
    delegate to the tracker (which mutates this job's status in place).
    """

    def __init__(self, job: BatchJob):
        """
        This constructs a full set of metadata for a batch job.
        Later if needed, this can include other extral metadata
        as an easy extention.
        """
        # Initialize the parent BatchJob with the same data
        super().__init__(
            sessionID=job.session_id,
            typeMeta=job.type_meta,
            metadata=job.metadata,
            spec=job.spec,
            status=job.status,
        )
        self._job_driver: Optional[JobDriver] = None
        # Per-request progress lives in a standalone JobProgressTracker (which
        # does NOT inherit BatchJob); JobMetaInfo just delegates to it. The
        # tracker mutates this job's status, so behavior is unchanged.
        self._tracker = JobProgressTracker(self)

    @property
    def batch_job(self) -> BatchJob:
        return BatchJob(
            typeMeta=self.type_meta,
            metadata=self.metadata,
            spec=self.spec,
            status=self.status,
        )

    @property
    def job_driver(self) -> Optional[JobDriver]:
        return self._job_driver

    def set_request_executed(self, req_id):
        return self._tracker.set_request_executed(req_id)

    def get_request_bit(self, req_id):
        return self._tracker.get_request_bit(req_id)

    def complete_one_request(self, req_id, failed: bool = False):
        return self._tracker.complete_one_request(req_id, failed)

    def next_request_id(self) -> int:
        return self._tracker.next_request_id()
