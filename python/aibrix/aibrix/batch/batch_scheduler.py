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
"""BatchScheduler: the scheduling routine for the in-process / standalone path.

A scheduler is *mechanism* + *policy*. This class is the mechanism: it holds the
queue, admits jobs (``validate_job`` -> in-progress), expires past-due jobs, and
runs the perpetual loop that picks the next job and dispatches it to that job's
driver. The *ordering* — which job goes next — is delegated to a pluggable
``SchedulingPolicy`` (FIFO now, prefix-aware / priority later).

Dispatching a picked job to its driver is a scheduler's job (pick + dispatch,
like an OS scheduler); the per-job execution itself lives in the JobDriver.
"""

import asyncio
import time
from typing import TYPE_CHECKING, Optional, Tuple

import aibrix.batch.constant as constant
from aibrix.batch.job_entity import BatchJobState
from aibrix.batch.job_lease import JobLease
from aibrix.batch.scheduling_policy import FIFOScheduling, SchedulingPolicy
from aibrix.batch.state import SchedulableJobs
from aibrix.context import InfrastructureContext
from aibrix.logger import init_logger

if TYPE_CHECKING:
    from aibrix.batch.job_driver import JobDriver

# BatchManager will be passed as parameter to avoid circular import

logger = init_logger(__name__)


class BatchScheduler:
    def __init__(
        self,
        context: InfrastructureContext,
        job_progress_manager: SchedulableJobs,
        pool_size: int = constant.DEFAULT_JOB_POOL_SIZE,
        policy: Optional[SchedulingPolicy] = None,
        job_lease: Optional[JobLease] = None,
    ) -> None:
        """
        self._policy owns the ordering of queued jobs (FIFO by default).
        Expiry is derived from the registry's pending pool (see expire_jobs);
        the scheduler keeps no duplicated due-time / inactive bookkeeping — the
        registry is the single source of truth for job state.

        ``pool_size`` caps the number of concurrently executing jobs while the
        policy still decides which pending job is admitted next.
        """
        self._context = context
        self._job_progress_manager = job_progress_manager
        self._pool_size = max(1, pool_size)
        self._policy: SchedulingPolicy = policy or FIFOScheduling()
        self._job_lease = job_lease
        self._idle_interval = constant.SCHEDULE_IDLE_INTERVAL
        self._expire_interval = constant.EXPIRE_INTERVAL

        # Set by start(); declared here (not just in start()) so a
        # stop()-before-start() is well-defined.
        self._serve_loop: Optional[asyncio.AbstractEventLoop] = None
        self._jobs_running_task: Optional[asyncio.Task] = None
        self._jobs_cleanup_task: Optional[asyncio.Task] = None
        self._job_execution_tasks: dict[str, asyncio.Task[None]] = {}
        self._queued_job_ids: set[str] = set()
        self._lease_retry_at: dict[str, float] = {}

    def append_job(self, job_id: str):
        # Enqueue a job for scheduling; the policy owns the ordering. Expiry is
        # derived from the registry's pending pool, so no due-time is tracked.
        if (
            job_id in self._queued_job_ids
            or job_id in self._lease_retry_at
            or job_id in self._job_execution_tasks
        ):
            return
        self._policy.add(job_id)
        self._queued_job_ids.add(job_id)

    def _next_job(self) -> Optional[str]:
        job_id = self._policy.next()
        if job_id is not None:
            self._queued_job_ids.discard(job_id)
        return job_id

    def _defer_job_for_lease(self, job_id: str, retry_after_seconds: float) -> None:
        retry_delay = max(
            retry_after_seconds,
            constant.JOB_LEASE_MIN_RETRY_INTERVAL_SECONDS,
        )
        self._lease_retry_at[job_id] = asyncio.get_running_loop().time() + retry_delay

    def _enqueue_due_lease_retries(self) -> None:
        now = asyncio.get_running_loop().time()
        for job_id, retry_at in list(self._lease_retry_at.items()):
            if retry_at > now or job_id in self._job_execution_tasks:
                continue
            del self._lease_retry_at[job_id]
            self.append_job(job_id)

    def _idle_wait_seconds(self) -> float:
        if not self._lease_retry_at:
            return self._idle_interval
        next_retry_at = min(self._lease_retry_at.values())
        retry_wait = max(0.0, next_retry_at - asyncio.get_running_loop().time())
        if self._idle_interval <= 0:
            return retry_wait
        return min(self._idle_interval, retry_wait)

    async def _release_job_lease(self, job_id: str) -> None:
        if self._job_lease is None:
            return
        try:
            await self._job_lease.release(job_id)
        except Exception as e:
            logger.warning(
                "Failed to release job lease",
                job_id=job_id,
                error=str(e),
            )  # type: ignore[call-arg]

    async def _admit_with_lease(
        self,
        job_id: str,
    ) -> tuple[Optional["JobDriver"], bool]:
        if self._job_lease is None:
            return await self._job_progress_manager.admit(job_id), False

        lease_lost = asyncio.Event()
        admission_task = asyncio.create_task(self._refresh_and_admit(job_id))
        lease_renewal_task = asyncio.create_task(
            self._renew_job_lease(job_id, admission_task, lease_lost)
        )
        try:
            try:
                driver = await admission_task
                return driver, lease_lost.is_set()
            except asyncio.CancelledError:
                if lease_lost.is_set():
                    return None, True
                raise
        finally:
            lease_renewal_task.cancel()
            await asyncio.gather(lease_renewal_task, return_exceptions=True)

    async def _refresh_and_admit(self, job_id: str) -> Optional["JobDriver"]:
        await self._job_progress_manager.refresh_job(job_id)
        return await self._job_progress_manager.admit(job_id)

    async def schedule_next_job(self) -> Optional[Tuple[str, "JobDriver"]]:
        # Pick the next job in policy order and admit it (skipping expired /
        # un-admittable jobs). Returns the admitted (job_id, driver) for the
        # loop to dispatch, or None if the queue drains.
        self._enqueue_due_lease_retries()
        if self._policy.empty():
            logger.debug("Job scheduler is waiting jobs coming")
            await asyncio.sleep(self._idle_wait_seconds())
            self._enqueue_due_lease_retries()

        job_id = self._next_job()
        while job_id is not None:
            if self._job_lease is not None:
                acquisition = await self._job_lease.acquire(job_id)
                if not acquisition.acquired:
                    self._defer_job_for_lease(
                        job_id,
                        acquisition.retry_after_seconds,
                    )
                    logger.debug(
                        "Job lease is owned by another scheduler",
                        job_id=job_id,
                        retry_after_seconds=acquisition.retry_after_seconds,
                    )  # type: ignore[call-arg]
                    job_id = self._next_job()
                    continue

            logger.info("Job scheduler is scheduling job", job_id=job_id)  # type: ignore[call-arg]
            try:
                driver, lease_lost = await self._admit_with_lease(job_id)
            except BaseException:
                await self._release_job_lease(job_id)
                raise
            if lease_lost:
                self._defer_job_for_lease(job_id, self._idle_interval)
                job_id = self._next_job()
                continue
            if driver is not None:
                return (job_id, driver)
            await self._release_job_lease(job_id)
            # admit() returns None for an expired / no-longer-pending job, so an
            # expired job is skipped here without a separate "inactive" set.
            logger.warning(
                "Scheduler skipped job: admit() declined (expired or no longer pending)",
                job_id=job_id,
            )  # type: ignore[call-arg]
            job_id = self._next_job()

        return None

    async def expire_jobs(self):
        # Derive past-due jobs from schedulable pools rather than a duplicated
        # due-time list. The deadline is recomputed per job:
        # created_at + completion_window.
        now = time.time()
        seen_job_ids = set()
        schedulable_jobs = list(await self._job_progress_manager.list_pending())
        schedulable_jobs.extend(await self._job_progress_manager.list_in_progress())
        for job in schedulable_jobs:
            if job.job_id in seen_job_ids:
                continue
            seen_job_ids.add(job.job_id)
            # FINALIZING jobs are already on the terminalization path, so the
            # cleanup loop should stop retrying expiry checks for them.
            if job.status.state == BatchJobState.FINALIZING:
                continue
            created_at = job.status.created_at
            if created_at is None:
                continue
            due_time = created_at.timestamp() + job.spec.completion_window
            if due_time <= now:
                if self._job_lease is not None:
                    try:
                        acquisition = await self._job_lease.acquire(job.job_id)
                    except Exception as e:
                        logger.warning(
                            "Failed to acquire job lease for expiry",
                            job_id=job.job_id,
                            error=str(e),
                        )  # type: ignore[call-arg]
                        continue
                    if not acquisition.acquired:
                        continue
                # A non-running expiry lease is intentionally left to its TTL.
                # Peer registries need one refresh cycle to observe the
                # terminal state before the job becomes claimable again.
                await self._job_progress_manager.expire_job(job.job_id)

    async def start(self):
        self._serve_loop = asyncio.get_running_loop()
        logger.info("in start")
        self._jobs_running_task = self._serve_loop.create_task(self.jobs_running_loop())
        logger.info("running loop set up")
        self._jobs_cleanup_task = self._serve_loop.create_task(self.jobs_cleanup_loop())
        logger.info("cleanup loop set up")

    async def _execute_scheduled_job(
        self, job_id: str, job_driver: "JobDriver"
    ) -> None:
        lease_lost = asyncio.Event()
        lease_renewal_task: Optional[asyncio.Task[None]] = None
        if self._job_lease is not None:
            execution_task = asyncio.current_task()
            assert execution_task is not None
            lease_renewal_task = asyncio.create_task(
                self._renew_job_lease(job_id, execution_task, lease_lost)
            )
        try:
            await job_driver.execute(job_id)
        except asyncio.CancelledError:
            raise
        except Exception as e:
            # Since _job_progress_manager(SchedulableJobs) not support mark_job_failed anymore,
            # It is expected that job_driver mark_job_failed using RunningJobs protocol
            # The exception here is for guardian purpose and should not be reached.
            logger.error(
                "Failed to execute job",
                job_id=job_id,
                error=str(e),
            )  # type: ignore[call-arg]
        finally:
            if lease_renewal_task is not None:
                lease_renewal_task.cancel()
                await asyncio.gather(lease_renewal_task, return_exceptions=True)
            await self._release_job_lease(job_id)
            if lease_lost.is_set():
                self._defer_job_for_lease(job_id, self._idle_interval)

    async def _renew_job_lease(
        self,
        job_id: str,
        execution_task: asyncio.Task,
        lease_lost: asyncio.Event,
    ) -> None:
        assert self._job_lease is not None
        loop = asyncio.get_running_loop()
        safety_buffer = (
            self._job_lease.ttl_seconds * constant.JOB_LEASE_DEADLINE_SAFETY_RATIO
        )
        local_lease_duration = self._job_lease.ttl_seconds - safety_buffer
        lease_deadline = loop.time() + local_lease_duration
        next_renewal = loop.time() + self._job_lease.renew_interval_seconds

        while True:
            wake_at = min(lease_deadline, next_renewal)
            await asyncio.sleep(max(0.0, wake_at - loop.time()))
            if loop.time() >= lease_deadline:
                logger.error("Job lease expired", job_id=job_id)  # type: ignore[call-arg]
                lease_lost.set()
                execution_task.cancel()
                return

            try:
                renewed = await self._job_lease.renew(job_id)
            except Exception as e:
                logger.warning(
                    "Failed to renew job lease; retrying before expiry",
                    job_id=job_id,
                    error=str(e),
                )  # type: ignore[call-arg]
                next_renewal = min(
                    lease_deadline,
                    loop.time() + constant.JOB_LEASE_RENEW_RETRY_INTERVAL_SECONDS,
                )
                continue

            if not renewed:
                logger.error("Job lease ownership lost", job_id=job_id)  # type: ignore[call-arg]
                lease_lost.set()
                execution_task.cancel()
                return

            lease_deadline = loop.time() + local_lease_duration
            next_renewal = loop.time() + self._job_lease.renew_interval_seconds

    def _prune_execution_tasks(self) -> None:
        finished_job_ids = [
            job_id for job_id, task in self._job_execution_tasks.items() if task.done()
        ]
        for job_id in finished_job_ids:
            del self._job_execution_tasks[job_id]

    async def jobs_running_loop(self) -> None:
        """Pop the next due, valid job (FIFO) and dispatch up to pool_size."""
        if self._serve_loop is None:
            self._serve_loop = asyncio.get_running_loop()
        logger.info("Starting scheduling...")
        while True:
            self._prune_execution_tasks()
            if len(self._job_execution_tasks) >= self._pool_size:
                await asyncio.wait(
                    self._job_execution_tasks.values(),
                    return_when=asyncio.FIRST_COMPLETED,
                )
                continue

            scheduled: Optional[Tuple[str, "JobDriver"]] = None
            try:
                scheduled = await self.schedule_next_job()
            except Exception as e:
                logger.error(
                    "Failed to schedule job",
                    error=str(e),
                )  # type: ignore[call-arg]

            if scheduled:
                one_job, job_driver = scheduled
                task = self._serve_loop.create_task(
                    self._execute_scheduled_job(one_job, job_driver)
                )
                self._job_execution_tasks[one_job] = task
            # yield loop
            await asyncio.sleep(0)

    async def jobs_cleanup_loop(self):
        """
        This is a long-running process to check if jobs have expired or not.
        """
        while True:
            start_time = time.time()  # Record start time
            await self.expire_jobs()  # Run the process
            elapsed_time = time.time() - start_time  # Calculate elapsed time
            time_to_next_run = max(
                0, self._expire_interval - elapsed_time
            )  # Calculate remaining time
            await asyncio.sleep(time_to_next_run)  # Wait for the remaining time

    async def stop(self):
        """Properly shutdown the driver and cancel running tasks"""
        if self._serve_loop is None:
            # never started; nothing to stop
            return
        assert self._serve_loop == asyncio.get_running_loop()
        # Cancel running loop
        if self._jobs_running_task is not None:
            if not self._jobs_running_task.done():
                self._jobs_running_task.cancel()
            # wait _jobs_running_task for capturing any exception
            try:
                await self._jobs_running_task
            except asyncio.CancelledError:
                pass
        # Cancel cleanup loop
        if self._jobs_cleanup_task is not None and not self._jobs_cleanup_task.done():
            self._jobs_cleanup_task.cancel()
            try:
                await self._jobs_cleanup_task
            except asyncio.CancelledError:
                pass

        running_tasks = list(self._job_execution_tasks.values())
        for task in running_tasks:
            if not task.done():
                task.cancel()
        if running_tasks:
            await asyncio.gather(*running_tasks, return_exceptions=True)
        self._job_execution_tasks.clear()

    def reset_runtime_state(self) -> None:
        self._policy.reset_runtime_state()
        self._job_execution_tasks.clear()
        self._queued_job_ids.clear()
        self._lease_retry_at.clear()
