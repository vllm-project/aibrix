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
from typing import Any, Coroutine, Dict, List, Optional

import aibrix.batch.storage as _storage
from aibrix.batch.constant import DEFAULT_JOB_POOL_SIZE
from aibrix.batch.job_driver import InferenceEngineClient, ProxyInferenceEngineClient
from aibrix.batch.job_entity import JobEntityManager
from aibrix.batch.job_manager import JobManager
from aibrix.batch.scheduler import JobScheduler
from aibrix.batch.storage.batch_metastore import (
    get_metastore_type,
    initialize_batch_metastore,
)
from aibrix.logger import init_logger
from aibrix.metadata.core import AsyncLoopThread, T
from aibrix.storage import StorageType

logger = init_logger(__name__)


class BatchDriver:
    def __init__(
        self,
        job_entity_manager: Optional[JobEntityManager] = None,
        storage_type: StorageType = StorageType.AUTO,
        metastore_type: StorageType = StorageType.AUTO,
        llm_engine_endpoint: Optional[str] = None,
        stand_alone: bool = False,
        params={},
    ):
        """
        This is main entrance to bind all components to serve job requests.

        Args:
            stand_alone: Set to true to start a new thread for job management.
        """
        _storage.initialize_storage(storage_type, params)
        initialize_batch_metastore(metastore_type, params)
        self._async_thread_loop: Optional[AsyncLoopThread] = None
        if stand_alone:
            self._async_thread_loop = AsyncLoopThread("BatchDriver")
        self._storage = _storage
        self._job_entity_manager: Optional[JobEntityManager] = job_entity_manager
        self._job_manager: JobManager = JobManager()
        self._scheduler: Optional[JobScheduler] = None
        # Only initiate scheduler if JobEntityManager does not have its own sched
        if (
            not self._job_entity_manager
            or not self._job_entity_manager.is_scheduler_enabled()
        ):
            self._scheduler = JobScheduler(self._job_manager, DEFAULT_JOB_POOL_SIZE)
            self._job_manager.set_scheduler(self._scheduler)

        # Initialize inference client with optional LLM engine endpoint
        self._inference_client: Optional[InferenceEngineClient] = None
        if llm_engine_endpoint is not None:
            self._inference_client = ProxyInferenceEngineClient(llm_engine_endpoint)

        # Track jobs with fail_after_n_requests for stop() validation
        self._jobs_with_fail_after: set[str] = set()

        logger.info(
            "Batch driver initialized",
            job_entity_manager=True if job_entity_manager else False,
            job_scheduler=True if self._scheduler else False,
            storage=_storage.get_storage_type().value,
            metastore=get_metastore_type().value,
        )  # type: ignore[call-arg]

    @property
    def job_manager(self) -> JobManager:
        return self._job_manager

    async def start(self):
        # Start thread
        if self._async_thread_loop is not None:
            self._async_thread_loop.start()
            logger.info("Batch driver stand alone thread started")  # type: ignore[call-arg]
        else:
            # name the loop
            asyncio.get_running_loop().name = "default"

        if self._job_entity_manager is not None:
            logger.info("Registering job entity manager handlers")
            await self.run_coroutine(
                self.job_manager.set_job_entity_manager(self._job_entity_manager)
            )

        if self._scheduler is not None:
            logger.info("starting scheduler")
            await self.run_coroutine(self._scheduler.start(self._inference_client))

    async def upload_job_data(self, input_file_name) -> str:
        return await self.run_coroutine(
            self._storage.upload_input_data(input_file_name)
        )

    async def retrieve_job_result(self, file_id) -> List[Dict[str, Any]]:
        return await self.run_coroutine(self._storage.download_output_data(file_id))

    async def stop(self):
        """Properly shutdown the driver and cancel running tasks"""
        if self._scheduler is not None:
            await self.run_coroutine(self._scheduler.stop())

        if self._async_thread_loop is not None:
            self._async_thread_loop.stop()
            logger.info("Batch driver stand alone thread stopped")  # type: ignore[call-arg]

    async def clear_job(self, job_id):
        """Clear job related data for testing"""
        if (
            self._async_thread_loop is not None
            and self._async_thread_loop.loop != asyncio.get_running_loop()
        ):
            return await self._async_thread_loop.run_coroutine(self.clear_job(job_id))

        job = await self._job_manager.get_job(job_id)
        if job is None:
            return

        if await self._job_manager.delete_job(job_id):
            tasks = [self._storage.remove_job_data(job.spec.input_file_id)]
            if job.status.output_file_id is not None:
                tasks.append(self._storage.remove_job_data(job.status.output_file_id))
            if job.status.error_file_id is not None:
                tasks.append(self._storage.remove_job_data(job.status.error_file_id))

            await asyncio.gather(*tasks)

    async def run_coroutine(self, coro: Coroutine[Any, Any, T]) -> T:
        """
        Submits a coroutine to the event loop and returns an awaitable Future.
        This method itself MUST be awaited. (For use from async code)
        """
        if self._async_thread_loop is not None:
            return await self._async_thread_loop.run_coroutine(coro)

        return await coro
