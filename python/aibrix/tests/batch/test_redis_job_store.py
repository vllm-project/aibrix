import copy
import os

import pytest

os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import (
    BatchJobSpec,
    BatchJobState,
    BatchJobStatusCopy,
    RequestCountStats,
)
from aibrix.batch.state import JobEntityManager
from aibrix.batch.state.redis_job_store import RedisJobStore


class FakeRedisPipeline:
    def __init__(self, redis):
        self.redis = redis
        self.commands = []

    def get(self, key):
        self.commands.append(("get", key))
        return self

    def smembers(self, key):
        self.commands.append(("smembers", key))
        return self

    def set(self, key, value, **kwargs):
        self.commands.append(("set", key, value, kwargs))
        return self

    def delete(self, *keys):
        self.commands.append(("delete", *keys))
        return self

    def zadd(self, key, mapping, **kwargs):
        self.commands.append(("zadd", key, mapping, kwargs))
        return self

    def zrem(self, key, *members):
        self.commands.append(("zrem", key, *members))
        return self

    def sadd(self, key, *values):
        self.commands.append(("sadd", key, *values))
        return self

    async def execute(self, raise_on_error=True):
        results = []
        for command in self.commands:
            name = command[0]
            args = list(command[1:])
            kwargs = {}
            if args and isinstance(args[-1], dict):
                kwargs = args.pop()
            result = getattr(self.redis, name)(*args, **kwargs)
            if hasattr(result, "__await__"):
                result = await result
            results.append(result)
        return results


class FakeRedis:
    def __init__(self):
        self.values = {}
        self.sorted_sets = {}
        self.sets = {}
        self.zrevrange_calls = []
        self.pipeline_calls = []

    async def get(self, key):
        value = self.values.get(key)
        if value is None:
            return None
        return copy.deepcopy(value)

    async def set(self, key, value, **kwargs):
        self.values[key] = value
        return True

    async def delete(self, *keys):
        for key in keys:
            self.values.pop(key, None)
        return 1

    async def zadd(self, key, mapping, **kwargs):
        self.sorted_sets.setdefault(key, {})
        self.sorted_sets[key].update(mapping)
        return 1

    async def zrevrange(self, key, start, end):
        self.zrevrange_calls.append((key, start, end))
        items = sorted(
            self.sorted_sets.get(key, {}).items(),
            key=lambda item: item[1],
            reverse=True,
        )
        members = [member for member, _ in items]
        if end == -1:
            selected = members[start:]
        else:
            selected = members[start : end + 1]
        return [
            member.encode("utf-8") if isinstance(member, str) else member
            for member in selected
        ]

    async def zrevrank(self, key, member):
        items = sorted(
            self.sorted_sets.get(key, {}).items(),
            key=lambda item: item[1],
            reverse=True,
        )
        members = [
            item_member.encode("utf-8") if isinstance(item_member, str) else item_member
            for item_member, _ in items
        ]
        encoded_member = member.encode("utf-8") if isinstance(member, str) else member
        try:
            return members.index(encoded_member)
        except ValueError:
            return None

    async def zrem(self, key, *members):
        if key in self.sorted_sets:
            for member in members:
                self.sorted_sets[key].pop(member, None)
        return 1

    async def sadd(self, key, *values):
        self.sets.setdefault(key, set()).update(values)
        return 1

    async def smembers(self, key):
        return {
            value.encode("utf-8") if isinstance(value, str) else value
            for value in self.sets.get(key, set())
        }

    def pipeline(self, transaction=True, shard_hint=None):
        pipeline = FakeRedisPipeline(self)
        self.pipeline_calls.append(pipeline.commands)
        return pipeline


@pytest.mark.asyncio
async def test_redis_job_cache_implements_job_entity_manager():
    cache = RedisJobStore(redis_client=FakeRedis())
    assert isinstance(cache, JobEntityManager)


@pytest.mark.asyncio
async def test_redis_job_cache_submit_and_list_jobs():
    cache = RedisJobStore(redis_client=FakeRedis())
    committed_jobs = []

    async def committed_handler(job):
        committed_jobs.append(job)
        return True

    cache.on_job_committed(committed_handler)

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

    assert len(committed_jobs) == 2
    assert committed_jobs[0].metadata.resource_version == "1"
    assert committed_jobs[1].metadata.resource_version == "1"

    listed_jobs = await cache.list_jobs()
    assert [job.session_id for job in listed_jobs] == ["session-2", "session-1"]
    assert (await cache.get_job(committed_jobs[0].job_id)).session_id == "session-1"


@pytest.mark.asyncio
async def test_redis_job_cache_list_jobs_paginates_with_after_cursor():
    cache = RedisJobStore(redis_client=FakeRedis())

    for index in range(4):
        spec = BatchJobSpec.from_strings(
            input_file_id=f"input-{index}",
            endpoint="/v1/chat/completions",
            completion_window="24h",
        )
        await cache.submit_job(f"session-{index}", spec)

    first_page = await cache.list_jobs(limit=2)
    assert [job.session_id for job in first_page] == ["session-3", "session-2"]

    second_page = await cache.list_jobs(after=first_page[-1].job_id, limit=2)
    assert [job.session_id for job in second_page] == ["session-1", "session-0"]

    empty_page = await cache.list_jobs(after="missing-job", limit=2)
    assert empty_page == []


@pytest.mark.asyncio
async def test_redis_job_cache_update_and_delete_callbacks():
    cache = RedisJobStore(redis_client=FakeRedis())
    updated_jobs = []
    deleted_jobs = []

    async def committed_handler(job):
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

    spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    await cache.submit_job("session-1", spec)

    job = (await cache.list_jobs())[0]
    ready_job = job.model_copy(deep=True)
    ready_job.status.in_progress_at = ready_job.status.created_at
    ready_job.status.temp_output_file_id = "temp-output"
    ready_job.status.temp_error_file_id = "temp-error"

    await cache.update_job_ready(ready_job)

    persisted_ready_job = await cache.get_job(job.job_id)
    assert persisted_ready_job.status.temp_output_file_id == "temp-output"
    assert persisted_ready_job.metadata.resource_version == "2"
    assert updated_jobs[-1][0].metadata.resource_version == "1"
    assert updated_jobs[-1][1].metadata.resource_version == "2"

    finalized_job = persisted_ready_job.model_copy(deep=True)
    finalized_job.status.state = BatchJobState.FINALIZED
    await cache.update_job_status(finalized_job)

    assert (await cache.get_job(job.job_id)).status.state == BatchJobState.FINALIZED
    assert (await cache.get_job(job.job_id)).metadata.resource_version == "3"

    await cache.delete_job(await cache.get_job(job.job_id))

    assert await cache.get_job(job.job_id) is None
    assert deleted_jobs[0].job_id == job.job_id


@pytest.mark.asyncio
async def test_redis_job_cache_delete_updates_oldest_unfinished_timestamp():
    redis = FakeRedis()
    cache = RedisJobStore(redis_client=redis)

    for index in range(2):
        spec = BatchJobSpec.from_strings(
            input_file_id=f"input-{index}",
            endpoint="/v1/chat/completions",
            completion_window="24h",
        )
        await cache.submit_job(f"session-{index}", spec)

    listed_jobs = await cache.list_jobs()
    oldest_job = min(listed_jobs, key=lambda job: job.status.created_at)
    expected_next_oldest = max(listed_jobs, key=lambda job: job.status.created_at)

    await cache.delete_job(oldest_job)

    assert (
        await cache._get_oldest_unfinished_job_created_at()
        == expected_next_oldest.status.created_at
    )


@pytest.mark.asyncio
async def test_redis_job_cache_persists_status_copies_separately():
    redis = FakeRedis()
    cache = RedisJobStore(redis_client=redis)

    spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    await cache.submit_job("session-1", spec)

    job = (await cache.list_jobs())[0]
    job.status.state = BatchJobState.IN_PROGRESS
    job.status.request_counts.total = 10
    job.status.request_counts.launched = 0
    job.status.request_counts.completed = 0
    job.status.status_copies = {
        "worker-1": BatchJobStatusCopy(
            state=BatchJobState.IN_PROGRESS,
            requestCounts=RequestCountStats(
                total=10, launched=2, completed=1, failed=0
            ),
            updated=True,
        ),
        "worker-2": BatchJobStatusCopy(
            state=BatchJobState.IN_PROGRESS,
            requestCounts=RequestCountStats(
                total=10, launched=3, completed=2, failed=1
            ),
            updated=True,
        ),
    }

    await cache.update_job_status(job)

    cache.active_jobs.clear()
    fetched = await cache.get_job(job.job_id)
    assert fetched.status.request_counts.total == 10
    assert fetched.status.request_counts.launched == 5
    assert fetched.status.request_counts.completed == 3
    assert fetched.status.request_counts.failed == 1
    assert set(fetched.status.status_copies) == {"worker-1", "worker-2"}
    assert cache.active_jobs[job.job_id].status.request_counts.launched == 5
    assert set(cache.active_jobs[job.job_id].status.status_copies) == {
        "worker-1",
        "worker-2",
    }
    assert f"batch_jobs:batchstatus_copies:{job.job_id}:worker-1" in redis.values
    assert f"batch_jobs:batchstatus_copies:{job.job_id}:worker-2" in redis.values

    finalized = fetched.model_copy(deep=True)
    finalized.status.state = BatchJobState.FINALIZED
    await cache.update_job_status(finalized)

    assert job.job_id not in cache.active_jobs
    uncached_finalized = await cache.get_job(job.job_id)
    assert uncached_finalized.status.status_copies is None

    await cache.delete_job(uncached_finalized)

    assert f"batch_jobs:batchstatus_copies:{job.job_id}:worker-1" not in redis.values
    assert f"batch_jobs:batchstatus_copies:{job.job_id}:worker-2" not in redis.values


@pytest.mark.asyncio
async def test_redis_job_cache_empty_prefix_interworks_with_batch_metastore_keys():
    redis = FakeRedis()
    cache = RedisJobStore(redis_client=redis, key_prefix="")

    spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    await cache.submit_job("session-1", spec)

    job = (await cache.list_jobs())[0]
    job.status.state = BatchJobState.IN_PROGRESS
    job.status.request_counts.total = 10
    job.status.request_counts.launched = 0
    job.status.request_counts.completed = 0
    job.status.status_copies = {
        "worker-1": BatchJobStatusCopy(
            state=BatchJobState.IN_PROGRESS,
            requestCounts=RequestCountStats(
                total=10, launched=2, completed=1, failed=0
            ),
            updated=True,
        )
    }

    await cache.update_job_status(job)

    assert f"batchjob:{job.job_id}" in redis.values
    assert f"batchstatus_copies:{job.job_id}:worker-1" in redis.values
    assert redis.sorted_sets["timestamps:all"][f"batchjob:{job.job_id}"] > 0
    assert (
        redis.sorted_sets["timestamps:all"][f"batchstatus_copies:{job.job_id}:worker-1"]
        > 0
    )

    cache.active_jobs.clear()
    fetched = await cache.get_job(job.job_id)
    assert fetched.status.request_counts.launched == 2
    assert fetched.status.request_counts.completed == 1
    assert set(fetched.status.status_copies) == {"worker-1"}
    assert cache.active_jobs[job.job_id].status.request_counts.launched == 2
    assert set(cache.active_jobs[job.job_id].status.status_copies) == {"worker-1"}

    finalized = fetched.model_copy(deep=True)
    finalized.status.state = BatchJobState.FINALIZED
    await cache.update_job_status(finalized)

    assert job.job_id not in cache.active_jobs
    uncached_finalized = await cache.get_job(job.job_id)
    assert uncached_finalized.status.status_copies is None

    listed_jobs = await cache.list_jobs()
    assert [listed.job_id for listed in listed_jobs] == [job.job_id]

    await cache.delete_job(uncached_finalized)

    assert f"batchjob:{job.job_id}" not in redis.values
    assert f"batchstatus_copies:{job.job_id}:worker-1" not in redis.values
    assert f"batchjob:{job.job_id}" not in redis.sorted_sets["timestamps:all"]
    assert (
        f"batchstatus_copies:{job.job_id}:worker-1"
        not in redis.sorted_sets["timestamps:all"]
    )


@pytest.mark.asyncio
async def test_redis_job_cache_list_jobs_caches_unfinished_loaded_jobs():
    redis = FakeRedis()
    cache = RedisJobStore(redis_client=redis)

    spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    await cache.submit_job("session-1", spec)

    job = (await cache.list_jobs())[0]
    job.status.state = BatchJobState.IN_PROGRESS
    job.status.request_counts.total = 5
    job.status.status_copies = {
        "worker-1": BatchJobStatusCopy(
            state=BatchJobState.IN_PROGRESS,
            requestCounts=RequestCountStats(total=5, launched=4, completed=3, failed=1),
            updated=True,
        )
    }
    await cache.update_job_status(job)

    cache.active_jobs.clear()
    listed_jobs = await cache.list_jobs()

    assert [listed.job_id for listed in listed_jobs] == [job.job_id]
    assert listed_jobs[0].status.request_counts.launched == 4
    assert set(listed_jobs[0].status.status_copies) == {"worker-1"}
    assert cache.active_jobs[job.job_id].status.request_counts.launched == 4


@pytest.mark.asyncio
async def test_redis_job_cache_list_jobs_batches_status_copy_fetches_across_jobs():
    redis = FakeRedis()
    cache = RedisJobStore(redis_client=redis)

    job_ids = []
    for index in range(2):
        spec = BatchJobSpec.from_strings(
            input_file_id=f"input-{index}",
            endpoint="/v1/chat/completions",
            completion_window="24h",
        )
        await cache.submit_job(f"session-{index}", spec)
        job = (await cache.list_jobs(limit=1))[0]
        job.status.state = BatchJobState.IN_PROGRESS
        job.status.request_counts.total = 5
        job.status.status_copies = {
            f"worker-{index}": BatchJobStatusCopy(
                state=BatchJobState.IN_PROGRESS,
                requestCounts=RequestCountStats(
                    total=5, launched=index + 1, completed=index, failed=0
                ),
                updated=True,
            )
        }
        await cache.update_job_status(job)
        job_ids.append(job.job_id)

    cache.active_jobs.clear()
    redis.pipeline_calls.clear()

    listed_jobs = await cache.list_jobs(limit=2)

    assert [job.job_id for job in listed_jobs] == job_ids[::-1]
    status_copy_get_batch = redis.pipeline_calls[-1]
    assert [command[0] for command in status_copy_get_batch] == ["get", "get"]
    assert {command[1] for command in status_copy_get_batch} == {
        f"batch_jobs:batchstatus_copies:{job_ids[0]}:worker-0",
        f"batch_jobs:batchstatus_copies:{job_ids[1]}:worker-1",
    }


@pytest.mark.asyncio
async def test_redis_job_cache_empty_prefix_paginates_through_shared_list_path():
    redis = FakeRedis()
    cache = RedisJobStore(redis_client=redis, key_prefix="")

    for index in range(4):
        spec = BatchJobSpec.from_strings(
            input_file_id=f"input-{index}",
            endpoint="/v1/chat/completions",
            completion_window="24h",
        )
        await cache.submit_job(f"session-{index}", spec)

    first_page = await cache.list_jobs(limit=2)
    assert [job.session_id for job in first_page] == ["session-3", "session-2"]

    second_page = await cache.list_jobs(after=first_page[-1].job_id, limit=2)
    assert [job.session_id for job in second_page] == ["session-1", "session-0"]
    assert ("timestamps:all", 0, -1) not in redis.zrevrange_calls


@pytest.mark.asyncio
async def test_redis_job_cache_recovery_uses_oldest_unfinished_timestamp():
    redis = FakeRedis()
    cache = RedisJobStore(redis_client=redis)

    unfinished_job_ids = []
    for index in range(25):
        spec = BatchJobSpec.from_strings(
            input_file_id=f"input-{index}",
            endpoint="/v1/chat/completions",
            completion_window="24h",
        )
        await cache.submit_job(f"session-{index}", spec)
        job = (await cache.list_jobs(limit=1))[0]
        if index >= 22:
            unfinished_job_ids.append(job.job_id)
            continue
        job.status.state = BatchJobState.FINALIZED
        await cache.update_job_status(job)

    redis.zrevrange_calls.clear()

    recovered_jobs = await cache._list_recovery_jobs()

    assert [job.job_id for job in recovered_jobs] == unfinished_job_ids[::-1]
    assert ("batch_jobs:index", 0, 19) in redis.zrevrange_calls
