# Copyright 2026 The Aibrix Team.
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
"""JobStore: the metastore-backed JobEntityManager.

A single JobEntityManager whose document store is the **batch metastore**
(``aibrix.batch.storage.batch_metastore``).

Role split (see ``JobEntityManager``):
  * command store — document CRUD delegated to the metastore (one source of truth);
  * event source — committed / updated / deleted emitted on writes;
  * recovery — ``_list_recovery_jobs`` reads the metastore on startup.

"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any, List, Optional

from aibrix.batch.job_entity import BatchJob, BatchJobSpec
from aibrix.batch.state.job_entity_manager import JobEntityManager
from aibrix.batch.storage import batch_metastore
from aibrix.batch.storage.batch_metastore import (
    delete_batch_job,
    get_batch_job,
    initialize_batch_metastore,
    list_batch_jobs,
    put_batch_job,
)
from aibrix.metadata.setting import settings
from aibrix.storage.types import StorageListOrdering


class JobStore(JobEntityManager):
    def __init__(
        self,
        storage_type=None,
        params: Optional[dict[str, Any]] = None,
    ) -> None:
        super().__init__()
        self._storage_type = storage_type or settings.METASTORE_TYPE
        self._params = dict(params or {})
        if batch_metastore.p_metastore is None:
            initialize_batch_metastore(self._storage_type, self._params)

    async def get_job(
        self, job_id: str, force_reload: bool = False
    ) -> Optional[BatchJob]:
        if job_id in self.active_jobs and not force_reload:
            return self.active_jobs[job_id]
        job = await get_batch_job(job_id)
        await self._publish_active_job_on_cache_miss(job)
        if job is not None and not job.status.finished:
            return self.active_jobs.get(job_id, job)
        return job

    async def list_jobs(
        self,
        after: Optional[str] = None,
        limit: int = JobEntityManager.DEFAULT_JOB_PAGE_LIMIT,
    ) -> List[BatchJob]:
        return await list_batch_jobs(
            after=after,
            limit=limit,
            cached_job_getter=self.active_jobs.get,
        )

    async def _list_recovery_jobs(self) -> List[BatchJob]:
        return await self._list_jobs_for_recovery(
            await batch_metastore.get_oldest_unfinished_job_created_at()
        )

    def _supports_created_at_desc_recovery_ordering(self) -> bool:
        return (
            batch_metastore.p_metastore is not None
            and batch_metastore.p_metastore.get_list_ordering()
            == StorageListOrdering.CREATED_AT_DESC
        )

    async def submit_job(
        self, session_id: str, job_spec: BatchJobSpec, request_count: int = 0
    ):
        job = BatchJob.new_local(spec=job_spec, request_count=request_count)
        job.session_id = session_id
        stored_job = await self._upsert_job(job, None)
        await self.job_committed(stored_job)

    async def update_job_ready(self, job: BatchJob):
        await self._update_existing_job(job)

    async def update_job_status(self, job: BatchJob):
        await self._update_existing_job(job)

    async def cancel_job(self, job: BatchJob):
        await self._update_existing_job(job)

    async def delete_job(self, job: BatchJob):
        if job.job_id is None:
            raise ValueError("job_id is required")
        existing_job = await self.get_job(job.job_id) or job
        await delete_batch_job(job.job_id)
        await self.job_deleted(existing_job)
        await self._persist_oldest_unfinished_job_created_at(None)

    async def _update_existing_job(self, job: BatchJob) -> None:
        if job.job_id is None:
            raise ValueError("job_id is required")
        old_job = await self.get_job(job.job_id)
        stored_job = await self._upsert_job(job, old_job)
        await self._persist_oldest_unfinished_job_created_at(stored_job)
        if old_job is not None:
            await self.job_updated(old_job, stored_job)
        else:
            self._sync_active_job(stored_job)

    async def _upsert_job(self, job: BatchJob, old_job: Optional[BatchJob]) -> BatchJob:
        stored_job = job.model_copy(deep=True)
        stored_job_id = stored_job.job_id
        stored_job.metadata.resource_version = self._next_resource_version(old_job)
        await put_batch_job(stored_job_id, stored_job)
        return stored_job

    async def _persist_oldest_unfinished_job_created_at(
        self, candidate_job: Optional[BatchJob] = None
    ) -> None:
        await batch_metastore.set_oldest_unfinished_job_created_at(
            self._oldest_unfinished_job_created_at(candidate_job)
        )

    def _oldest_unfinished_job_created_at(
        self, candidate_job: Optional[BatchJob] = None
    ) -> Optional[datetime]:
        created_at_values = [
            job.status.created_at
            for job in self.active_jobs.values()
            if not job.status.finished
            and job.status.created_at is not None
            and (candidate_job is None or job.job_id != candidate_job.job_id)
        ]
        if (
            candidate_job is not None
            and not candidate_job.status.finished
            and candidate_job.status.created_at is not None
        ):
            created_at_values.append(candidate_job.status.created_at)
        return min(created_at_values, default=None)

    def _next_resource_version(self, old_job: Optional[BatchJob]) -> str:
        if old_job is None or old_job.metadata.resource_version is None:
            return "1"
        try:
            return str(int(old_job.metadata.resource_version) + 1)
        except ValueError:
            return "1"

    def _created_at_sort_key(self, job: BatchJob) -> datetime:
        created_at = job.status.created_at
        if created_at is not None:
            return created_at
        return datetime.min.replace(tzinfo=timezone.utc)
