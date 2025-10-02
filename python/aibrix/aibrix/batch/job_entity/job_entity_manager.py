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
from typing import Any, Callable, Coroutine, Optional

from aibrix.batch.job_entity.batch_job import BatchJob, BatchJobSpec
from aibrix.logger import init_logger

logger = init_logger(__name__)


class JobEntityManager(ABC):
    """
    This is an abstract class.

    A storage should implement this class, such as Local files, TOS and S3.
    Any storage implementation are transparent to external components.
    """

    def on_job_committed(
        self, handler: Callable[[BatchJob], Coroutine[Any, Any, bool]]
    ):
        """Register a job committed callback.

        Args:
            handler: (async Callable[[BatchJob], bool])
                The callback function. It should accept a single `BatchJob` object
                representing the committed job and return `None`.
        """
        self._job_committed_loop = asyncio.get_running_loop()
        logger.debug(
            "job committed handler registered",
            loop=getattr(self._job_committed_loop, "name", "unknown"),
        )  # type: ignore[call-arg]
        self._job_committed_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = handler

    async def job_committed(self, committed: BatchJob) -> bool:
        if self._job_committed_handler is None:
            return True

        return await asyncio.wrap_future(
            asyncio.run_coroutine_threadsafe(
                self._job_committed_handler(committed), self._job_committed_loop
            )
        )

    def on_job_updated(
        self, handler: Callable[[BatchJob, BatchJob], Coroutine[Any, Any, bool]]
    ):
        """Register a job updated callback.

        Args:
            handler: (async Callable[[BatchJob, BatchJob], bool])
                The callback function. It should accept two `BatchJob` objects
                representing the old job and new job and return `None`.
                Example: `lambda old_job, new_job: logger.info("Job updated", old_id=old_job.id, new_id=new_job.id)`
        """
        self._job_updated_loop = asyncio.get_running_loop()
        logger.debug(
            "job updated handler registered",
            loop=getattr(self._job_updated_loop, "name", "unknown"),
        )  # type: ignore[call-arg]
        self._job_updated_handler: Optional[
            Callable[[BatchJob, BatchJob], Coroutine[Any, Any, bool]]
        ] = handler

    async def job_updated(self, old: BatchJob, new: BatchJob) -> bool:
        if self._job_updated_handler is None:
            return True

        return await asyncio.wrap_future(
            asyncio.run_coroutine_threadsafe(
                self._job_updated_handler(old, new), self._job_updated_loop
            )
        )

    def on_job_deleted(self, handler: Callable[[BatchJob], Coroutine[Any, Any, bool]]):
        """Register a job deleted callback.

        Args:
            handler: (async Callable[[BatchJob], bool])
                The callback function. It should accept a single `BatchJob` object
                representing the deleted job and return `None`.
                Example: `lambda deleted_job: logger.info("Job deleted", job_id=deleted_job.id)`
        """
        self._job_deleted_loop = asyncio.get_running_loop()
        logger.debug(
            "job deleted handler registered",
            loop=getattr(self._job_deleted_loop, "name", "unknown"),
        )  # type: ignore[call-arg]
        self._job_deleted_handler: Optional[
            Callable[[BatchJob], Coroutine[Any, Any, bool]]
        ] = handler

    async def job_deleted(self, deleted: BatchJob) -> bool:
        if self._job_deleted_handler is None:
            return True

        return await asyncio.wrap_future(
            asyncio.run_coroutine_threadsafe(
                self._job_deleted_handler(deleted), self._job_deleted_loop
            )
        )

    def is_scheduler_enabled(self) -> bool:
        """Check if JobEntityManager has own scheduler enabled."""
        return False

    @abstractmethod
    async def submit_job(self, session_id: str, job: BatchJobSpec):
        """Submit job by submiting job to the persist store.

        Args:
            session_id (str): id identifiy the job submission sesstion
            job (BatchJob): Job to add.
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
    def get_job(self, job_id: str) -> Optional[BatchJob]:
        """Get cached job detail by batch id.

        Args:
            str (str): Batch id.

        Returns:
            BatchJob: Job detail.
        """
        pass

    @abstractmethod
    def list_jobs(self) -> list[BatchJob]:
        """List unarchived jobs that cached locally.

        Returns:
            list[BatchJob]: List of jobs.
        """
        pass
