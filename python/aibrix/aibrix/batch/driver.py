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
from aibrix.batch.batch_manager import BatchManager
from aibrix.batch.batch_scheduler import BatchScheduler
from aibrix.batch.client import EndpointSource
from aibrix.batch.constant import DEFAULT_JOB_POOL_SIZE
from aibrix.batch.job_entity import BatchJob, BatchJobSpec
from aibrix.batch.state import JobEntityManager
from aibrix.context import InfrastructureContext
from aibrix.logger import init_logger
from aibrix.metadata.core import T
from aibrix.storage import StorageType

logger = init_logger(__name__)


class BatchDriver:
    def __init__(
        self,
        context: InfrastructureContext,
        job_entity_manager: Optional[JobEntityManager] = None,
        storage_type: StorageType = StorageType.AUTO,
        metastore_type: StorageType = StorageType.AUTO,
        endpoint_source: Optional[EndpointSource] = None,
        params={},
    ):
        """
        This is main entrance to bind all components to serve job requests.

        """
        # initialize storage and metastore singletons
        _storage.initialize_batch_storage(storage_type, params)
        _storage.initialize_batch_metastore(metastore_type, params)
        self._context = context
        self._storage = _storage
        self._endpoint_source = endpoint_source
        # The manager owns the entity manager from construction; the driver does
        # not keep a duplicate reference. Handler registration is deferred to
        # start() (the entity manager captures the running loop at bind time).
        self._batch_manager: BatchManager = BatchManager(
            self._context, job_entity_manager
        )

        self._scheduler: Optional[BatchScheduler] = BatchScheduler(
            self._context,
            self._batch_manager,
            DEFAULT_JOB_POOL_SIZE,
        )
        self._batch_manager.set_scheduler(self._scheduler)

        # Track jobs with fail_after_n_requests for stop() validation
        self._jobs_with_fail_after: set[str] = set()

        logger.info(
            "Batch driver initialized",
            job_entity_manager=True if job_entity_manager else False,
            job_scheduler=True if self._scheduler else False,
            storage=_storage.get_storage_type().value,
            metastore=_storage.get_metastore_type().value,
        )  # type: ignore[call-arg]

    @property
    def job_manager(self) -> BatchManager:
        return self._batch_manager

    # Job operations: the public facade. Each dispatches the BatchManager call
    # via run_coroutine so callers never reach through .job_manager.
    async def create_job(
        # TODO: request_count needs some refactor? should not be here?
        self,
        session_id: str,
        job_spec: BatchJobSpec,
        request_count: int = 0,
    ) -> str:
        return await self.run_coroutine(
            self._batch_manager.create_job_with_spec(
                session_id=session_id,
                job_spec=job_spec,
                request_count=request_count,
            )
        )

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        return await self.run_coroutine(self._batch_manager.get_job(job_id))

    async def cancel_job(self, job_id: str) -> bool:
        return await self.run_coroutine(self._batch_manager.cancel_job(job_id))

    async def list_jobs(self) -> List[BatchJob]:
        return await self.run_coroutine(self._batch_manager.list_jobs())

    async def job_committed_handler(self, job: BatchJob) -> bool:
        return await self.run_coroutine(self._batch_manager.job_committed_handler(job))

    async def start(self):
        # The batch subsystem runs on the caller's event loop (the metadata
        # service's HTTP loop). Don't set attributes on the loop object — some
        # implementations (e.g. uvloop) reject it; just log for diagnostics.
        logger.info("Starting BatchDriver on the current event loop")

        # Register the entity manager's lifecycle handlers on this loop (no-op
        # if none was configured). Deferred here because the entity manager
        # captures the running loop at handler registration.
        await self.run_coroutine(self._batch_manager.bind_entity_manager())

        if self._scheduler is not None:
            logger.info("starting scheduler")
            # TODO: refactor, job level endpoint source should be removed.
            # The driver dispatches against this endpoint source; inject it into
            # the manager (which builds the driver on admission) rather than
            # threading it through the scheduler.
            self._batch_manager.set_endpoint_source(self._endpoint_source)
            await self.run_coroutine(self._scheduler.start())

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

    async def clear_job(self, job_id):
        """Clear job related data for testing"""
        job = await self._batch_manager.get_job(job_id)
        if job is None:
            return

        if await self._batch_manager.delete_job(job_id):
            tasks = [self._storage.remove_job_data(job.spec.input_file_id)]
            if job.status.output_file_id is not None:
                tasks.append(self._storage.remove_job_data(job.status.output_file_id))
            if job.status.error_file_id is not None:
                tasks.append(self._storage.remove_job_data(job.status.error_file_id))

            await asyncio.gather(*tasks)

    async def run_coroutine(self, coro: Coroutine[Any, Any, T]) -> T:
        """Await a coroutine on the driver's event loop.

        The batch subsystem now runs on the caller's loop, so this is a direct
        await. Kept as the single seam the facade/helpers dispatch through (it
        previously marshaled onto a dedicated thread).
        """
        return await coro
