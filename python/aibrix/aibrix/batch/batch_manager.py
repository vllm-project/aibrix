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
from typing import Any, Dict, List, Optional, Tuple, cast

from aibrix.batch.batch_scheduler import BatchScheduler
from aibrix.batch.client import EndpointSource
from aibrix.batch.job_driver import (
    JobDriver,
    RunningJobs,
    TerminateResult,
    create_job_driver,
)
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchJobStatusCopy,
    Condition,
    ConditionStatus,
    ConditionType,
    aggregate_batch_job_status,
    ensure_batch_job_error,
)
from aibrix.batch.state import (
    BatchRegistry,
    EntityManagerBridge,
    JobEntityManager,
    JobMetaInfo,
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

JobBucket = dict[str, BatchJob] | dict[str, JobMetaInfo]


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
    # 3b. In_progress -> Finalized (condition: failed before temp files exist)
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
        self._pending_deleted_jobs: set[str] = set()

    # The three pools live in the BatchRegistry; these proxies keep the existing
    # by-pool access (orchestration logic + tests) working unchanged while the
    # registry concern is owned by a named service-level component.
    @property
    def _pending_jobs(self) -> dict[str, BatchJob]:
        return self._registry.pending

    @property
    def _in_progress_jobs(self) -> dict[str, JobMetaInfo]:
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

    def _as_job_meta(self, job: BatchJob) -> JobMetaInfo:
        if isinstance(job, JobMetaInfo):
            return job
        return JobMetaInfo(job)

    def _refresh_job_meta(self, meta_job: JobMetaInfo, job: BatchJob) -> JobMetaInfo:
        meta_job.session_id = job.session_id
        meta_job.type_meta = job.type_meta
        meta_job.metadata = job.metadata
        meta_job.spec = job.spec
        meta_job.status = job.status
        return meta_job

    def _normalize_recovered_in_progress_job_for_cleanup(
        self, job_id: str, job: BatchJob
    ) -> JobMetaInfo | BatchJob:
        """Move a recovered active job into the in-progress registry if needed.

        Restart/bootstrap can temporarily stage a persisted ``IN_PROGRESS`` job
        in ``_pending_jobs`` before scheduler admission rebuilds the runtime
        state. Expire/cancel cleanup paths operate on the in-progress registry,
        so normalize that recovered shape before persisting status updates or
        clearing durable execution metadata.
        """

        if job.status.state != BatchJobState.IN_PROGRESS:
            return job
        if job_id in self._in_progress_jobs:
            return self._in_progress_jobs[job_id]
        if job_id not in self._pending_jobs:
            return job

        meta_data = self._as_job_meta(job)
        del self._pending_jobs[job_id]
        self._in_progress_jobs[job_id] = meta_data
        logger.debug(
            "Normalized recovered in-progress job for cleanup",
            job_id=job_id,
        )  # type: ignore[call-arg]
        return meta_data

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

    def reset_runtime_state(self) -> None:
        self._registry.reset_runtime_state()
        self._bridge.reset_runtime_state()
        self._pending_deleted_jobs.clear()
        self._creation_timeouts.clear()
        self._session_metadata.clear()

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

    async def cancel_job(self, job_id: str) -> TerminateResult:
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
            TerminateResult: cancellation decision/result for the target job
        """
        # Check if job exists in any state
        job = None
        job_driver = None
        job_in_progress = False
        if job_id in self._pending_jobs:
            job = self._pending_jobs[job_id]
            # remove from _pending_jobs to prevent scheduling anyway.
            del self._pending_jobs[job_id]
            logger.debug("Job removed from a category", category="_pending_jobs")  # type: ignore[call-arg]
        elif job_id in self._in_progress_jobs:
            job = self._in_progress_jobs[job_id]
            job_driver = job.job_driver
            job_in_progress = job.status.state == BatchJobState.IN_PROGRESS
        elif job_id in self._done_jobs:
            # Job is already done (completed, failed, expired, or cancelled)
            logger.debug("Job is already in final state", job_id=job_id)  # type: ignore[call-arg]
            return TerminateResult.REJECTED
        else:
            logger.warning("Job not found", job_id=job_id)  # type: ignore[call-arg]
            return TerminateResult.REJECTED

        # Check if job is finalizing
        # We allow CANCELLING job be signalled again.
        if job.status.state == BatchJobState.FINALIZING:
            logger.info(  # type: ignore[call-arg]
                "Job is finalizing", job_id=job_id, state=job.status.state
            )
            return TerminateResult.REJECTED
        if job.status.state == BatchJobState.CANCELLING or job.status.check_condition(
            ConditionType.CANCELLED
        ):
            return TerminateResult.ALREADY_REQUESTED

        # Keep cancel initiation on the live in-memory job/driver path.
        # If we later decide to seed this from persisted state again, we must
        # re-check the reloaded status before applying cancellation.
        job_cancelled = self._build_cancelled_job_update(job)
        cancelling_at = job_cancelled.status.cancelling_at
        assert cancelling_at is not None

        if job_driver is not None:
            terminate_result = await job_driver.terminate(job_cancelled)
            if terminate_result == TerminateResult.REJECTED:
                logger.info(  # type: ignore[call-arg]
                    "Live job rejected cancellation",
                    job_id=job_id,
                    state=job_cancelled.status.state,
                )
                return TerminateResult.REJECTED
            return terminate_result

        # Fallback path
        job.status.state = BatchJobState.CANCELLING
        job.status.cancelling_at = cancelling_at
        job.status.conditions = job_cancelled.status.conditions
        if not job_in_progress:
            self._in_progress_jobs[job_id] = JobMetaInfo(job)
            logger.debug(
                "Job added to a category during cancelling", category="_pending_jobs"
            )  # type: ignore[call-arg]

        if self._job_entity_manager:
            # Signal the entity manager to cancel the job
            # The actual state update will be handled by job_updated_handler when called back
            await self._job_entity_manager.cancel_job(job_cancelled)
            return TerminateResult.ACCEPTED

        await self.job_updated_handler(job, job_cancelled)
        return TerminateResult.ACCEPTED

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
        if job.session_id:
            self._bridge.resolve_creation(job.session_id, job_id)

        # Safeguard for handling existing jobs
        existing_job = (
            self._pending_jobs.get(job_id)
            or self._in_progress_jobs.get(job_id)
            or self._done_jobs.get(job_id)
        )
        if existing_job is not None:
            return await self.job_updated_handler(existing_job, job)

        category, name = self._categorize_jobs(job, first_seen=True)
        if category is self._in_progress_jobs:
            self._in_progress_jobs[job_id] = self._as_job_meta(job)
        else:
            cast(dict[str, BatchJob], category)[job_id] = job
        logger.debug("Job added to a category", category=name)  # type: ignore[call-arg]

        if category is self._done_jobs:
            logger.debug(  # type: ignore[call-arg]
                "Recovered terminal job without scheduling", job_id=job_id
            )
            return False

        if job.is_expiring():
            logger.info(
                "Expire committed job before scheduling",
                job_id=job_id,
                state=job.status.state,
                condition=job.status.condition,
            )  # type: ignore[call-arg]
            await self.expire_job(job_id)
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
                error = ensure_batch_job_error(e, BatchJobErrorCode.INFERENCE_FAILED)
                await self.mark_job_failed(
                    job_id,
                    error,
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
                _preserve_local_timestamps(old_job.status, new_job.status)
                old_job.metadata = new_job.metadata  # Update resource version
                old_job.status = new_job.status  # Update status
                new_job = old_job
            else:
                # Move job from old category to new category
                _preserve_local_timestamps(old_job.status, new_job.status)
                del old_category[job_id]
                if new_category is self._in_progress_jobs:
                    self._in_progress_jobs[job_id] = self._refresh_job_meta(
                        self._as_job_meta(old_job), new_job
                    )
                    new_job = self._in_progress_jobs[job_id]
                else:
                    cast(dict[str, BatchJob], new_category)[job_id] = new_job
                logger.debug(
                    "Job moved to a new category",
                    old_category=old_name,
                    new_category=new_name,
                )  # type: ignore[call-arg]

            if job_id in self._pending_deleted_jobs and new_job.status.finished:
                await self.job_deleted_handler(new_job)
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
            # This is basically a cancel trigger by the entity manager.
            # Jobs added to _pending_deleted_jobs will kept in JobManager, and will be
            # finally removed after job get finalized and put in _done_jobs.
            self._pending_deleted_jobs.add(job_id)
            in_progress_job = self._in_progress_jobs[job_id]
            job_driver = in_progress_job.job_driver
            if job_driver is not None and not await job_driver.terminate(job):
                return False
            if in_progress_job.status.state not in (
                BatchJobState.CANCELLING,
                BatchJobState.FINALIZING,
            ):
                job_cancelled = self._build_cancelled_job_update(in_progress_job)
                await self.job_updated_handler(in_progress_job, job_cancelled)
            return True

        if job_id in self._pending_jobs:
            self._pending_deleted_jobs.discard(job_id)
            del self._pending_jobs[job_id]
            logger.debug("Job removed from a category", category="_pending_jobs")  # type: ignore[call-arg]
            return True

        if job_id in self._done_jobs:
            self._pending_deleted_jobs.discard(job_id)
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

    async def list_jobs(
        self,
        after: Optional[str] = None,
        limit: int = JobEntityManager.DEFAULT_JOB_PAGE_LIMIT,
    ) -> List[BatchJob]:
        """List all jobs."""
        if self._job_entity_manager:
            return await self._job_entity_manager.list_jobs(after=after, limit=limit)

        # Collect jobs from all states
        all_jobs: List[BatchJob] = []
        all_jobs.extend(self._pending_jobs.values())
        all_jobs.extend(self._in_progress_jobs.values())
        all_jobs.extend(self._done_jobs.values())

        # Sort by creation time (newest first)
        assert all_jobs is not None
        all_jobs.sort(key=lambda job: job.status.created_at, reverse=True)
        if after:
            after_index = next(
                (
                    index
                    for index, job in enumerate(all_jobs)
                    if job.job_id == after or job.status.job_id == after
                ),
                -1,
            )
            if after_index < 0:
                return []
            all_jobs = all_jobs[after_index + 1 :]
        return all_jobs[:limit]

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
        needs_validation = job.status.state == BatchJobState.CREATED or (
            job.status.state == BatchJobState.IN_PROGRESS
            and job.status.in_progress_at is None
        )
        # Build the active JobMetaInfo from a deep copy so the validating
        # transition does not mutate the still-pending store snapshot before the
        # entity manager persists the CREATED -> VALIDATING update.
        meta_data = JobMetaInfo(job.model_copy(deep=True))
        # In-place status update, will be reflected in the entity_manager if available.
        if needs_validation:
            # Only update state for first validation.
            meta_data.status.state = BatchJobState.VALIDATING
        if needs_validation and self._job_entity_manager is not None:
            # Persist the validating snapshot first so the entity manager's
            # callback sees the job in the same category transition path that
            # BatchManager expects: pending -> in_progress.
            await self._job_entity_manager.update_job_status(meta_data)
            meta_data = await self._meta_from_in_progress_job(job_id)
        else:
            del self._pending_jobs[job_id]
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
            meta_data._job_driver = job_driver
            if needs_validation:
                await job_driver.validate_job(meta_data.batch_job)
                # Reload status since we expect validate_job will update and persist status.
                if self._job_entity_manager is not None:
                    reloaded_job = await self._job_entity_manager.get_job(job_id)
                    if reloaded_job is not None:
                        meta_data.metadata = (
                            reloaded_job.metadata
                        )  # Update resource version (as in k8s job)
                        meta_data.status = reloaded_job.status  # Update status
        except Exception as e:
            logger.error("Job validation failed", job_id=job_id, exc_info=True)  # type: ignore[call-arg]
            error = ensure_batch_job_error(e, BatchJobErrorCode.VALIDATION_ERROR)
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

    async def mark_job_validated(self, job_id: str, status: BatchJobStatus) -> BatchJob:
        meta_data = await self._meta_from_in_progress_job(job_id)
        validated = meta_data.copy(status)
        if validated.status.state == BatchJobState.VALIDATING:
            validated.status.in_progress_at = datetime.now(timezone.utc)
            validated.status.state = BatchJobState.IN_PROGRESS

        if self._job_entity_manager is not None:
            await self._job_entity_manager.update_job_status(validated)
        await self.job_updated_handler(meta_data, validated)
        return validated

    async def update_job_status(self, job_id: str, status: BatchJobStatus) -> BatchJob:
        meta_data = await self._meta_from_in_progress_job(job_id)
        updated = meta_data.copy(status)
        if self._job_entity_manager is not None:
            await self._job_entity_manager.update_job_status(updated)
        await self.job_updated_handler(meta_data, updated)
        return updated

    async def update_job_local_status(
        self, job_id: str, worker_id: str, status: BatchJobStatus
    ) -> BatchJob:
        # Cancel, failing, expiring will not move job out of in_progress state
        meta_data = await self._meta_from_in_progress_job(job_id)
        persisted = meta_data.copy()
        persisted.status.execution = status.execution
        # Set worker-wise status copies.
        if persisted.status.status_copies is None:
            persisted.status.status_copies = {}
        persisted.status.status_copies[worker_id] = BatchJobStatusCopy.from_status(
            status
        )
        persisted.status.status_copies[worker_id].updated = True
        persisted.status = aggregate_batch_job_status(persisted.status)

        if self._job_entity_manager is not None:
            await self._job_entity_manager.update_job_status(persisted)
        await self.job_updated_handler(meta_data, persisted)
        return persisted

    async def mark_job_finalizing(self, job_id: str) -> BatchJob:
        meta_data = await self._meta_from_in_progress_job(job_id)
        if meta_data.status.state == BatchJobState.FINALIZING:
            return meta_data

        live_cancelled = (
            meta_data.status.state == BatchJobState.CANCELLING
            or meta_data.status.condition == ConditionType.CANCELLED
        )
        persisted: BatchJob = meta_data.copy()
        if self._job_entity_manager is not None:
            if live_cancelled:
                # A live cancel request may already have updated the shared
                # JobMetaInfo while store persistence is still in flight. In
                # that boundary window, prefer the live state so finalizing does
                # not overwrite a cancellation into completion.
                persisted = meta_data.copy()
            else:
                # Reload status copies from backend to collect all (including left over) status copies.
                loaded_job = await self._job_entity_manager.get_job(job_id)
                if loaded_job is None:
                    logger.warning(
                        "Job missing from entity manager during finalizing reload",
                        job_id=job_id,
                        state=meta_data.status.state,
                    )  # type: ignore[call-arg]
                    raise JobUnexpectedStateError(
                        "Job missing from entity manager during finalizing reload",
                        meta_data.status.state,
                    )
                persisted = loaded_job
        else:
            persisted = meta_data.copy()
        if persisted.status.finalizing_at is None:
            persisted.status.finalizing_at = datetime.now(timezone.utc)
        persisted.status.state = BatchJobState.FINALIZING
        if persisted.status.condition is None:
            request_counts = persisted.status.request_counts
            if request_counts.completed + request_counts.failed < request_counts.total:
                # Calibrate total requests
                logger.warning(
                    "Calibrate total requests to %d during finalizing with no condition set, completed %d, failed %d",
                    request_counts.completed + request_counts.failed,
                    request_counts.completed,
                    request_counts.failed,
                )  # type: ignore[call-arg]
                persisted.status.request_counts.total = (
                    request_counts.completed + request_counts.failed
                )
            persisted.status.add_condition(
                Condition(
                    type=ConditionType.COMPLETED,
                    status=ConditionStatus.TRUE,
                    lastTransitionTime=datetime.now(timezone.utc),
                )
            )

        if self._job_entity_manager:
            await self._job_entity_manager.update_job_status(persisted)
        await self.job_updated_handler(meta_data, persisted)
        return persisted

    async def mark_job_done(self, job: BatchJob) -> BatchJob:
        """
        Mark job done.

        Raises:
            JobUnexpectedStateError: If job is not in progress and not finalizing.
        """
        try:
            meta_data = await self._meta_from_in_progress_job(job.job_id)
        except JobUnexpectedStateError as juse:
            logger.warning(str(juse), state=juse.state)  # type: ignore[call-arg]
            raise

        if meta_data.status.state != BatchJobState.FINALIZING:
            logger.error("Job is not in finalizing state", state=meta_data.status.state)  # type: ignore[call-arg]
            raise JobUnexpectedStateError(
                "Job is not in finalizing state", meta_data.status.state
            )

        logger.debug(
            "mark_job_done source counts",
            job_id=job.job_id,
            total=job.status.request_counts.total,
            launched=job.status.request_counts.launched,
            completed=job.status.request_counts.completed,
            failed=job.status.request_counts.failed,
            state=job.status.state,
        )  # type: ignore[call-arg]

        # Copy the target status snapshot so the live in-progress JobMetaInfo is
        # not mutated to FINALIZED before job_updated_handler computes the old
        # category transition.
        job = meta_data.copy(job.status.model_copy(deep=True))
        finalized_at = datetime.now(timezone.utc)
        job.status.finalized_at = finalized_at
        # Do not override existing condition. Fill up locally for data integrity in case apply_job_changes does nothing
        if job.status.condition is None:
            job.status.completed_at = finalized_at
            job.status.add_condition(
                Condition(
                    type=ConditionType.COMPLETED,
                    status=ConditionStatus.TRUE,
                    lastTransitionTime=finalized_at,
                )
            )
        elif job.status.condition == ConditionType.COMPLETED:
            if job.status.completed_at is None:
                job.status.completed_at = finalized_at
        elif job.status.condition == ConditionType.EXPIRED:
            if job.status.expired_at is None:
                job.status.expired_at = finalized_at
        elif job.status.condition == ConditionType.CANCELLED:
            if job.status.cancelled_at is None:
                job.status.cancelled_at = finalized_at
        elif job.status.condition == ConditionType.FAILED:
            if job.status.failed_at is None:
                job.status.failed_at = finalized_at
        job.status.state = BatchJobState.FINALIZED

        if not await self.conclude_job(job, meta_data):
            return meta_data

        logger.info("Job is finalized", job_id=job.job_id)  # type: ignore[call-arg]
        return job

    async def mark_job_failed(self, job_id: str, ex: BatchJobError) -> BatchJob:
        """
        Mark job failed.

        Raises:
            JobUnexpectedStateError: If job is not in progress.
        """
        existing_job = await self.get_job(job_id)
        if existing_job is not None and existing_job.status.finished:
            logger.info(
                "Job already failed or finalized, ignoring duplicate mark_job_failed",
                job_id=job_id,
                state=existing_job.status.state,
            )  # type: ignore[call-arg]
            return existing_job

        meta_data = await self._meta_from_in_progress_job(job_id)

        failed_at = datetime.now(timezone.utc)
        job = meta_data.copy()
        job.status.errors = [ex]
        job.status.add_condition(
            Condition(
                type=ConditionType.FAILED,
                status=ConditionStatus.TRUE,
                lastTransitionTime=failed_at,
                reason=ex.code,
                message=ex.message,
            )
        )
        if meta_data.status.state != BatchJobState.IN_PROGRESS or not (
            meta_data.status.temp_output_file_id and meta_data.status.temp_error_file_id
        ):
            job.status.finalized_at = failed_at
            job.status.failed_at = failed_at
            job.status.state = BatchJobState.FINALIZED

        if not await self.conclude_job(job, meta_data):
            return meta_data

        logger.info("Job failed", job_id=job_id)  # type: ignore[call-arg]
        return job

    async def expire_job(self, job_id: str) -> bool:
        """Expire a schedulable job whose completion window has passed.

        Pending jobs finalize immediately into the done pool. In-progress jobs
        persist the expired condition and then signal their live driver like the
        cancellation flow, so execution stops before finalizing the expired job.
        Jobs that have already entered FINALIZING are past the interruption
        point; expiration is rejected so the in-flight terminalization can
        complete consistently.

        Returns True if the job was expired. Called by the scheduler's cleanup
        loop, which makes the registry the single source of truth for expiry
        (no separate "inactive" set on the scheduler).
        """
        old_job = self._pending_jobs.get(job_id) or self._in_progress_jobs.get(job_id)
        if old_job is None:
            existing_job = await self.get_job(job_id)
            if existing_job is None:
                raise JobUnexpectedStateError(
                    "Expiring job does not exist",
                    None,
                )
            if existing_job.status.finished:
                return True
            raise JobUnexpectedStateError(
                "Unexpected job state on expiring",
                existing_job.status.state,
            )

        if (
            old_job.status.state == BatchJobState.IN_PROGRESS
            and old_job.status.check_condition(ConditionType.EXPIRED)
        ):
            return True

        if old_job.status.state == BatchJobState.FINALIZING:
            logger.info(
                "Job already finalizing, skipping expiration",
                job_id=job_id,
                state=old_job.status.state,
            )  # type: ignore[call-arg]
            return False

        old_job = self._normalize_recovered_in_progress_job_for_cleanup(job_id, old_job)

        job_expired = self._build_expired_job_update(old_job)
        if old_job.status.state in (BatchJobState.CREATED, BatchJobState.VALIDATING):
            expired_at = job_expired.status.expired_at
            assert expired_at is not None
            job_expired.status.finalized_at = expired_at
            job_expired.status.state = BatchJobState.FINALIZED

        if not await self.conclude_job(job_expired, old_job):
            return False

        job_driver = getattr(old_job, "job_driver", None)
        if job_driver is not None:
            terminate_result = await job_driver.terminate(job_expired)
            if terminate_result == TerminateResult.REJECTED:
                logger.info(  # type: ignore[call-arg]
                    "Live job rejected expiration termination",
                    job_id=job_id,
                    state=job_expired.status.state,
                )
                return False
            logger.info("Job expired", job_id=job_id)  # type: ignore[call-arg]
            return True

        # The job may already have persisted durable execution metadata even if
        # the original in-memory driver instance was lost (restart / recovery /
        # category reload before the driver ref was rebound). In that orphaned
        # state there is no live driver to terminate, but the provisioned
        # runtime may still exist, so reconstruct a driver only to run the
        # best-effort cleanup path and clear the stale execution ref.
        if job_expired.status.execution is not None:
            cleanup_driver = create_job_driver(
                self._context,
                self,
                self._job_entity_manager,
                old_job,
                self._endpoint_source,
            )
            await cleanup_driver.cleanup(job_expired)
            logger.info(
                "Expired job cleaned orphaned execution",
                job_id=job_id,
            )  # type: ignore[call-arg]

        logger.info("Job expired", job_id=job_id)  # type: ignore[call-arg]
        return True

    async def list_pending(self) -> List[BatchJob]:
        """Snapshot of the registry's pending pool. The scheduler derives expiry
        from this rather than a duplicated due-time list."""
        return list(self._pending_jobs.values())

    async def list_in_progress(self) -> List[BatchJob]:
        """Snapshot of admitted in-progress jobs for expiry checks."""
        return list(self._in_progress_jobs.values())

    async def conclude_job(
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
                "Job has not been scheduled yet or has been processed",
                job.status.state if job else None,
            )

        return self._in_progress_jobs[job_id]

    def _categorize_jobs(
        self, job: BatchJob, first_seen: bool = False
    ) -> Tuple[JobBucket, str]:
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

    def _build_cancelled_job_update(self, job: BatchJob) -> BatchJob:
        cancelling_at = datetime.now(timezone.utc)
        job_cancelled = job.copy()
        job_cancelled.status.cancelling_at = cancelling_at
        job_cancelled.status.add_condition(
            Condition(
                type=ConditionType.CANCELLED,
                status=ConditionStatus.TRUE,
                lastTransitionTime=cancelling_at,
            )
        )
        if job.status.state in (BatchJobState.CREATED, BatchJobState.VALIDATING):
            job_cancelled.status.state = BatchJobState.FINALIZED
            job_cancelled.status.finalized_at = cancelling_at
            job_cancelled.status.cancelled_at = cancelling_at
        else:
            job_cancelled.status.state = BatchJobState.CANCELLING
        return job_cancelled

    def _build_expired_job_update(self, job: BatchJob) -> BatchJob:
        expired_at = datetime.now(timezone.utc)
        job_expired = job.copy()
        if not job_expired.status.check_condition(ConditionType.EXPIRED):
            job_expired.status.add_condition(
                Condition(
                    type=ConditionType.EXPIRED,
                    status=ConditionStatus.TRUE,
                    lastTransitionTime=expired_at,
                )
            )
        if job.status.state != BatchJobState.IN_PROGRESS:
            job_expired.status.expired_at = expired_at
        return job_expired
