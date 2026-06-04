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
from abc import ABC, abstractmethod
from datetime import datetime
from typing import Any, Callable, Coroutine, Optional

from aibrix.batch.job_entity.batch_job import BatchJob, BatchJobSpec
from aibrix.logger import init_logger

logger = init_logger(__name__)


class JobEntityManager(ABC):
    DEFAULT_JOB_PAGE_LIMIT = 20
    """
    This is an abstract class.
    
    A batch-job persistence backend: a *command store* and an *event source*
    fused into one port (the BatchManager binds to it via EntityManagerBridge).

    The two halves point in OPPOSITE directions — keep them apart when reading:

    * Command store (manager -> store) — the abstract contract each backend
      implements: ``submit_job`` / ``update_job_ready`` / ``update_job_status``
      / ``cancel_job`` / ``delete_job`` / ``get_job`` / ``list_jobs``. The
      manager *calls* these to mutate/read persisted jobs.
    * Event source (store -> manager) — the provided pub/sub machinery below:
      the owner *subscribes* with ``on_job_committed/updated/deleted``; the
      backend *publishes* by calling ``job_committed/updated/deleted``. Publish
      is thread-safe — it marshals the callback onto the subscriber's loop via
      ``run_coroutine_threadsafe`` — because a backend may fire events from
      another thread (e.g. a backend that watches an external store).

    """

    # --- Event source (store -> manager): provided thread-safe pub/sub bus.
    #     Subscribe via on_*; the backend publishes via job_* (marshaled onto the
    #     subscriber's loop, since a backend may fire from another thread). ---
    def __init__(self):
        self.active_jobs: dict[str, BatchJob] = {}
        self._job_committed_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._job_committed_loop: Optional[asyncio.AbstractEventLoop] = None
        self._job_updated_handler: Optional[
            Callable[[BatchJob, BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._job_updated_loop: Optional[asyncio.AbstractEventLoop] = None
        self._job_deleted_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = None
        self._job_deleted_loop: Optional[asyncio.AbstractEventLoop] = None
        self._monitored_job_snapshots: dict[str, BatchJob] = {}
        self._refresh_task: Optional[asyncio.Task[None]] = None
        self._refresh_interval_seconds = 1.0

    def on_job_committed(
        self,
        handler: Callable[[BatchJob], Coroutine[Any, Any, bool]],
        update_running_loop: bool = True,
    ) -> Optional[Callable[[BatchJob], Coroutine[Any, Any, bool]]]:
        """Register a job committed callback.

        Args:
            handler: (async Callable[[BatchJob], bool])
                The callback function. It should accept a single `BatchJob` object
                representing the committed job and return `None`.
        """
        # Keeps the loop reference to the first registration.
        # Otherwise, it will be overwritten by the next registration.
        if self._job_committed_loop is None or update_running_loop:
            self._job_committed_loop = asyncio.get_running_loop()
        logger.debug(
            "job committed handler registered",
            loop=getattr(self._job_committed_loop, "name", "unknown"),
        )  # type: ignore[call-arg]
        old_handler = self._job_committed_handler
        self._job_committed_handler = handler
        return old_handler

    async def job_committed(self, committed: BatchJob) -> bool:
        success = True
        if self._job_committed_handler is not None:
            if self._job_committed_loop is None:
                raise RuntimeError("job committed handler loop is not initialized")
            success = await asyncio.wrap_future(
                asyncio.run_coroutine_threadsafe(
                    self._job_committed_handler(committed), self._job_committed_loop
                )
            )
        if success:
            self._remember_job(committed)
            self._sync_active_job(committed)
        return success

    def on_job_updated(
        self,
        handler: Callable[[BatchJob, BatchJob], Coroutine[Any, Any, bool]],
        update_running_loop: bool = True,
    ) -> Optional[Callable[[BatchJob, BatchJob], Coroutine[Any, Any, bool]]]:
        """Register a job updated callback.

        Args:
            handler: (async Callable[[BatchJob, BatchJob], bool])
                The callback function. It should accept two `BatchJob` objects
                representing the old job and new job and return `None`.
                Example: `lambda old_job, new_job: logger.info("Job updated", old_id=old_job.id, new_id=new_job.id)`
        """
        # Keeps the loop reference to the first registration.
        # Otherwise, it will be overwritten by the next registration.
        if self._job_committed_loop is None or update_running_loop:
            self._job_updated_loop = asyncio.get_running_loop()
        logger.debug(
            "job updated handler registered",
            loop=getattr(self._job_updated_loop, "name", "unknown"),
        )  # type: ignore[call-arg]
        old_handler = self._job_updated_handler
        self._job_updated_handler = handler
        return old_handler

    async def job_updated(self, old: BatchJob, new: BatchJob) -> bool:
        success = True
        if self._job_updated_handler is not None:
            if self._job_updated_loop is None:
                raise RuntimeError("job updated handler loop is not initialized")
            success = await asyncio.wrap_future(
                asyncio.run_coroutine_threadsafe(
                    self._job_updated_handler(old, new), self._job_updated_loop
                )
            )
        if success:
            self._remember_job(new)
            self._sync_active_job(new)
        return success

    def on_job_deleted(
        self,
        handler: Callable[[BatchJob], Coroutine[Any, Any, bool]],
        update_running_loop: bool = True,
    ) -> Optional[Callable[[BatchJob], Coroutine[Any, Any, bool]]]:
        """Register a job deleted callback.

        Args:
            handler: (async Callable[[BatchJob], bool])
                The callback function. It should accept a single `BatchJob` object
                representing the deleted job and return `None`.
                Example: `lambda deleted_job: logger.info("Job deleted", job_id=deleted_job.id)`
        """
        # Keeps the loop reference to the first registration.
        # Otherwise, it will be overwritten by the next registration.
        if self._job_committed_loop is None or update_running_loop:
            self._job_deleted_loop = asyncio.get_running_loop()
        logger.debug(
            "job deleted handler registered",
            loop=getattr(self._job_deleted_loop, "name", "unknown"),
        )  # type: ignore[call-arg]
        old_handler = self._job_deleted_handler
        self._job_deleted_handler = handler
        return old_handler

    async def job_deleted(self, deleted: BatchJob) -> bool:
        success = True
        if self._job_deleted_handler is not None:
            if self._job_deleted_loop is None:
                raise RuntimeError("job deleted handler loop is not initialized")
            success = await asyncio.wrap_future(
                asyncio.run_coroutine_threadsafe(
                    self._job_deleted_handler(deleted), self._job_deleted_loop
                )
            )
        if success:
            self._forget_job(deleted.job_id)
            self._remove_active_job(deleted.job_id)
        return success

    def is_scheduler_enabled(self) -> bool:
        """Check if JobEntityManager has own scheduler enabled."""
        return False

    async def start(self) -> None:
        if self._refresh_task is not None:
            return
        await self._bootstrap_jobs()
        self._refresh_task = asyncio.create_task(self._refresh_loop())

    async def stop(self) -> None:
        if self._refresh_task is None:
            return
        self._refresh_task.cancel()
        try:
            await self._refresh_task
        except asyncio.CancelledError:
            pass
        self._refresh_task = None

    async def refresh(self) -> None:
        jobs = await self._list_recovery_jobs()
        current_jobs: dict[str, BatchJob] = {}
        for job in jobs:
            job_id = job.job_id
            if job_id is None:
                continue
            snapshot = job.model_copy(deep=True)
            current_jobs[job_id] = snapshot
            old_job = self._monitored_job_snapshots.get(job_id)
            if old_job is None:
                self._monitored_job_snapshots[job_id] = snapshot
                if not snapshot.status.finished:
                    await self.job_committed(snapshot)
                continue
            if self._jobs_equal(old_job, snapshot):
                self._monitored_job_snapshots[job_id] = snapshot
                continue
            self._monitored_job_snapshots[job_id] = snapshot
            await self.job_updated(old_job, snapshot)

        deleted_job_ids = set(self._monitored_job_snapshots) - set(current_jobs)
        for job_id in deleted_job_ids:
            previous_job = self._monitored_job_snapshots.get(job_id)
            if previous_job is None:
                continue
            latest_job = await self.get_job(job_id)
            if latest_job is None:
                await self.job_deleted(previous_job)
                continue
            if self._jobs_equal(previous_job, latest_job):
                self._forget_job(job_id)
                self._sync_active_job(latest_job)
                continue
            await self.job_updated(previous_job, latest_job)

    async def _bootstrap_jobs(self) -> None:
        for job in await self._list_recovery_jobs():
            job_id = job.job_id
            if job_id is None:
                continue
            snapshot = job.model_copy(deep=True)
            self._monitored_job_snapshots[job_id] = snapshot
            if snapshot.status.finished:
                continue
            await self.job_committed(snapshot)

    async def _refresh_loop(self) -> None:
        while True:
            try:
                await asyncio.sleep(self._refresh_interval_seconds)
                await self.refresh()
            except asyncio.CancelledError:
                raise
            except Exception:
                logger.error("job entity manager refresh failed", exc_info=True)  # type: ignore[call-arg]

    async def _list_recovery_jobs(self) -> list[BatchJob]:
        return await self._list_jobs_for_recovery(None)

    async def _list_jobs_for_recovery(
        self, oldest_unfinished_created_at: Optional[datetime]
    ) -> list[BatchJob]:
        recovered_jobs: list[BatchJob] = []
        after: Optional[str] = None
        while True:
            jobs = await self.list_jobs(after=after, limit=self.DEFAULT_JOB_PAGE_LIMIT)
            if not jobs:
                return recovered_jobs
            for job in jobs:
                if job.job_id is not None and not job.status.finished:
                    recovered_jobs.append(job)
            last_job = jobs[-1]
            last_job_id = last_job.job_id or last_job.status.job_id
            last_created_at = last_job.status.created_at
            if last_job_id is None or len(jobs) < self.DEFAULT_JOB_PAGE_LIMIT:
                return recovered_jobs
            if (
                oldest_unfinished_created_at is not None
                and last_created_at is not None
                and last_created_at < oldest_unfinished_created_at
            ):
                return recovered_jobs
            after = last_job_id

    # TODO: remove, no manual pagination
    def _paginate_jobs(
        self,
        jobs: list[BatchJob],
        after: Optional[str] = None,
        limit: Optional[int] = None,
    ) -> list[BatchJob]:
        if after:
            after_index = next(
                (
                    index
                    for index, job in enumerate(jobs)
                    if job.job_id == after or job.status.job_id == after
                ),
                -1,
            )
            if after_index < 0:
                return []
            jobs = jobs[after_index + 1 :]
        if limit is not None:
            jobs = jobs[:limit]
        return jobs

    def _remember_job(self, job: BatchJob) -> None:
        if job.job_id is None:
            return
        if job.status.finished:
            self._monitored_job_snapshots.pop(job.job_id, None)
            return
        self._monitored_job_snapshots[job.job_id] = job.model_copy(deep=True)

    def _forget_job(self, job_id: Optional[str]) -> None:
        if job_id is None:
            return
        self._monitored_job_snapshots.pop(job_id, None)

    def _sync_active_job(self, job: BatchJob) -> None:
        job_id = job.job_id
        if job_id is None:
            return
        if job.status.finished:
            self.active_jobs.pop(job_id, None)
            return
        self.active_jobs[job_id] = job

    def _remove_active_job(self, job_id: Optional[str]) -> None:
        if job_id is None:
            return
        self.active_jobs.pop(job_id, None)

    def _jobs_equal(self, old_job: BatchJob, new_job: BatchJob) -> bool:
        return old_job.model_dump(
            mode="json", by_alias=True, exclude_none=True
        ) == new_job.model_dump(mode="json", by_alias=True, exclude_none=True)

    # --- Command store (manager -> store): the abstract contract each backend
    #     implements. The manager calls these to mutate/read persisted jobs. ---
    @abstractmethod
    async def submit_job(
        self, session_id: str, job: BatchJobSpec, request_count: int = 0
    ):
        """Submit job by submiting job to the persist store.

        Args:
            session_id (str): id identifiy the job submission sesstion
            job (BatchJob): Job to add.
            request_count (int): validated input line count; pre-seeds
                request_counts.total so it is fixed at creation.
        """
        pass

    @abstractmethod
    async def update_job_ready(self, job: BatchJob):
        """Update job by marking job ready with required information.

        Args:
            job (BatchJob): Job to update.
        """

    @abstractmethod
    async def update_job_status(self, job: BatchJob):
        """Update job status by persisting status information as annotations.

        Args:
            job (BatchJob): Job with updated status to persist.

        This method persists critical job status information including:
        - Finalized state
        - Conditions (completed, failed, cancelled)
        - Request counts
        - Timestamps (completed_at, cancelling_at, etc.)
        """

    @abstractmethod
    async def cancel_job(self, job: BatchJob):
        """Cancel job by notifing the persist store on job cancelling or failure.

        Args:
            job (BatchJob): Job to cancel or failed
        """
        pass

    @abstractmethod
    async def delete_job(self, job: BatchJob):
        """Delete job from the persist store.

        Args:
            job (BatchJob): Job to delete.
        """
        pass

    @abstractmethod
    async def get_job(
        self, job_id: str, force_reload: bool = False
    ) -> Optional[BatchJob]:
        """Get cached job detail by batch id.

        Args:
            str (str): Batch id.

        Returns:
            BatchJob: Job detail.
        """
        pass

    @abstractmethod
    async def list_jobs(
        self,
        after: Optional[str] = None,
        limit: int = DEFAULT_JOB_PAGE_LIMIT,
    ) -> list[BatchJob]:
        """List unarchived jobs that cached locally.

        Returns:
            list[BatchJob]: List of jobs.
        """
        pass
