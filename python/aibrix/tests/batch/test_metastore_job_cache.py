import os
from typing import Optional

import pytest

os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    JobEntityManager,
)
from aibrix.batch.storage import batch_metastore
from aibrix.metadata.cache.metastore import MetastoreJobCache
from aibrix.storage import StorageType


class FakeMetastore:
    def __init__(self) -> None:
        self.objects: dict[str, bytes] = {}

    async def put_object(self, key: str, data, **kwargs) -> bool:
        if isinstance(data, bytes):
            payload = data
        else:
            payload = str(data).encode("utf-8")
        self.objects[key] = payload
        return True

    async def get_object(self, key: str) -> bytes:
        try:
            return self.objects[key]
        except KeyError as exc:
            raise FileNotFoundError(key) from exc

    async def delete_object(self, key: str) -> None:
        self.objects.pop(key, None)

    async def list_objects(
        self,
        prefix: str = "",
        delimiter: Optional[str] = None,
        limit: Optional[int] = None,
        continuation_token: Optional[str] = None,
        after_key: Optional[str] = None,
    ) -> tuple[list[str], Optional[str]]:
        del delimiter
        keys = sorted(key for key in self.objects if key.startswith(prefix))
        if continuation_token is not None:
            offset = int(continuation_token or "0")
        elif after_key is not None:
            try:
                offset = keys.index(after_key) + 1
            except ValueError:
                return [], None
        else:
            offset = 0
        remaining_keys = keys[offset:]
        page = remaining_keys[:limit] if limit is not None else remaining_keys
        next_token = (
            str(offset + len(page))
            if limit is not None and len(remaining_keys) > len(page)
            else None
        )
        return page, next_token


@pytest.fixture
def fake_metastore(monkeypatch):
    store = FakeMetastore()
    calls = []

    def fake_initialize_batch_metastore(storage_type, params=None):
        calls.append((storage_type, dict(params or {})))
        module.batch_metastore.p_metastore = store

    from aibrix.metadata.cache import metastore as module

    monkeypatch.setattr(
        module, "initialize_batch_metastore", fake_initialize_batch_metastore
    )
    monkeypatch.setattr(module.batch_metastore, "p_metastore", None)
    return store, calls


@pytest.mark.asyncio
async def test_storage_job_cache_implements_job_entity_manager(fake_metastore):
    cache = MetastoreJobCache(storage_type=StorageType.LOCAL)
    assert isinstance(cache, JobEntityManager)


@pytest.mark.asyncio
async def test_storage_job_cache_initializes_batch_metastore(fake_metastore):
    _, calls = fake_metastore
    MetastoreJobCache(storage_type=StorageType.REDIS, params={"db": 3})

    assert calls == [(StorageType.REDIS, {"db": 3})]


@pytest.mark.asyncio
async def test_storage_job_cache_submit_update_list_and_delete(fake_metastore):
    _, _ = fake_metastore
    cache = MetastoreJobCache(storage_type=StorageType.LOCAL)
    committed_jobs = []
    updated_jobs = []
    deleted_jobs = []

    async def committed_handler(job):
        committed_jobs.append(job)
        return True

    async def updated_handler(old_job, new_job):
        updated_jobs.append((old_job, new_job))
        return True

    async def deleted_handler(job):
        deleted_jobs.append(job)
        return True

    cache.on_job_committed(committed_handler)
    cache.on_job_updated(updated_handler)
    cache.on_job_deleted(deleted_handler)

    older_spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    newer_spec = BatchJobSpec.from_strings(
        input_file_id="input-2",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )

    await cache.submit_job("session-1", older_spec)
    await cache.submit_job("session-2", newer_spec)

    first_job = committed_jobs[0]
    second_job = committed_jobs[1]
    listed_jobs = await cache.list_jobs()

    assert len(committed_jobs) == 2
    assert {job.session_id for job in listed_jobs} == {"session-1", "session-2"}
    assert (await cache.get_job(first_job.job_id)).session_id == "session-1"

    ready_job = (await cache.get_job(first_job.job_id)).model_copy(deep=True)
    ready_job.status.temp_output_file_id = "temp-output"
    await cache.update_job_ready(ready_job)

    finalized_job = (await cache.get_job(first_job.job_id)).model_copy(deep=True)
    finalized_job.status.state = BatchJobState.FINALIZED
    await cache.update_job_status(finalized_job)

    persisted_job = await cache.get_job(first_job.job_id)
    assert persisted_job.status.temp_output_file_id == "temp-output"
    assert persisted_job.status.state == BatchJobState.FINALIZED
    assert persisted_job.metadata.resource_version == "3"
    assert updated_jobs[-1][0].metadata.resource_version == "2"
    assert updated_jobs[-1][1].metadata.resource_version == "3"

    await cache.delete_job(second_job)

    assert await cache.get_job(second_job.job_id) is None
    assert deleted_jobs[0].job_id == second_job.job_id


@pytest.mark.asyncio
async def test_batch_metastore_list_metastore_keys_supports_pagination(fake_metastore):
    store, _ = fake_metastore
    MetastoreJobCache(storage_type=StorageType.LOCAL)

    for suffix in ("001", "002", "003"):
        await store.put_object(f"batchjob:{suffix}", "{}")

    first_page, next_token = await batch_metastore.list_metastore_keys(
        "batchjob:", limit=2
    )
    second_page, final_token = await batch_metastore.list_metastore_keys(
        "batchjob:", limit=2, continuation_token=next_token
    )

    assert first_page == ["batchjob:001", "batchjob:002"]
    assert next_token == "2"
    assert second_page == ["batchjob:003"]
    assert final_token is None


@pytest.mark.asyncio
async def test_batch_metastore_list_batch_jobs_supports_pagination(fake_metastore):
    _, _ = fake_metastore
    cache = MetastoreJobCache(storage_type=StorageType.LOCAL)

    for index in range(3):
        spec = BatchJobSpec.from_strings(
            input_file_id=f"input-{index}",
            endpoint="/v1/chat/completions",
            completion_window="24h",
        )
        await cache.submit_job(f"session-{index}", spec)

    first_page = await batch_metastore.list_batch_jobs(limit=2)
    second_page = await batch_metastore.list_batch_jobs(
        after=first_page[-1].job_id,
        limit=2,
    )

    combined_ids = {
        job.job_id for job in first_page + second_page if job.job_id is not None
    }

    assert len(first_page) == 2
    assert len(second_page) == 1
    assert len(combined_ids) == 3


@pytest.mark.asyncio
async def test_batch_metastore_list_batch_jobs_prefers_cached_jobs(fake_metastore):
    _, _ = fake_metastore
    cache = MetastoreJobCache(storage_type=StorageType.LOCAL)

    spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    await cache.submit_job("session-1", spec)
    job = (await cache.list_jobs())[0]

    cached_job = job.model_copy(deep=True)
    cached_job.status.temp_output_file_id = "cached-output"
    cache.active_jobs[job.job_id] = cached_job

    jobs = await batch_metastore.list_batch_jobs(
        limit=1,
        cached_job_getter=cache.active_jobs.get,
    )

    assert jobs[0].status.temp_output_file_id == "cached-output"


@pytest.mark.asyncio
async def test_storage_job_cache_list_jobs_prefers_active_jobs(fake_metastore):
    _, _ = fake_metastore
    cache = MetastoreJobCache(storage_type=StorageType.LOCAL)

    spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    await cache.submit_job("session-1", spec)
    job = (await cache.list_jobs())[0]

    cached_job = job.model_copy(deep=True)
    cached_job.status.temp_output_file_id = "cached-output"
    cache.active_jobs[job.job_id] = cached_job

    listed_jobs = await cache.list_jobs()

    assert listed_jobs[0].status.temp_output_file_id == "cached-output"


@pytest.mark.asyncio
async def test_storage_job_cache_start_bootstraps_unfinished_jobs(fake_metastore):
    _, _ = fake_metastore
    cache = MetastoreJobCache(storage_type=StorageType.LOCAL)
    committed_jobs = []

    async def committed_handler(job):
        committed_jobs.append(job)
        return True

    cache.on_job_committed(committed_handler)

    running_spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    finished_spec = BatchJobSpec.from_strings(
        input_file_id="input-2",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )

    await cache.submit_job("session-running", running_spec)
    running_job = committed_jobs.pop()
    running_job.status.state = BatchJobState.IN_PROGRESS
    await cache.update_job_status(running_job)

    await cache.submit_job("session-finished", finished_spec)
    finished_job = committed_jobs.pop()
    finished_job.status.state = BatchJobState.FINALIZED
    await cache.update_job_status(finished_job)

    restarted_cache = MetastoreJobCache(storage_type=StorageType.LOCAL)
    restarted_commits = []

    async def restarted_committed_handler(job):
        restarted_commits.append(job)
        return True

    restarted_cache.on_job_committed(restarted_committed_handler)
    restarted_cache._refresh_interval_seconds = 3600

    await restarted_cache.start()
    await restarted_cache.stop()

    assert (
        await batch_metastore.get_oldest_unfinished_job_created_at()
        == running_job.status.created_at
    )
    assert [job.job_id for job in restarted_commits] == [running_job.job_id]


@pytest.mark.asyncio
async def test_storage_job_cache_recovery_stops_after_oldest_unfinished_marker(
    fake_metastore, monkeypatch
):
    _, _ = fake_metastore
    cache = MetastoreJobCache(storage_type=StorageType.LOCAL)
    jobs: list[BatchJob] = []
    for index in range(25):
        spec = BatchJobSpec.from_strings(
            input_file_id=f"input-{index}",
            endpoint="/v1/chat/completions",
            completion_window="24h",
        )
        job = BatchJob.new_local(spec=spec)
        job.session_id = f"session-{index}"
        jobs.append(job)
    jobs.sort(key=lambda job: job.status.created_at, reverse=True)
    first_page = jobs[:20]
    second_page = jobs[20:]
    for job in first_page[3:]:
        job.status.state = BatchJobState.FINALIZED
    for job in second_page:
        job.status.state = BatchJobState.FINALIZED
    oldest_unfinished = min(job.status.created_at for job in first_page[:3])
    await batch_metastore.set_oldest_unfinished_job_created_at(oldest_unfinished)

    async def fake_list_jobs(after=None, limit=JobEntityManager.DEFAULT_JOB_PAGE_LIMIT):
        if after is None:
            return first_page
        if after == first_page[-1].job_id:
            return second_page
        return []

    monkeypatch.setattr(cache, "list_jobs", fake_list_jobs)

    recovered_jobs = await cache._list_recovery_jobs()

    assert [job.job_id for job in recovered_jobs] == [
        job.job_id for job in first_page[:3]
    ]
