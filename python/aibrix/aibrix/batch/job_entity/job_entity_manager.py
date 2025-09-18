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
from abc import ABC, abstractmethod
from typing import Callable, Optional

from aibrix.batch.job_entity.batch_job import BatchJob, BatchJobSpec


class JobEntityManager(ABC):
    """
    This is an abstract class.

    A storage should implement this class, such as Local files, TOS and S3.
    Any storage implementation are transparent to external components.
    """

    def on_job_committed(self, handler: Callable[[BatchJob], None]):
        """Register a job committed callback.

        Args:
            handler: (Callable[[BatchJob], None])
                The callback function. It should accept a single `BatchJob` object
                representing the committed job and return `None`.
        """
        self._job_committed_handler: Optional[Callable[[BatchJob], None]] = handler

    def on_job_updated(self, handler: Callable[[BatchJob, BatchJob], None]):
        """Register a job updated callback.

        Args:
            handler: (Callable[[BatchJob, BatchJob], None])
                The callback function. It should accept two `BatchJob` objects
                representing the old job and new job and return `None`.
                Example: `lambda old_job, new_job: logger.info("Job updated", old_id=old_job.id, new_id=new_job.id)`
        """
        self._job_updated_handler: Optional[Callable[[BatchJob, BatchJob], None]] = (
            handler
        )

    def on_job_deleted(self, handler: Callable[[BatchJob], None]):
        """Register a job deleted callback.

        Args:
            handler: (Callable[[BatchJob], None])
                The callback function. It should accept a single `BatchJob` object
                representing the deleted job and return `None`.
                Example: `lambda deleted_job: logger.info("Job deleted", job_id=deleted_job.id)`
        """
        self._job_deleted_handler: Optional[Callable[[BatchJob], None]] = handler

    def is_scheduler_enabled(self) -> bool:
        """Check if JobEntityManager has own scheduler enabled."""
        return False

    @abstractmethod
    def submit_job(self, session_id: str, job: BatchJobSpec):
        """Submit job by submiting job to the persist store.

        Args:
            job (BatchJob): Job to add.
        """
        pass

    @abstractmethod
    def cancel_job(self, job_id: str):
        """Cancel job by notifing the persist store.

        Args:
            job_id (str): Job id.
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
