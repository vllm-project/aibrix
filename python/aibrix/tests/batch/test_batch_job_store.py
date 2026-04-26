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

import uuid
from datetime import datetime, timezone

import pytest

from aibrix.batch.job_entity.batch_job import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    CompletionWindow,
    ObjectMeta,
    RequestCountStats,
    TypeMeta,
)
from aibrix.batch.store import (
    BatchJobStore,
    InMemoryBatchJobStore,
    ObjectBatchJobStore,
)
from aibrix.storage.local import LocalStorage


def _make_job(
    job_id: str = "job-1",
    state: BatchJobState = BatchJobState.CREATED,
    completed: int = 0,
) -> BatchJob:
    return BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name=f"test-{job_id}",
            namespace="default",
            uid=str(uuid.uuid4()),
            creationTimestamp=datetime.now(timezone.utc),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="file-input",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID=job_id,
            state=state,
            createdAt=datetime.now(timezone.utc),
            requestCounts=RequestCountStats(
                total=10, launched=0, completed=completed, failed=0
            ),
        ),
    )


@pytest.fixture(params=["memory", "object"])
def store(request, tmp_path) -> BatchJobStore:
    if request.param == "memory":
        return InMemoryBatchJobStore()
    return ObjectBatchJobStore(LocalStorage(base_path=str(tmp_path)))


@pytest.mark.asyncio
async def test_get_missing_returns_none(store: BatchJobStore):
    assert await store.get("does-not-exist") is None


@pytest.mark.asyncio
async def test_put_then_get_round_trip(store: BatchJobStore):
    job = _make_job("job-rt", state=BatchJobState.IN_PROGRESS, completed=3)
    await store.put("job-rt", job)

    fetched = await store.get("job-rt")
    assert fetched is not None
    assert fetched.status.job_id == "job-rt"
    assert fetched.status.state == BatchJobState.IN_PROGRESS
    assert fetched.status.request_counts.completed == 3


@pytest.mark.asyncio
async def test_put_overwrites_lww(store: BatchJobStore):
    job = _make_job("job-ow", completed=1)
    await store.put("job-ow", job)

    job_v2 = _make_job("job-ow", state=BatchJobState.FINALIZED, completed=10)
    await store.put("job-ow", job_v2)

    fetched = await store.get("job-ow")
    assert fetched is not None
    assert fetched.status.state == BatchJobState.FINALIZED
    assert fetched.status.request_counts.completed == 10


@pytest.mark.asyncio
async def test_delete_missing_is_noop(store: BatchJobStore):
    await store.delete("never-existed")  # must not raise


@pytest.mark.asyncio
async def test_delete_then_get_returns_none(store: BatchJobStore):
    job = _make_job("job-del")
    await store.put("job-del", job)
    await store.delete("job-del")
    assert await store.get("job-del") is None


@pytest.mark.asyncio
async def test_update_helper_mutates_and_persists(store: BatchJobStore):
    job = _make_job("job-up", completed=0)
    await store.put("job-up", job)

    async def bump(current: BatchJob) -> BatchJob:
        current.status.request_counts.completed = 7
        current.status.state = BatchJobState.IN_PROGRESS
        return current

    result = await store.update("job-up", bump)
    assert result is not None
    assert result.status.request_counts.completed == 7

    fetched = await store.get("job-up")
    assert fetched is not None
    assert fetched.status.request_counts.completed == 7
    assert fetched.status.state == BatchJobState.IN_PROGRESS


@pytest.mark.asyncio
async def test_update_helper_returns_none_when_missing(store: BatchJobStore):
    async def bump(current: BatchJob) -> BatchJob:
        return current  # pragma: no cover - should not be called

    assert await store.update("missing", bump) is None


@pytest.mark.asyncio
async def test_in_memory_isolates_callers_from_mutation():
    """Holding a reference to a previously-put job must not let a caller
    mutate stored state — store must deep-copy on the boundary."""
    store = InMemoryBatchJobStore()
    job = _make_job("job-iso", completed=1)
    await store.put("job-iso", job)

    # Mutate the local reference — should not bleed into the store.
    job.status.request_counts.completed = 999

    fetched = await store.get("job-iso")
    assert fetched is not None
    assert fetched.status.request_counts.completed == 1

    # Mutating the fetched copy must not bleed back either.
    fetched.status.request_counts.completed = 42
    again = await store.get("job-iso")
    assert again is not None
    assert again.status.request_counts.completed == 1


@pytest.mark.asyncio
async def test_object_store_writes_under_prefix_path(tmp_path):
    storage = LocalStorage(base_path=str(tmp_path))
    store = ObjectBatchJobStore(storage, prefix="custom/batches")
    await store.put("job-key", _make_job("job-key"))

    expected = tmp_path / "custom" / "batches" / "job-key" / "metadata.json"
    assert expected.exists(), f"expected metadata.json at {expected}"


@pytest.mark.asyncio
async def test_object_store_excludes_none_fields(tmp_path):
    """Optional unset fields (e.g. status.usage before worker reports) must
    serialize as absent, not as ``null``, to keep the document compact and
    forward-compatible."""
    storage = LocalStorage(base_path=str(tmp_path))
    store = ObjectBatchJobStore(storage)
    await store.put("job-en", _make_job("job-en"))

    raw = (tmp_path / "batches" / "job-en" / "metadata.json").read_bytes()
    assert b"null" not in raw
