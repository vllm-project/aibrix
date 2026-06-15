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
  * recovery — ``list_jobs`` reads the metastore on startup.

"""

from __future__ import annotations

import asyncio
from typing import List, Optional

from aibrix.batch.job_entity import BatchJob, BatchJobSpec
from aibrix.batch.state.job_entity_manager import JobEntityManager
from aibrix.batch.storage import batch_metastore
from aibrix.logger import init_logger

logger = init_logger(__name__)

_KEY_PREFIX = "batchjob:"


def _require_id(job: BatchJob) -> str:
    if job.job_id is None:
        raise ValueError("job_id is required")
    return job.job_id


class JobStore(JobEntityManager):
    """JobEntityManager backed by the batch metastore — the single document store."""

    # --- command store: document CRUD delegated to the metastore ----------
    async def submit_job(
        self, session_id: str, job_spec: BatchJobSpec, request_count: int = 0
    ) -> None:
        job = BatchJob.new_local(spec=job_spec, request_count=request_count)
        job.session_id = session_id
        await self._put(job)
        await self.job_committed(job)

    async def update_job_ready(self, job: BatchJob) -> None:
        await self._update(job)

    async def update_job_status(self, job: BatchJob) -> None:
        await self._update(job)

    async def cancel_job(self, job: BatchJob) -> None:
        await self._update(job)

    async def delete_job(self, job: BatchJob) -> None:
        job_id = _require_id(job)
        existing = await self.get_job(job_id) or job
        await batch_metastore.delete_batch_job(job_id)
        await self.job_deleted(existing)

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        return await batch_metastore.get_batch_job(job_id)

    async def list_jobs(self) -> List[BatchJob]:
        """All persisted jobs — the recovery source on startup."""
        keys = await batch_metastore.list_metastore_keys(_KEY_PREFIX)

        async def _get_job(key: str) -> Optional[BatchJob]:
            job_id = key[len(_KEY_PREFIX) :] if key.startswith(_KEY_PREFIX) else key
            return await batch_metastore.get_batch_job(job_id)

        # Fetch concurrently: sequential awaits bottleneck startup/recovery when
        # there are many persisted jobs.
        results = await asyncio.gather(*(_get_job(key) for key in keys))
        return [job for job in results if job is not None]

    # --- internals --------------------------------------------------------
    async def _update(self, job: BatchJob) -> None:
        job_id = _require_id(job)
        old = await self.get_job(job_id)
        await self._put(job)
        if old is not None:
            await self.job_updated(old, job)

    async def _put(self, job: BatchJob) -> None:
        await batch_metastore.put_batch_job(_require_id(job), job)
