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
from typing import Awaitable, Callable, Optional

from aibrix.batch.job_entity.batch_job import BatchJob


class BatchJobNotFoundError(KeyError):
    """Raised when a BatchJob document is not found in the store."""

    def __init__(self, batch_id: str):
        super().__init__(batch_id)
        self.batch_id = batch_id


class BatchJobStore(ABC):
    """Typed document store for ``BatchJob`` state.

    Implementations own the storage layout (key path, serialization format)
    and the consistency model. The interface intentionally hides bytes,
    paths, and content-type concerns from callers.

    Concurrency model: all implementations currently use last-writer-wins
    (LWW) on ``put`` — the most recent writer wins, prior writes can be
    silently overwritten. This is acceptable today because the controller
    is a single writer for state-machine transitions and the worker only
    appends usage which is idempotent on ``custom_id``.

    TODO(A.2 follow-up): introduce optimistic concurrency control via an
    ETag / version token returned by ``get`` and required by ``put`` so
    that concurrent writers cannot silently clobber each other. The
    interface will grow a ``put(..., expected_version=...)`` parameter and
    raise ``BatchJobConflictError`` on mismatch.
    """

    @abstractmethod
    async def get(self, batch_id: str) -> Optional[BatchJob]:
        """Return the stored BatchJob, or ``None`` if absent."""

    @abstractmethod
    async def put(self, batch_id: str, job: BatchJob) -> None:
        """Persist ``job`` under ``batch_id`` (last-writer-wins)."""

    @abstractmethod
    async def delete(self, batch_id: str) -> None:
        """Remove the BatchJob document. No-op if it does not exist."""

    async def update(
        self,
        batch_id: str,
        mutator: Callable[[BatchJob], Awaitable[BatchJob]],
    ) -> Optional[BatchJob]:
        """Read-modify-write convenience helper.

        Reads the current document, passes it to ``mutator``, and writes
        back the returned BatchJob. Returns the written BatchJob, or
        ``None`` if no document exists for ``batch_id``.

        TODO(A.2 follow-up): once CAS lands, this becomes a retry loop on
        version conflicts. Today it is a plain LWW write.
        """
        current = await self.get(batch_id)
        if current is None:
            return None
        updated = await mutator(current)
        await self.put(batch_id, updated)
        return updated
