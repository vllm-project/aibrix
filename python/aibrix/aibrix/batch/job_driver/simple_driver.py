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

from typing import Optional

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobState,
    JobEntityManager,
)
from aibrix.logger import init_logger

from .local_driver import LocalJobDriver
from .progress_manager import JobProgressManager

logger = init_logger(__name__)


class simpleJobDriver(LocalJobDriver):
    def __init__(
        self,
        progress_manager: JobProgressManager,
        entity_manager: JobEntityManager,
    ) -> None:
        """
        SimpleJobDriver drives job progress by:
        1. Prepare job output files and set k8s job ready.
        2. Wait for k8s job to complete.
        3. Finalize job outputs and errors.
        """
        super().__init__(progress_manager)
        self._entity_manager = entity_manager
        self._mgr_updated_handler = entity_manager.on_job_updated(
            self._job_updated_handler
        )

    async def execute_job(self, job_id):
        """
        Execute complete job workflow: prepare -> execute -> finalize.
        This function executes all three steps.
        """
        job = await self._progress_manager.get_job(job_id)
        if job is None:
            logger.warning("Job not found", job_id=job_id)
            return

        # Check if temp file IDs exist to determine if we should skip steps 1 and 3
        has_temp_files = (
            job.status.temp_output_file_id and job.status.temp_error_file_id
        )

        try:
            if not has_temp_files:
                # Step 1: Prepare job output files
                logger.debug("Temp files not created, creating...", job_id=job_id)
                job = await self.prepare_job(job)

            logger.info(
                "Job preparation completed, files ready",
                job_id=job_id,
                output_file_id=job.status.output_file_id,
                temp_output_file_id=job.status.temp_output_file_id,
                error_file_id=job.status.error_file_id,
                temp_error_file_id=job.status.temp_error_file_id,
            )  # type: ignore[call-arg]

            await self._entity_manager.update_job_ready(job)
        except Exception as e:
            logger.error("Job preparation failed", job_id=job_id, exc_info=True)  # type: ignore[call-arg]
            await self._progress_manager.mark_job_failed(
                job_id,
                BatchJobError(
                    code=BatchJobErrorCode.PREPARE_OUTPUT_ERROR, message=str(e)
                ),
            )
            # No need to stop job because only update_job_ready will start job.

    async def job_updated_handler(self, old_job: BatchJob, new_job: BatchJob) -> bool:
        # Mark post-state-transition flags
        finalizing_needed = (
            new_job.status.state == BatchJobState.FINALIZING
            and old_job.status.state != BatchJobState.FINALIZING
        )
        job_id = old_job.job_id

        if (
            self._mgr_updated_handler is not None
            and not await self._mgr_updated_handler(old_job, new_job)
        ):
            return False

        # The transition is valid. finalize job when transitioning to FINALIZING
        if finalizing_needed:
            logger.debug(
                "job_updated_handler in SimpleJobDriver",
                old_state=old_job.status.state.value,
                new_state=BatchJobState.FINALIZING.value,
                finalizing_needed=finalizing_needed,
            )  # type: ignore[call-arg]
            try:
                logger.info("Starting job finalization", job_id=job_id)  # type: ignore[call-arg]
                await self.finalize_job(new_job)
            except Exception as fe:
                logger.error("Job finalization failed", job_id=job_id, exc_info=True)  # type: ignore[call-arg]
                if job_id is not None:
                    await self._progress_manager.mark_job_failed(
                        job_id,
                        BatchJobError(
                            code=BatchJobErrorCode.FINALIZING_ERROR, message=str(fe)
                        ),
                    )
        return True

    async def _job_updated_handler(self, old_job: BatchJob, new_job: BatchJob) -> bool:
        return await self.job_updated_handler(old_job, new_job)


_simple_job_driver: Optional[simpleJobDriver] = None


def SimpleJobDriver(
    progress_manager: JobProgressManager, entity_manager: JobEntityManager
) -> simpleJobDriver:
    global _simple_job_driver

    if _simple_job_driver is None:
        _simple_job_driver = simpleJobDriver(progress_manager, entity_manager)
    return _simple_job_driver
