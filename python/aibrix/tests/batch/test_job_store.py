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
"""JobStore: document CRUD delegates to the metastore (one document store);
lifecycle events fire on writes; list_jobs is the recovery source."""

import pytest

from aibrix.batch.job_entity import (
    BatchJobEndpoint,
    BatchJobSpec,
    CompletionWindow,
)
from aibrix.batch.state import JobStore
from aibrix.batch.storage import batch_metastore


@pytest.fixture
def fake_metastore(monkeypatch):
    """In-memory stand-in for the metastore — the single document store the
    JobStore delegates to (keyed ``batchjob:<id>`` like the real one)."""
    store: dict = {}

    async def put(batch_id, job):
        store[batch_id] = job.copy()

    async def get(batch_id):
        return store.get(batch_id)

    async def delete(batch_id):
        store.pop(batch_id, None)

    async def list_keys(prefix):
        return [f"batchjob:{job_id}" for job_id in store]

    monkeypatch.setattr(batch_metastore, "put_batch_job", put)
    monkeypatch.setattr(batch_metastore, "get_batch_job", get)
    monkeypatch.setattr(batch_metastore, "delete_batch_job", delete)
    monkeypatch.setattr(batch_metastore, "list_metastore_keys", list_keys)
    return store


def _spec():
    return BatchJobSpec(
        input_file_id="file-1",
        endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
        completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
    )


@pytest.mark.asyncio
async def test_submit_persists_to_metastore_and_fires_committed(fake_metastore):
    em = JobStore()
    committed = []

    async def on_committed(job):
        committed.append(job)
        return True

    em.on_job_committed(on_committed)

    await em.submit_job("sess-1", _spec(), request_count=3)

    assert len(committed) == 1
    job_id = committed[0].job_id
    assert job_id is not None
    # Persisted to the (one) metastore, readable back, and listed for recovery.
    assert len(fake_metastore) == 1
    fetched = await em.get_job(job_id)
    assert fetched is not None and fetched.job_id == job_id
    listed = await em.list_jobs()
    assert [j.job_id for j in listed] == [job_id]


@pytest.mark.asyncio
async def test_update_persists_and_fires_updated(fake_metastore):
    em = JobStore()
    committed = []
    updated = []

    async def on_committed(job):
        committed.append(job)
        return True

    async def on_updated(old, new):
        updated.append((old, new))
        return True

    em.on_job_committed(on_committed)
    em.on_job_updated(on_updated)

    await em.submit_job("sess-2", _spec())
    job = committed[0]
    await em.update_job_status(job)

    assert len(updated) == 1
    old, new = updated[0]
    assert old.job_id == job.job_id and new.job_id == job.job_id


@pytest.mark.asyncio
async def test_delete_removes_from_metastore_and_fires_deleted(fake_metastore):
    em = JobStore()
    committed = []
    deleted = []

    async def on_committed(job):
        committed.append(job)
        return True

    async def on_deleted(job):
        deleted.append(job)
        return True

    em.on_job_committed(on_committed)
    em.on_job_deleted(on_deleted)

    await em.submit_job("sess-3", _spec())
    job = committed[0]
    await em.delete_job(job)

    assert len(deleted) == 1 and deleted[0].job_id == job.job_id
    assert len(fake_metastore) == 0
    assert await em.get_job(job.job_id) is None
