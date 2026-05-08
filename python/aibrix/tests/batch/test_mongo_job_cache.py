import copy
import os

import pytest

os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import BatchJobSpec, BatchJobState, JobEntityManager
from aibrix.metadata.cache.mongodb import MongoJobCache


def _nested_value(document, key):
    value = document
    for part in key.split("."):
        value = value[part]
    return value


class FakeCursor:
    def __init__(self, documents):
        self._documents = documents

    def sort(self, key, direction):
        reverse = direction < 0
        return FakeCursor(
            sorted(
                self._documents,
                key=lambda document: _nested_value(document, key),
                reverse=reverse,
            )
        )

    def __iter__(self):
        return iter(self._documents)


class FakeCollection:
    def __init__(self):
        self.documents = {}
        self.indexes = []

    def create_index(self, key):
        self.indexes.append(key)

    def find_one(self, filter):
        document = self.documents.get(filter["_id"])
        if document is None:
            return None
        return copy.deepcopy(document)

    def find(self, filter):
        assert filter == {}
        return FakeCursor(
            [copy.deepcopy(document) for document in self.documents.values()]
        )

    def replace_one(self, filter, document, upsert=False):
        assert upsert
        self.documents[filter["_id"]] = copy.deepcopy(document)

    def delete_one(self, filter):
        self.documents.pop(filter["_id"], None)


@pytest.mark.asyncio
async def test_mongo_job_cache_implements_job_entity_manager():
    cache = MongoJobCache(mongo_collection=FakeCollection())
    assert isinstance(cache, JobEntityManager)


@pytest.mark.asyncio
async def test_mongo_job_cache_submit_and_list_jobs():
    cache = MongoJobCache(mongo_collection=FakeCollection())
    committed_jobs = []

    async def committed_handler(job):
        committed_jobs.append(job)
        return True

    cache.on_job_committed(committed_handler)

    newer_spec = BatchJobSpec.from_strings(
        input_file_id="input-2",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )
    older_spec = BatchJobSpec.from_strings(
        input_file_id="input-1",
        endpoint="/v1/chat/completions",
        completion_window="24h",
    )

    await cache.submit_job("session-1", older_spec)
    await cache.submit_job("session-2", newer_spec)

    assert len(committed_jobs) == 2
    assert committed_jobs[0].session_id == "session-1"
    assert committed_jobs[1].session_id == "session-2"
    assert committed_jobs[0].metadata.resource_version == "1"
    assert committed_jobs[1].metadata.resource_version == "1"

    listed_jobs = cache.list_jobs()
    assert [job.session_id for job in listed_jobs] == ["session-2", "session-1"]
    assert cache.get_job(committed_jobs[0].job_id).session_id == "session-1"


@pytest.mark.asyncio
async def test_mongo_job_cache_update_and_delete_callbacks():
    cache = MongoJobCache(mongo_collection=FakeCollection())
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

    job = cache.list_jobs()[0]
    ready_job = job.model_copy(deep=True)
    ready_job.status.in_progress_at = ready_job.status.created_at
    ready_job.status.temp_output_file_id = "temp-output"
    ready_job.status.temp_error_file_id = "temp-error"

    await cache.update_job_ready(ready_job)

    persisted_ready_job = cache.get_job(job.job_id)
    assert persisted_ready_job.status.temp_output_file_id == "temp-output"
    assert persisted_ready_job.metadata.resource_version == "2"
    assert updated_jobs[-1][0].metadata.resource_version == "1"
    assert updated_jobs[-1][1].metadata.resource_version == "2"

    finalized_job = persisted_ready_job.model_copy(deep=True)
    finalized_job.status.state = BatchJobState.FINALIZED
    await cache.update_job_status(finalized_job)

    assert cache.get_job(job.job_id).status.state == BatchJobState.FINALIZED
    assert cache.get_job(job.job_id).metadata.resource_version == "3"

    await cache.delete_job(cache.get_job(job.job_id))

    assert cache.get_job(job.job_id) is None
    assert deleted_jobs[0].job_id == job.job_id
