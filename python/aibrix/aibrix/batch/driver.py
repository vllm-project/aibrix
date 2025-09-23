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
from typing import Any, Dict, List, Optional

import aibrix.batch.storage as _storage
from aibrix.batch.constant import DEFAULT_JOB_POOL_SIZE
from aibrix.batch.job_entity import JobEntityManager
from aibrix.batch.job_manager import JobManager
from aibrix.batch.request_proxy import RequestProxy
from aibrix.batch.scheduler import JobScheduler
from aibrix.batch.storage.batch_metastore import initialize_batch_metastore
from aibrix.metadata.logger import init_logger
from aibrix.storage import StorageType

logger = init_logger(__name__)


class BatchDriver:
    def __init__(
        self,
        job_entity_manager: Optional[JobEntityManager] = None,
        storage_type: StorageType = StorageType.AUTO,
        metastore_type: StorageType = StorageType.AUTO,
    ):
        """
        This is main entrance to bind all components to serve job requests.
        """
        _storage.initialize_storage(storage_type)
        initialize_batch_metastore(metastore_type)
        self._storage = _storage
        self._job_manager: JobManager = JobManager(job_entity_manager)
        self._scheduler: Optional[JobScheduler] = None
        self._scheduling_task: Optional[asyncio.Task] = None
        self._proxy: RequestProxy = RequestProxy(self._job_manager)
        # Only create jobs_running_loop if JobEntityManager does not have its own sched
        if not job_entity_manager or not job_entity_manager.is_scheduler_enabled():
            self._scheduler = JobScheduler(self._job_manager, DEFAULT_JOB_POOL_SIZE)
            self._job_manager.set_scheduler(self._scheduler)
            self._scheduling_task = asyncio.create_task(self.jobs_running_loop())

    @property
    def job_manager(self) -> JobManager:
        return self._job_manager

    async def upload_job_data(self, input_file_name) -> str:
        return await self._storage.upload_input_data(input_file_name)

    async def retrieve_job_result(self, file_id) -> List[Dict[str, Any]]:
        return await self._storage.download_output_data(file_id)

    async def jobs_running_loop(self):
        """
        This loop is going through all active jobs in scheduler.
        For now, the executing unit is one request. Later if necessary,
        we can support a batch size of request per execution.
        """
        logger.info("Starting scheduling...")
        while True:
            one_job = await self._scheduler.round_robin_get_job()
            if one_job:
                try:
                    await self._proxy.execute_queries(one_job)
                except Exception as e:
                    job = self._job_manager.mark_job_failed(one_job)
                    logger.error(
                        "Failed to execute job",
                        job_id=one_job,
                        status=job.status.state.value,
                        error=e,
                    )
                    raise
            await asyncio.sleep(0)

    async def close(self):
        """Properly shutdown the driver and cancel running tasks"""
        if self._scheduling_task and not self._scheduling_task.done():
            self._scheduling_task.cancel()
            try:
                await self._scheduling_task
            except (asyncio.CancelledError, RuntimeError) as e:
                if isinstance(e, RuntimeError) and "different loop" in str(e):
                    logger.warning(
                        "Task cancellation from different event loop, forcing cancellation"
                    )
                pass
        if self._scheduler:
            await self._scheduler.close()

    async def clear_job(self, job_id):
        job = self._job_manager.get_job(job_id)
        if job is None:
            return

        self._job_manager.job_deleted_handler(job)
        if self._job_manager.get_job(job_id) is None:
            await self._storage.remove_job_data(job.spec.input_file_id)
            if job.status.output_file_id is not None:
                await self._storage.remove_job_data(job.status.output_file_id)
            if job.status.error_file_id is not None:
                await self._storage.remove_job_data(job.status.error_file_id)
