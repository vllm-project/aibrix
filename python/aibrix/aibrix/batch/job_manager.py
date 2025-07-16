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
import copy
import uuid
from datetime import datetime, timezone
from typing import Optional

import aibrix.batch.storage as _storage
from aibrix.batch.scheduler import JobScheduler
from aibrix.metadata.logger import init_logger

from .job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    JobEntityManager,
    ObjectMeta,
    RequestCountStats,
    TypeMeta,
)

# Initialize logger
logger = init_logger(__name__)


class JobMetaInfo(BatchJob):
    """Legacy code, will be removed in the future."""

    def __init__(self, job: BatchJob):
        """
        This constructs a full set of metadata for a batch job.
        Later if needed, this can include other extral metadata
        as an easy extention.
        """
        # Initialize the parent BatchJob with the same data
        super().__init__(
            typeMeta=job.type_meta,
            metadata=job.metadata,
            spec=job.spec,
            status=job.status,
        )
        self._async_lock = asyncio.Lock()
        self._current_request_id = 0
        # Initialize progress bits based on total request count
        total_requests: int = (
            job.status.request_counts.total
            if job.status and job.status.request_counts
            else 0
        )
        self._request_progress_bits: list[bool] = [False] * total_requests

    def set_request_executed(self, req_id):
        # This marks the request successfully executed.
        self._request_progress_bits[req_id] = True

    def get_request_bit(self, req_id):
        return self._request_progress_bits[req_id]

    def get_job_status(self) -> BatchJobState:
        return self.status.state

    def complete_one_request(self, req_id):
        """
        This is called after an inference call. If all requests
        are done, we need to update its status to be completed.
        """
        if not self._request_progress_bits[req_id]:
            self.set_request_executed(req_id)
            self.status.request_counts.completed += 1
            if self.status.request_counts.completed == self.status.request_counts.total:
                self.status.finalizing_at = datetime.now(timezone.utc)
                self.status.completed_at = datetime.now(timezone.utc)
                self.status.state = BatchJobState.COMPLETED

    def next_request_id(self):
        """
        Returns the next request for inference. Due to the propobility
        that some requests are failed, this returns a request that
        are not marked as executed.
        """
        if self.status.request_counts.completed == self.status.request_counts.total:
            return -1

        req_id = self._current_request_id
        while self._request_progress_bits[req_id]:
            req_id += 1
            if req_id == self.status.request_counts.total:
                req_id = 0

        return req_id

    def validate_job(self):
        """
        This handles all validations before successfully creating a job.
        This is also the place connecting other components
        to check connection and status.
        """
        # 1. this makes sure the job id is consistent with storage side
        input_num = _storage.get_job_num_request(self.spec.input_file_id)
        self.status.request_counts.total = input_num
        self._request_progress_bits = [False] * input_num
        if input_num == 0:
            logger.warning("Storage side does not have valid request to process")
            raise BatchJobError(
                code=BatchJobErrorCode.INVALID_INPUT_FILE,
                message="Storage side does not have valid request to process",
                param="input_file_id",
            )

        # 2. Authenticate job and rate limit
        if not self.job_authentication():
            raise BatchJobError(
                code=BatchJobErrorCode.AUTHENTICATION_ERROR,
                message="authentication error",
            )

    def job_authentication(self):
        # [TODO] xin
        # Check if the job and account is permitted and rate limit.
        return True


class JobManager:
    def __init__(self, job_entity_manager: Optional[JobEntityManager] = None):
        """
        This manages jobs in three categorical job pools.
        1. _pending_jobs are jobs that are not scheduled yet
        2. _in_progress_jobs are jobs that are in progress now.
        Theses are the input to the job scheduler.
        3. _done_jobs are inactive jobs. This needs to be updated periodically.
        """
        self._pending_jobs: dict[str, BatchJob] = {}
        self._in_progress_jobs: dict[str, BatchJob] = {}
        self._done_jobs: dict[str, BatchJob] = {}
        self._job_scheduler: Optional[JobScheduler] = None
        self._job_entity_manager: Optional[JobEntityManager] = job_entity_manager

        # Register job lifecycle handlers if entity manager is available
        if self._job_entity_manager:
            self._job_entity_manager.on_job_committed(self.job_committed_handler)
            self._job_entity_manager.on_job_updated(self.job_updated_handler)
            self._job_entity_manager.on_job_deleted(self.job_deleted_handler)

    def set_scheduler(self, scheduler: JobScheduler):
        self._job_scheduler = scheduler

    def create_job(
        self,
        session_id: str,
        input_file_id: str,
        api_endpoint: str,
        completion_window: str,
        meta_data: dict,
    ) -> str:
        """
        This interface is exposed to users to submit a new job and create
        a job accordingly.
        Before calling this, user needs to submit job input to storage first
        to have job ID ready.
        This will validate a job with multiple checking steps.
        """
        job_spec = BatchJobSpec.from_strings(
            input_file_id, api_endpoint, completion_window, meta_data
        )
        if self._job_entity_manager:
            self._job_entity_manager.submit_job(
                job_spec
            )  # Will trigger job committed handler
            # Note: When using job_entity_manager, the job_id will be available after the committed handler
            # For now, we return None since we don't have immediate access to the generated job_id
            #
            # [TODO]: Add a self._creating_jobs to track jobs being created. details:
            # 1. Using session_id as key, and stored meta_data for tracking.
            # 2. Support asyncio to wait for job_id to be available.
            # 3. use uvloop if available.
            return None
        else:
            job = BatchJob(
                typeMeta=TypeMeta(apiVersion="", kind="LocalBatchJob"),
                metadata=ObjectMeta(
                    resourceVersion=None,
                    creationTimestamp=datetime.now(timezone.utc),
                    deletionTimestamp=None,
                ),
                spec=job_spec,
                status=BatchJobStatus(
                    jobID=str(uuid.uuid4()),
                    state=BatchJobState.CREATED,
                    createdAt=datetime.now(timezone.utc),
                    requestCounts=RequestCountStats(total=0, completed=0, failed=0),
                ),
            )
            self.job_committed_handler(job)
            assert job.job_id is not None

            return job.job_id

    def cancel_job(self, job_id: str) -> bool:
        """
        Cancel a job by job_id.

        This method supports both local job cancelling and job cancelling with _job_entity_manager.
        For jobs managed by _job_entity_manager, it signals the entity manager to cancel the job.
        For local jobs, it directly calls job_deleted_handler.

        The method considers the situation that while before signaling, the job is in pending or processing,
        but before job_deleted_handler is called, the job may have completed.

        Args:
            job_id: The ID of the job to cancel

        Returns:
            bool: True if cancellation was initiated successfully, False otherwise
        """
        # Check if job exists in any state
        job = None
        job_in_progress = False
        if job_id in self._pending_jobs:
            job = self._pending_jobs[job_id]
            # Delete from pending jobs to avoid the job being scheduled. Status will be updated later
            del self._pending_jobs[job_id]
        elif job_id in self._in_progress_jobs:
            job = self._in_progress_jobs[job_id]
            job_in_progress = True
        elif job_id in self._done_jobs:
            # Job is already done (completed, failed, expired, or cancelled)
            logger.debug("Job is already in final state", job_id=job_id)  # type: ignore[call-arg]
            return True
        else:
            logger.warning("Job not found", job_id=job_id)  # type: ignore[call-arg]
            return False

        # Check if job is already in a final state (race condition protection)
        # We allow CANCELLING job be signalled again.
        if job.status and job.status.state in [
            BatchJobState.COMPLETED,
            BatchJobState.FAILED,
            BatchJobState.EXPIRED,
            BatchJobState.CANCELED,
        ]:
            logger.info(  # type: ignore[call-arg]
                "Job is already in final state", job_id=job_id, state=job.status.state
            )
            return False

        if self._job_entity_manager:
            # Signal the entity manager to cancel the job
            # The actual state update will be handled by job_deleted_handler or job_updated_handler when called back
            self._job_entity_manager.cancel_job(job_id)
            return True

        # For local jobs, directly call job_deleted_handler
        job_done = copy.deepcopy(job)
        if job_done.status:
            job_done.status.state = BatchJobState.CANCELLING
            job_done.status.cancelling_at = datetime.now()
        if job_in_progress:
            # [TODO][NOW] zhangjyr
            # Remove all related requests from scheduler and proxy
            del self._in_progress_jobs[job_id]

        if job_done.status:
            job_done.status.state = BatchJobState.CANCELED
            job_done.status.cancelled_at = datetime.now()
        self.job_updated_handler(job, job_done)
        return True

    def job_committed_handler(self, job: BatchJob):
        """
        This is called by job entity manager when a job is committed.
        """
        job_id = job.job_id
        if not job_id:
            logger.error("Job ID not found in comitted job")
            return

        category = self._categorize_jobs(job)
        category[job_id] = job
        if category is self._pending_jobs and self._job_scheduler:
            created_at: datetime = job.status.created_at
            self._job_scheduler.append_job(
                job_id, created_at.timestamp() + job.spec.completion_window.expires_at()
            )

    def job_updated_handler(self, old_job: BatchJob, new_job: BatchJob):
        """
        This is called by job entity manager when a job status is updated.
        Handles state transitions when a job is cancelled or completed.
        """
        job_id = old_job.job_id
        if not job_id:
            logger.error("Job ID not found in updated job")
            return
        if not old_job.status or not new_job.status:
            logger.error("Job status not found in updated job", job_id=job_id)  # type: ignore[call-arg]
            return

        # Categorize jobs
        old_category = self._categorize_jobs(old_job)
        new_category = self._categorize_jobs(new_job)
        if old_category is new_category:
            return

        # Move job from old category to new category
        if job_id in old_category:
            del old_category[job_id]
        new_category[job_id] = new_job

    def job_deleted_handler(self, job: BatchJob):
        """
        This is called by job entity manager when a job is deleted.
        """
        job_id = job.job_id
        if job_id in self._in_progress_jobs:
            # [TODO][NEXT] zhangjyr
            # Remove all related requests from scheduler and proxy, and call job_updated_handler, followed by job_deleted_handler() again.
            logger.warning("Job is in progress, cannot be deleted", job_id=job_id)  # type: ignore[call-arg]
            return

        if job_id in self._pending_jobs:
            del self._pending_jobs[job_id]
        if job_id in self._done_jobs:
            del self._done_jobs[job_id]

    def get_job(self, job_id) -> Optional[BatchJob]:
        """
        This retrieves a job's status to users.
        Job scheduler does not need to check job status. It can directly
        check the job pool for scheduling, such as pending_jobs.
        """
        if self._job_entity_manager:
            return self._job_entity_manager.get_job(job_id)

        if job_id in self._pending_jobs:
            return self._pending_jobs[job_id]
        elif job_id in self._in_progress_jobs:
            return self._in_progress_jobs[job_id]
        elif job_id in self._done_jobs:
            return self._done_jobs[job_id]

        return None

    def get_job_status(self, job_id: str) -> Optional[BatchJobState]:
        """Get the current status of a job."""
        job = self.get_job(job_id)
        return job.status.state if job and job.status else None

    def start_execute_job(self, job_id) -> bool:
        """
        This interface should be called by scheduler.
        User is not allowed to choose a job to be scheduled.
        """
        if job_id not in self._pending_jobs:
            logger.warning("Job does not exist - maybe create it first", job_id=job_id)  # type: ignore[call-arg]
            return False
        if job_id in self._in_progress_jobs:
            logger.info("Job has already been launched", job_id=job_id)  # type: ignore[call-arg]
            return False

        job = self._pending_jobs[job_id]
        del self._pending_jobs[job_id]
        meta_data = JobMetaInfo(job)
        meta_data.status.state = BatchJobState.VALIDATING
        self._in_progress_jobs[job_id] = meta_data

        try:
            meta_data.validate_job()
            meta_data.status.in_progress_at = datetime.now(timezone.utc)
            # [TODO][NEXT] Use separate file id
            meta_data.status.output_file_id = meta_data.job_id
            meta_data.status.state = BatchJobState.IN_PROGRESS
        except BatchJobError as e:
            logger.error("Job validation failed", job_id=job_id, error=str(e))  # type: ignore[call-arg]
            meta_data.status.state = BatchJobState.FAILED
            meta_data.status.failed_at = datetime.now(timezone.utc)
            meta_data.status.errors = [e]
            del self._in_progress_jobs[job_id]
            self._done_jobs[job_id] = meta_data
            return False

        return True

    def get_job_next_request(self, job_id):
        request_id = -1
        if job_id not in self._in_progress_jobs:
            logger.info("Job has not been scheduled yet", job_id=job_id)  # type: ignore[call-arg]
            return request_id

        meta_data: JobMetaInfo = self._in_progress_jobs[job_id]
        return meta_data.next_request_id()

    def get_job_endpoint(self, job_id) -> str:
        if job_id in self._pending_jobs:
            job = self._pending_jobs[job_id]
        elif job_id in self._in_progress_jobs:
            job = self._in_progress_jobs[job_id]
        else:
            logger.info("Job is discarded", job_id=job_id)  # type: ignore[call-arg]
            return ""
        return str(job.spec.endpoint)

    def mark_job_progress(self, job_id, executed_requests):
        """
        This is used to sync job's progress, called by execution proxy.
        It is guaranteed that each request is executed at least once.
        """
        if job_id not in self._in_progress_jobs:
            logger.info("Job has not started yet", job_id=job_id)  # type: ignore[call-arg]
            return False

        meta_data: JobMetaInfo = self._in_progress_jobs[job_id]
        request_len = meta_data.status.request_counts.total
        invalid_flag = False

        for req_id in executed_requests:
            if req_id < 0 or req_id >= request_len:
                logger.error(  # type: ignore[call-arg]
                    "Mark job progress failed - request index out of boundary",
                    job_id=job_id,
                )
                invalid_flag = True
                continue
            meta_data.complete_one_request(req_id)

        state = meta_data.get_job_status()
        if state == BatchJobState.COMPLETED:
            # Mark the job to be completed if all requests are finished.
            del self._in_progress_jobs[job_id]
            self._done_jobs[job_id] = meta_data
            logger.info("Job is completed", job_id=job_id)  # type: ignore[call-arg]
        else:
            self._in_progress_jobs[job_id] = meta_data

        if invalid_flag:
            return False
        return True

    def expire_job(self, job_id):
        """
        This is called by scheduler. When a job arrives at its
        specified due time, scheduler will mark this expired.
        User can not expire a job, but can cancel a job.
        """

        if job_id in self._pending_jobs:
            job = self._pending_jobs[job_id]
            del self._pending_jobs[job_id]
            job.status.state = BatchJobState.EXPIRED
            self._done_jobs[job_id] = job
        elif job_id in self._in_progress_jobs:
            # Now a job can not be expired once it gets scheduled, considering
            # that expiring a partial executed job wastes resources.
            # Later we may apply another policy to force a job to expire
            # regardless of its current progress.
            logger.warning("Job was scheduled and cannot expire", job_id=job_id)  # type: ignore[call-arg]
            return False

        elif job_id in self._done_jobs:
            logger.error("Job is done and this should not happen", job_id=job_id)  # type: ignore[call-arg]
            return False

        return True

    def sync_job_to_storage(self, jobId):
        """
        [TODO] Xin
        This is used to serialize everything here to storage to make sure
        that job manager can restart it over from storage once it crashes
        or intentional quit.
        """
        pass

    def _categorize_jobs(self, job: BatchJob) -> dict[str, BatchJob]:
        """
        This is used to categorize jobs into pending, in progress, and done.
        """
        if not job.status:
            return self._pending_jobs
        if job.status.state == BatchJobState.CREATED:
            return self._pending_jobs
        elif job.status.state in [
            BatchJobState.COMPLETED,
            BatchJobState.FAILED,
            BatchJobState.EXPIRED,
            BatchJobState.CANCELED,
        ]:
            return self._done_jobs
        else:
            return self._in_progress_jobs
