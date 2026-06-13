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
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional, Tuple

from aibrix.batch.batch_scheduler import BatchScheduler
from aibrix.batch.client import EndpointSource
from aibrix.batch.job_driver import (
    JobDriver,
    create_job_driver,
)
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    Condition,
    ConditionStatus,
    ConditionType,
)
from aibrix.batch.state import (
    BatchRegistry,
    EntityManagerBridge,
    JobEntityManager,
    JobMetaInfo,
    RunningJobs,
    SchedulableJobs,
)
from aibrix.context import InfrastructureContext
from aibrix.logger import init_logger


# Custom exceptions for batch manager
class BatchManagerError(Exception):
    """Base exception for batch manager errors."""

    pass


class JobUnexpectedStateError(BatchManagerError):
    """Job in unexpcted status"""

    def __init__(self, message: str, state: Optional[BatchJobState]):
        super().__init__(message)
        self.state = state


@dataclass
class JobCreationRequest:
    """Request data for job creation."""

    session_id: str
    input_file_id: str
    api_endpoint: str
    completion_window: str
    metadata: Dict[str, Any]
    timeout: float = 30.0  # Default 30 second timeout


# Initialize logger
logger = init_logger(__name__)


def _preserve_local_timestamps(
    old_status: BatchJobStatus, new_status: BatchJobStatus
) -> None:
    """Carry forward timestamps + usage."""
    for field in ("in_progress_at", "usage"):
        if (
            getattr(new_status, field) is None
            and getattr(old_status, field) is not None
        ):
            setattr(new_status, field, getattr(old_status, field))


class BatchManager(RunningJobs, SchedulableJobs):
    # Valid state transitions are defined as:
    # 1. Started -> Validating -> In_progress -> Finalizing -> Finalzed(condition: completed)
    # 2. Started/Validating -> Finalzed (condition: failed)
    # 3. In_progress -> Finalizing -> Finalized (condition: failed)
    # 4. Started/Validating -> Cancelling -> Finalized (condition: cancelled)
    # 5. In_progress -> Cancelling -> Finalizing -> Finalized (condition: cancelled)
    # 6. Started/Validating -> Finalized (condition: expired)
    # 7. In_progress -> Finalizing -> Finalized (condition: expired)
    VALID_STATE_TRANSITIONS = {
        BatchJobState.CREATED: [
            BatchJobState.VALIDATING,
            BatchJobState.FINALIZED,  # expiry before validation (condition: expired)
        ],
        BatchJobState.VALIDATING: [
            BatchJobState.IN_PROGRESS,
            BatchJobState.FINALIZED,  # For failed/expired conditions
            BatchJobState.CANCELLING,  # For cancellation
        ],
        BatchJobState.IN_PROGRESS: [
            BatchJobState.FINALIZING,
            BatchJobState.FINALIZED,  # failure with no output to aggregate
            BatchJobState.CANCELLING,  # For cancellation
        ],
        BatchJobState.FINALIZING: [BatchJobState.FINALIZED],
        BatchJobState.CANCELLING: [
            BatchJobState.FINALIZED,
            BatchJobState.FINALIZING,  # For in_progress -> cancelling -> finalizing
        ],
        BatchJobState.FINALIZED: [],  # Terminal state
    }

    def __init__(
        self,
        context: InfrastructureContext,
        job_entity_manager: Optional[JobEntityManager] = None,
    ) -> None:
        """
        This manages jobs in three categorical job pools.
        1. _pending_jobs are jobs that are not scheduled yet
        2. _in_progress_jobs are jobs that are in progress now.
        Theses are the input to the job scheduler.
        3. _done_jobs are inactive jobs. This needs to be updated periodically.
        """
        super().__init__()

        # Service-level registry of all jobs (pending / in_progress / done).
        # BatchManager orchestrates; the registry holds the pools.
        self._registry = BatchRegistry()
        self._job_scheduler: Optional[BatchScheduler] = None
        self._job_entity_manager: Optional[JobEntityManager] = job_entity_manager
        # Adapter for the create coordination + persist-verb decision against
        # the entity-manager port (the manager keeps the simple direct calls).
        self._bridge = EntityManagerBridge()
        self._context = context
        # Injected at startup by the BatchDriver; passed to create_job_driver
        # when admitting a job (the inference backend the driver dispatches to).
        self._endpoint_source: Optional[EndpointSource] = None

        self._creation_timeouts: Dict[str, asyncio.Task] = {}
        self._session_metadata: Dict[str, Dict[str, Any]] = {}

    # The three pools live in the BatchRegistry; these proxies keep the existing
    # by-pool access (orchestration logic + tests) working unchanged while the
    # registry concern is owned by a named service-level component.
    @property
    def _pending_jobs(self) -> dict[str, BatchJob]:
        return self._registry.pending

    @property
    def _in_progress_jobs(self) -> dict[str, BatchJob]:
        return self._registry.in_progress

    @property
    def _done_jobs(self) -> dict[str, BatchJob]:
        return self._registry.done

    def set_scheduler(self, scheduler: BatchScheduler) -> None:
        self._job_scheduler = scheduler

    def set_endpoint_source(self, endpoint_source: Optional[EndpointSource]) -> None:
        """The inference backend an admitted job's driver dispatches against.
        Injected once at startup so admission need not thread it through."""
        self._endpoint_source = endpoint_source

    async def set_job_entity_manager(
        self, job_entity_manager: JobEntityManager
    ) -> None:
        self._job_entity_manager = job_entity_manager
        await self.bind_entity_manager()

    async def bind_entity_manager(self) -> None:
        """Register the entity manager's lifecycle handlers. Deferred to start()
        (not __init__) because the entity manager captures the running loop at
        handler registration — it must be the loop the manager runs on."""
        if self._job_entity_manager is None:
            return
        # The bridge attaches the port and registers the lifecycle handlers.
        self._bridge.bind(
            self._job_entity_manager,
            self.job_committed_handler,
            self.job_updated_handler,
            self.job_deleted_handler,
        )

    async def create_job(
        self,
        session_id: str,
        input_file_id: str,
        api_endpoint: str,
        completion_window: str,
        meta_data: dict,
        timeout: float = 30.0,
        initial_state: BatchJobState = BatchJobState.CREATED,
        request_count: int = 0,
    ) -> str:
        job_spec = BatchJobSpec.from_strings(
            input_file_id, api_endpoint, completion_window, meta_data
        )
        return await self.create_job_with_spec(
            session_id, job_spec, timeout, initial_state, request_count
        )

    async def create_job_with_spec(
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        timeout: float = 30.0,
        initial_state: BatchJobState = BatchJobState.CREATED,
        request_count: int = 0,
    ) -> str:
        """
        Async job creation that waits for job ID to be available.
        Before calling this, user needs to submit job input to storage first
        to have input_file_id ready.

        Note: Even create_job is timeout, the job can be successfully created.
        We do nothing to handle this case. Call list_jobs() for a full list.

        Args:
            session_id: Unique session identifier for tracking
            input_file_id: File ID for job input
            api_endpoint: API endpoint for job execution
            completion_window: Time window for job completion
            meta_data: Additional job metadata
            timeout: Timeout in seconds to wait for job ID

        Returns:
            str: Job ID when available

        Raises:
            asyncio.TimeoutError: If job ID not available within timeout
            Exception: If job submission fails
        """
        if self._job_entity_manager:
            # Submit + wait for the committed event to deliver the id. The
            # request/response coordination lives in the bridge.
            return await self._bridge.submit_and_wait(
                session_id, job_spec, request_count, timeout
            )

        # Local job handling.
        job = BatchJob.new_local(job_spec, request_count=request_count)
        job.status.state = initial_state
        await self.job_committed_handler(job)

        if job.job_id is None:
            raise RuntimeError("Job ID was not set after job committed handler")

        return job.job_id

    async def cancel_job(self, job_id: str) -> bool:
        """
        Cancel a job by job_id.

        This method supports both local job cancelling and job cancelling with _job_entity_manager.
        For jobs managed by _job_entity_manager, it signals the entity manager to cancel the job.
        For local jobs, it directly calls job_deleted_handler.

        The method considers the situation that while before signaling, the job is in pending or processing,
        but before job_deleted_handler is called, the job may have completed.

        Noted: job not will be deleted from job_manager

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
            # remove from _pending_jobs to prevent scheduling anyway.
            del self._pending_jobs[job_id]
            logger.debug("Job removed from a category", category="_pending_jobs")  # type: ignore[call-arg]
        elif job_id in self._in_progress_jobs:
            job = self._in_progress_jobs[job_id]
            job_in_progress = job.status.state == BatchJobState.IN_PROGRESS
        elif job_id in self._done_jobs:
            # Job is already done (completed, failed, expired, or cancelled)
            logger.debug("Job is already in final state", job_id=job_id)  # type: ignore[call-arg]
            return False
        else:
            logger.warning("Job not found", job_id=job_id)  # type: ignore[call-arg]
            return False

        # Check if job is finalizing
        # We allow CANCELLING job be signalled again.
        if job.status.state == BatchJobState.FINALIZING:
            logger.info(  # type: ignore[call-arg]
                "Job is finalizing", job_id=job_id, state=job.status.state
            )
            return False

        # Start cancel

        job.status.state = (
            BatchJobState.CANCELLING
        )  # update local state until being cancelled
        job.status.cancelling_at = datetime.now(timezone.utc)
        if not job_in_progress:
            self._in_progress_jobs[job_id] = job
            logger.debug(
                "Job added to a category during cancelling", category="_pending_jobs"
            )  # type: ignore[call-arg]

        job_cancelled = job.model_copy(deep=True)
        job_cancelled.status.add_condition(
            Condition(
                type=ConditionType.CANCELLED,
                status=ConditionStatus.TRUE,
                lastTransitionTime=datetime.now(timezone.utc),
            )
        )
        if job_in_progress:
            job_cancelled.status.state = BatchJobState.FINALIZING
        else:
            job_cancelled.status.state = BatchJobState.FINALIZED
            job_cancelled.status.finalized_at = job.status.cancelling_at
            # OpenAI's Batch object stamps cancelled_at when cancellation
            # completes; without this a cancelled batch reports status
            # "cancelled" but cancelled_at=null.
            job_cancelled.status.cancelled_at = job.status.cancelling_at

        if self._job_entity_manager:
            # Signal the entity manager to cancel the job
            # The actual state update will be handled by job_updated_handler when called back
            await self._job_entity_manager.cancel_job(job_cancelled)
            return True

        # For local jobs, transit directly
        if job_in_progress:
            # [TODO][NEXT] Review decision of disabling cancellation of local in progress job.
            # Local in progress job can not or need not be cancelled.
            return False

        await self.job_updated_handler(job, job_cancelled)
        return True

    async def delete_job(self, job_id: str) -> bool:
        """
        Delete a job by job_id. Only finished job can be deleted.

        Args:
            job_id: The ID of the job to cancel

        Returns:
            bool: True if deletion was initiated successfully, False otherwise
        """
        # Check if job exists in any state
        if (job := self._done_jobs.get(job_id)) is None:
            # Job is not already done (completed, failed, expired, or cancelled)
            logger.error("Job is not in final state on deleting", job_id=job_id)  # type: ignore[call-arg]
            return False

        if self._job_entity_manager:
            # Signal the entity manager to delete the job
            # The actual state update will be handled by job_deleted_handler when called back
            await self._job_entity_manager.delete_job(job)
            return True

        # For local jobs, transit directly
        return await self.job_deleted_handler(job)

    def _validate_state_transition(
        self, old_job: Optional[BatchJob], new_job: BatchJob
    ) -> bool:
        """Validate if the state transition is allowed based on the defined rules.

        Args:
            old_job: The previous job state (None for new jobs)
            new_job: The new job state

        Returns:
            True if transition is valid, False otherwise
        """
        if old_job is None:
            # New job, allow any initial state
            return True

        old_state = old_job.status.state
        new_state = new_job.status.state

        # Same state is always valid
        if old_state == new_state:
            return True

        # Check if transition is in valid transitions
        valid_next_states = self.VALID_STATE_TRANSITIONS.get(old_state, [])
        is_valid = new_state in valid_next_states

        if not is_valid:
            logger.warning(
                "Invalid state transition for job",
                job_id=new_job.status.job_id,
                old_state=old_state,
                new_state=new_state,
                valid_transitions=valid_next_states,
            )  # type: ignore[call-arg]

        return is_valid

    async def job_committed_handler(self, job: BatchJob) -> bool:
        """
        This is called by job entity manager when a job is committed.
        Enhanced to resolve pending job creation futures.
        """
        job_id = job.job_id
        if not job_id:
            logger.error("Job ID not found in comitted job")
            return False

        # Resolve a pending create future (the other half of submit_and_wait).
        # No pending future means a timed-out / already-created job: ignore it.
        if job.session_id and not self._bridge.resolve_creation(job.session_id, job_id):
            return False

        category, name = self._categorize_jobs(job, first_seen=True)
        category[job_id] = job
        logger.debug("Job added to a category", category=name)  # type: ignore[call-arg]

        if category is not self._pending_jobs:
            return True

        # Add to job scheduler if available (traditional workflow). Expiry is
        # derived from the registry's pending pool, so no due time is pushed.
        if self._job_scheduler:
            logger.info("Add job to scheduler", job_id=job_id)  # type: ignore[call-arg]
            self._job_scheduler.append_job(job_id)
        # For metadata server (no scheduler): prepare job output files when job is committed
        elif (
            job.status.output_file_id is None
            or job.status.temp_output_file_id is None
            or job.status.error_file_id is None
            or job.status.temp_error_file_id is None
        ) and self._job_entity_manager is not None:
            # Try starting job immiediately with job validation.
            if await self.admit(job_id) is None:
                return True

            # Initiate job preparing, see JobDriver for details
            logger.info("Starting job preparation for new job", job_id=job_id)  # type: ignore[call-arg]
            try:
                job_driver = create_job_driver(
                    self._context,
                    self,
                    self._job_entity_manager,
                    job,
                )
                await job_driver.execute(job_id)
                # Leave job_updated_handler to update job location in queues
            except Exception as e:
                logger.error("Job execution failed", job_id=job_id, exc_info=True)  # type: ignore[call-arg]
                await self.mark_job_failed(
                    job_id,
                    BatchJobError(
                        code=BatchJobErrorCode.PREPARE_OUTPUT_ERROR, message=str(e)
                    ),
                )
                # No need to stop job because only update_job_ready will start job.

        return True

    async def job_updated_handler(self, old_job: BatchJob, new_job: BatchJob) -> bool:
        """
        This is called by job entity manager when a job status is updated.
        Handles state transitions when a job is cancelled or completed.
        Validates state transitions according to defined rules.
        """
        try:
            job_id = old_job.job_id
            if not job_id:
                logger.error("Job ID not found in updated job")
                return False

            # Categorize jobs
            old_category, old_name = self._categorize_jobs(old_job)
            new_category, new_name = self._categorize_jobs(new_job)
            # Load cache job, possibily with local metainfo.
            old_job_in_category = old_category.get(job_id)
            if old_job_in_category is None:
                logger.warning(
                    "Job is not in old category, ignore updating",
                    old_category=old_name,
                    new_category=new_name,
                )  # type: ignore[call-arg]
                return False
            old_job = old_job_in_category

            # Validate state transition
            if not self._validate_state_transition(old_job, new_job):
                logger.warning(
                    "Invalid state transition for job - rejecting update",
                    job_id=job_id,
                )  # type: ignore[call-arg]
                return False

            logger.debug(
                "job_updated_handler passed state transition",
                old_state=old_job.status.state.value,
                new_state=new_job.status.state.value,
            )  # type: ignore[call-arg]

            # No category change, try update status
            if old_category == new_category:
                # avoid override local metainfo by update status only
                old_job.metadata = new_job.metadata  # Update resource version
                _preserve_local_timestamps(old_job.status, new_job.status)
                old_job.status = new_job.status  # Update status
                new_job = old_job
            else:
                # Move job from old category to new category
                _preserve_local_timestamps(old_job.status, new_job.status)
                del old_category[job_id]
                new_category[job_id] = new_job
                logger.debug(
                    "Job moved to a new category",
                    old_category=old_name,
                    new_category=new_name,
                )  # type: ignore[call-arg]

            return True
        except Exception:
            logger.error("exception in job_updated_handler", exc_info=True)  # type: ignore[call-arg]
            raise

    async def job_deleted_handler(self, job: BatchJob) -> bool:
        """
        This is called by job entity manager when a job is deleted.
        """
        job_id = job.job_id
        if job_id in self._in_progress_jobs:
            # [TODO][NEXT] zhangjyr
            # Remove all related requests from scheduler and proxy, and call job_updated_handler, followed by job_deleted_handler() again.
            logger.warning("Job is in progress, cannot be deleted", job_id=job_id)  # type: ignore[call-arg]
            return True

        if job_id in self._pending_jobs:
            del self._pending_jobs[job_id]
            logger.debug("Job removed from a category", category="_pending_jobs")  # type: ignore[call-arg]
            return True

        if job_id in self._done_jobs:
            del self._done_jobs[job_id]
            logger.debug("Job removed from a category", category="_done_jobs")  # type: ignore[call-arg]

        return True

    async def get_job(self, job_id) -> Optional[BatchJob]:
        """
        This retrieves a job's status to users.
        Job scheduler does not need to check job status. It can directly
        check the job pool for scheduling, such as pending_jobs.
        """
        if job_id in self._pending_jobs:
            return self._pending_jobs[job_id]
        elif job_id in self._in_progress_jobs:
            return self._in_progress_jobs[job_id]
        elif job_id in self._done_jobs:
            return self._done_jobs[job_id]

        if self._job_entity_manager:
            return await self._job_entity_manager.get_job(job_id)

        return None

    async def get_job_status(self, job_id: str) -> Optional[BatchJobStatus]:
        """Get the current status of a job."""
        job = await self.get_job(job_id)
        return job.status if job else None

    async def list_jobs(self) -> List[BatchJob]:
        """List all jobs."""
        # [TODO][NEXT Load all jobs from persistent store
        all_jobs: Optional[List[BatchJob]] = None
        if self._job_entity_manager:
            all_jobs = await self._job_entity_manager.list_jobs()
        else:
            # Collect jobs from all states
            all_jobs = []
            all_jobs.extend(self._pending_jobs.values())
            all_jobs.extend(self._in_progress_jobs.values())
            all_jobs.extend(self._done_jobs.values())

        # Sort by creation time (newest first)
        assert all_jobs is not None
        all_jobs.sort(key=lambda job: job.status.created_at, reverse=True)

        return all_jobs

    async def admit(self, job_id: str) -> Optional[JobDriver]:
        """Admit a pending job: validate it, promote pending -> in-progress, and
        build its driver. Returns the driver (handed to the scheduler to run) or
        None if the job could not be admitted. Called by the scheduler; the
        endpoint source is injected via set_endpoint_source, not threaded here.

        DO NOT OVERRIDE THIS IN THE TEST, A JOB SHOULD EITHER:
        * in state CREATED and in _pending_job, OR
        * not in state CREATED and in _in_progress_jobs.
        """
        if job_id not in self._pending_jobs:
            logger.warning("Job does not exist - maybe create it first", job_id=job_id)  # type: ignore[call-arg]
            return None
        if job_id in self._in_progress_jobs:
            logger.info("Job has already been launched", job_id=job_id)  # type: ignore[call-arg]
            return None

        job = self._pending_jobs[job_id]
        del self._pending_jobs[job_id]

        meta_data = JobMetaInfo(job)
        # In-place status update, will be reflected in the entity_manager if available.
        if job.status.state == BatchJobState.CREATED or (
            job.status.state == BatchJobState.IN_PROGRESS
            and job.status.in_progress_at is None
        ):
            # Only update state for first validation.
            meta_data.status.state = BatchJobState.VALIDATING
        self._in_progress_jobs[job_id] = meta_data
        logger.debug(
            "Job moved to a new category",
            old_category="_pending_jobs",
            new_category="_in_progress_jobs",
        )  # type: ignore[call-arg]

        try:
            job_driver = create_job_driver(
                self._context,
                self,
                self._job_entity_manager,
                meta_data,
                self._endpoint_source,
            )
            await job_driver.validate_job(meta_data.batch_job)
            # But we do not update state for in-progress job.
            if meta_data.status.state == BatchJobState.VALIDATING:
                meta_data.status.in_progress_at = datetime.now(timezone.utc)
                meta_data.status.state = BatchJobState.IN_PROGRESS
        except Exception as e:
            logger.error("Job validation failed", job_id=job_id, exc_info=True)  # type: ignore[call-arg]
            error = (
                e
                if isinstance(e, BatchJobError)
                else BatchJobError(
                    code=BatchJobErrorCode.VALIDATION_ERROR, message=str(e)
                )
            )
            await self.mark_job_failed(
                job_id,
                error,
            )
            return None

        return job_driver

    async def get_job_endpoint(self, job_id: str) -> str:
        if job_id in self._pending_jobs:
            job = self._pending_jobs[job_id]
        elif job_id in self._in_progress_jobs:
            job = self._in_progress_jobs[job_id]
        else:
            logger.info("Job is discarded", job_id=job_id)  # type: ignore[call-arg]
            return ""
        return str(job.spec.endpoint)

    async def mark_job_progress(self, job_id: str, req_id: int) -> Tuple[BatchJob, int]:
        """
        This is used to sync job's progress, called by job driver.
        It is guaranteed that each request is executed at least once.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        meta_data = await self._meta_from_in_progress_job(job_id)

        if req_id < 0 or req_id > meta_data.status.request_counts.total:
            raise ValueError(f"invalide request_id: {req_id}")

        meta_data.complete_one_request(req_id)
        return meta_data, meta_data.next_request_id()

    async def mark_jobs_progresses(
        self, job_id: str, executed_requests: List[int]
    ) -> BatchJob:
        """
        This is the batch operation to sync jobs' progresses, called by job driver.
        It is guaranteed that each request is executed at least once.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        meta_data = await self._meta_from_in_progress_job(job_id)

        request_len = meta_data.status.request_counts.total
        for req_id in executed_requests:
            if req_id < 0 or req_id > request_len:
                logger.error(  # type: ignore[call-arg]
                    "Mark job progress failed - request index out of boundary",
                    job_id=job_id,
                    req_id=req_id,
                    total=request_len,
                )
                continue
            meta_data.complete_one_request(req_id)

        return meta_data

    async def get_job_next_request(self, job_id: str) -> Tuple[BatchJob, int]:
        """
        Get next request id to execute, see JobMetaInfo::next_request_id for details

        Returns:
            tuple: (job, next_request_id) or (job, -1) if job is done

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        meta_data = await self._meta_from_in_progress_job(job_id)
        return meta_data, meta_data.next_request_id()

    async def complete_job_request(
        self, job_id: str, req_id: int, failed: bool = False
    ) -> BatchJob:
        """Mark a request completed without advancing the serial cursor."""
        meta_data = await self._meta_from_in_progress_job(job_id)
        async with meta_data._async_lock:
            if req_id < 0 or req_id >= meta_data.status.request_counts.total:
                raise ValueError(f"invalid request_id: {req_id}")
            meta_data.complete_one_request(req_id, failed=failed)
            return meta_data

    async def mark_job_progress_and_get_next_request(
        self, job_id: str, req_id: int
    ) -> Tuple[BatchJob, int]:
        """
        This is used to sync job's progress, called by execution proxy.
        It is guaranteed that each request is executed at least once.

        Returns:
            tuple: (job, next_request_id) or (job, -1) if job is done

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        meta_data = await self._meta_from_in_progress_job(job_id)

        meta_data.complete_one_request(req_id)
        return meta_data, meta_data.next_request_id()

    async def mark_job_total(self, job_id: str, total_requests: int) -> BatchJob:
        """
        This is used to set job's total requests when stream reader sees the end of the request.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        job, _ = await self.mark_job_progress(job_id, total_requests + 1)
        return job

    async def mark_job_done(self, job_id: str) -> BatchJob:
        """
        Mark job done.

        Raises:
            JobUnexpectedStateError: If job is not in progress and not finalizing.
        """
        try:
            meta_data = await self._meta_from_in_progress_job(job_id)
        except JobUnexpectedStateError as juse:
            logger.warning(str(juse), state=juse.state)  # type: ignore[call-arg]
            raise

        if meta_data.status.state != BatchJobState.FINALIZING:
            logger.error("Job is not in finalizing state", state=meta_data.status.state)  # type: ignore[call-arg]
            raise JobUnexpectedStateError(
                "Job is not in finalizing state", meta_data.status.state
            )

        job = meta_data.model_copy(deep=True)
        now = datetime.now(timezone.utc)
        job.status.finalized_at = now
        # Stamp the terminal timestamp matching the existing condition. A job
        # cancelled while in progress reaches finalize already carrying a
        # CANCELLED condition; stamping completed_at there would lose
        # cancelled_at and mislabel the outcome. With no condition yet this is a
        # normal completion. Do not override an existing condition.
        existing = job.status.condition
        if existing is None:
            job.status.completed_at = now
            job.status.add_condition(
                Condition(
                    type=ConditionType.COMPLETED,
                    status=ConditionStatus.TRUE,
                    lastTransitionTime=now,
                )
            )
        elif existing == ConditionType.CANCELLED:
            job.status.cancelled_at = now
        elif existing == ConditionType.FAILED:
            job.status.failed_at = now
        elif existing == ConditionType.EXPIRED:
            job.status.expired_at = now
        else:  # COMPLETED already recorded
            job.status.completed_at = now
        job.status.state = BatchJobState.FINALIZED

        if not await self.apply_job_changes(job, meta_data):
            return meta_data

        logger.info("Job is finalized", job_id=job_id)  # type: ignore[call-arg]
        return job

    async def mark_job_failed(self, job_id: str, ex: BatchJobError) -> BatchJob:
        """
        Mark job failed.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        meta_data = await self._meta_from_in_progress_job(job_id)

        job = meta_data.model_copy(deep=True)
        job.status.failed_at = datetime.now(timezone.utc)
        # Fill up locally for data integrity in case apply_job_changes does nothing
        job.status.add_condition(
            Condition(
                type=ConditionType.FAILED,
                status=ConditionStatus.TRUE,
                lastTransitionTime=job.status.failed_at,
                reason=ex.code,
                message=ex.message,
            )
        )
        job.status.errors = [ex]
        has_any_output_artifact_prepared = (
            meta_data.status.output_file_id is not None
            or meta_data.status.error_file_id is not None
            or meta_data.status.temp_output_file_id is not None
            or meta_data.status.temp_error_file_id is not None
        )
        if (
            meta_data.status.state == BatchJobState.IN_PROGRESS
            and has_any_output_artifact_prepared
        ):
            job.status.finalizing_at = datetime.now(timezone.utc)
            job.status.state = BatchJobState.FINALIZING
        else:
            job.status.finalized_at = job.status.failed_at
            job.status.state = BatchJobState.FINALIZED

        if not await self.apply_job_changes(job, meta_data):
            return meta_data

        logger.info("Job failed", job_id=job_id)  # type: ignore[call-arg]
        return job

    async def expire_job(self, job_id: str) -> bool:
        """Expire a pending job whose completion window has passed: move it to
        FINALIZED (condition: expired) and into the done pool, persisting the
        status via the entity manager. Only pending (not-yet-admitted) jobs are
        expired here — an admitted job runs to completion. Pending jobs are not
        yet submitted to any execution backend, so the in-process scheduler is
        the only component that can expire them.

        Returns True if the job was expired. Called by the scheduler's cleanup
        loop, which makes the registry the single source of truth for expiry
        (no separate "inactive" set on the scheduler).
        """
        job = self._pending_jobs.get(job_id)
        if job is None:
            # Raced with admission: the job left the pending pool (admitted, now
            # running to completion) between the cleanup snapshot and here. This
            # is intended — only pending jobs expire — but log it for visibility.
            logger.debug("Skip expiry: job no longer pending", job_id=job_id)  # type: ignore[call-arg]
            return False

        expired_at = datetime.now(timezone.utc)
        job_expired = job.model_copy(deep=True)
        job_expired.status.add_condition(
            Condition(
                type=ConditionType.EXPIRED,
                status=ConditionStatus.TRUE,
                lastTransitionTime=expired_at,
            )
        )
        job_expired.status.expired_at = expired_at
        job_expired.status.finalized_at = expired_at
        job_expired.status.state = BatchJobState.FINALIZED

        if self._job_entity_manager is not None:
            # Persist the expiry through the store; it emits job_updated, which
            # drives the pending -> done pool transition via job_updated_handler.
            await self._job_entity_manager.update_job_status(job_expired)
        elif not await self.job_updated_handler(job, job_expired):
            return False
        logger.info("Job expired", job_id=job_id)  # type: ignore[call-arg]
        return True

    async def list_pending(self) -> List[BatchJob]:
        """Snapshot of the registry's pending pool. The scheduler derives expiry
        from this rather than a duplicated due-time list."""
        return list(self._pending_jobs.values())

    async def apply_job_changes(
        self, job: BatchJob, old_job: Optional[BatchJob] = None
    ) -> bool:
        """
        Sync job status to persistent storage by calling update_job_status.

        This persists critical job status information including finalized state,
        conditions, request counts, and timestamps to Kubernetes annotations
        to ensure job state can be recovered after crashes.

        Args:
            job_id: Job ID to sync to storage
        """
        try:
            # Call update directly
            if old_job is None:
                old_job = await self.get_job(job.job_id)
                assert old_job is not None

            # Persist via the bridge (it picks the update vs cancel verb).
            if self._job_entity_manager:
                await self._bridge.persist_status(job, old_job)

                logger.debug(
                    "Job status synced to job entity manager",
                    job_id=job.job_id,
                    state=job.status.state,
                    condition=job.status.condition,
                )  # type: ignore[call-arg]
                return True

            logger.debug("Job status synced to job entity manager")
            await self.job_updated_handler(old_job, job)
            return True
        except Exception as e:
            logger.error(
                "Failed to apply job changes",
                job_id=job.job_id,
                error=str(e),
            )  # type: ignore[call-arg]
            # Don't re-raise - this is a background sync operation
            return False

    async def _meta_from_in_progress_job(self, job_id: str) -> JobMetaInfo:
        if job_id not in self._in_progress_jobs:
            job = await self.get_job(job_id)
            raise JobUnexpectedStateError(
                "Job has not been scheduled yet or has been scheduled",
                job.status.state if job else None,
            )

        job = self._in_progress_jobs[job_id]
        assert isinstance(job, JobMetaInfo)
        meta_data: JobMetaInfo = job
        return meta_data

    def _categorize_jobs(
        self, job: BatchJob, first_seen: bool = False
    ) -> Tuple[dict[str, BatchJob], str]:
        """
        This is used to categorize jobs into pending, in progress, and done.
        """
        if not job.status:
            return self._pending_jobs, "_pending_jobs"
        if job.status.state == BatchJobState.CREATED:
            return self._pending_jobs, "_pending_jobs"
        elif job.status.finished:
            return self._done_jobs, "_done_jobs"
        elif first_seen and self._job_scheduler:
            # We need to pending jobs to be scheduled to make progress
            return self._pending_jobs, "_pending_jobs"
        else:
            return self._in_progress_jobs, "_in_progress_jobs"
