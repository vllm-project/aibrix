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

import os

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import BatchJob, BatchJobEndpoint, JobEntityManager
from aibrix.metadata.cache.job import JobCache


def test_job_cache_implements_job_entity_manager():
    """Test that JobCache properly implements JobEntityManager interface."""
    cache = JobCache()
    assert isinstance(cache, JobEntityManager)


def test_job_cache_empty_operations():
    """Test JobCache operations when empty."""
    cache = JobCache()

    # Test list_jobs with empty cache
    jobs = cache.list_jobs()
    assert jobs == []

    # Test get_job with non-existent job
    result = cache.get_job("non-existent-id")
    assert result is None


def test_job_cache_callback_registration():
    """Test callback registration functionality."""
    cache = JobCache()

    # Track callbacks
    committed_jobs = []
    updated_jobs = []
    deleted_jobs = []

    def on_committed(job):
        committed_jobs.append(job)

    def on_updated(old_job, new_job):
        updated_jobs.append((old_job, new_job))

    def on_deleted(job):
        deleted_jobs.append(job)

    # Register callbacks
    cache.on_job_committed(on_committed)
    cache.on_job_updated(on_updated)
    cache.on_job_deleted(on_deleted)

    # Verify callbacks are stored
    assert cache._job_committed_handler == on_committed
    assert cache._job_updated_handler == on_updated
    assert cache._job_deleted_handler == on_deleted


def test_job_cache_manual_operations():
    """Test JobCache with manually added jobs."""
    cache = JobCache()

    # Create a mock BatchJob
    from datetime import datetime, timezone

    from aibrix.batch.job_entity import (
        BatchJobSpec,
        BatchJobState,
        BatchJobStatus,
        CompletionWindow,
        ObjectMeta,
        TypeMeta,
    )

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="test-job",
            namespace="default",
            uid="test-uid-123",
            creationTimestamp=datetime.now(timezone.utc),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="file-123",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS,
        ),
        status=BatchJobStatus(
            jobID="test-uid-123",
            state=BatchJobState.CREATED,
            createdAt=datetime.now(timezone.utc),
        ),
    )

    # Manually add job to cache (simulating what kopf handlers would do)
    cache.active_jobs["test-uid-123"] = batch_job

    # Test get_job
    retrieved_job = cache.get_job("test-uid-123")
    assert retrieved_job == batch_job
    assert retrieved_job.metadata.name == "test-job"

    # Test list_jobs
    jobs = cache.list_jobs()
    assert len(jobs) == 1
    assert jobs[0] == batch_job
