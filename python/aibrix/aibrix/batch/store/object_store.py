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

from aibrix.batch.job_entity.batch_job import BatchJob
from aibrix.batch.store.base import BatchJobStore
from aibrix.storage.base import BaseStorage

_DEFAULT_PREFIX = "batches"
_METADATA_FILE = "metadata.json"
_CONTENT_TYPE = "application/json"


class ObjectBatchJobStore(BatchJobStore):
    """BatchJobStore backed by an :class:`aibrix.storage.BaseStorage`.

    Layout:

        {prefix}/{batch_id}/metadata.json

    Serialization uses ``by_alias=True`` so the on-disk JSON keys match
    the API/CRD shape (``typeMeta``, ``jobID``, ``createdAt``, ...) rather
    than the Python snake_case attribute names — the same shape callers
    will eventually parse from K8s objects or external tooling.
    ``exclude_none=True`` keeps optional unset fields (notably
    ``status.usage`` before the worker reports tokens) absent on disk
    rather than serialized as ``null``.

    Concurrency: last-writer-wins. ``BaseStorage.put_object`` does not
    expose ETag-based conditional writes today, so two concurrent writers
    can clobber each other. The controller is the single writer for
    state-machine transitions and the worker appends idempotent usage, so
    LWW is sufficient for now. See ``BatchJobStore`` docstring for the
    planned CAS upgrade.
    """

    def __init__(
        self,
        storage: BaseStorage,
        prefix: str = _DEFAULT_PREFIX,
    ) -> None:
        self._storage = storage
        self._prefix = prefix.strip("/")

    def _key(self, batch_id: str) -> str:
        return f"{self._prefix}/{batch_id}/{_METADATA_FILE}"

    async def get(self, batch_id: str) -> Optional[BatchJob]:
        try:
            raw = await self._storage.get_object(self._key(batch_id))
        except FileNotFoundError:
            return None
        return BatchJob.model_validate_json(raw)

    async def put(self, batch_id: str, job: BatchJob) -> None:
        payload = job.model_dump_json(by_alias=True, exclude_none=True).encode("utf-8")
        await self._storage.put_object(
            self._key(batch_id),
            payload,
            content_type=_CONTENT_TYPE,
        )

    async def delete(self, batch_id: str) -> None:
        try:
            await self._storage.delete_object(self._key(batch_id))
        except FileNotFoundError:
            return
