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
from typing import Optional

from aibrix.batch.job_entity.batch_job import BatchJob
from aibrix.batch.store.base import BatchJobStore


class InMemoryBatchJobStore(BatchJobStore):
    """Process-local BatchJobStore for tests and single-node setups.

    Uses ``BatchJob.model_copy(deep=True)`` on both ingress and egress so
    callers cannot mutate stored state by holding a reference. A single
    asyncio lock serializes writes to keep semantics close to the future
    CAS-backed store.
    """

    def __init__(self) -> None:
        self._jobs: dict[str, BatchJob] = {}
        self._lock = asyncio.Lock()

    async def get(self, batch_id: str) -> Optional[BatchJob]:
        async with self._lock:
            job = self._jobs.get(batch_id)
            return job.model_copy(deep=True) if job is not None else None

    async def put(self, batch_id: str, job: BatchJob) -> None:
        async with self._lock:
            self._jobs[batch_id] = job.model_copy(deep=True)

    async def delete(self, batch_id: str) -> None:
        async with self._lock:
            self._jobs.pop(batch_id, None)
