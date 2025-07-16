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
import os
from datetime import datetime
from typing import List, Optional

import pytest

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    CompletionWindow,
    JobEntityManager,
    ObjectMeta,
    TypeMeta,
)
from aibrix.batch.job_manager import JobManager


@pytest.mark.asyncio
async def test_local_job_cancellation():
    """Test cancelling a local job (without entity manager)."""
    # Create job manager without entity manager
    job_manager = JobManager()

    # Create a job
    await job_manager.create_job(
        session_id="test-session-1",
        input_file_id="test-file-1",
        api_endpoint="/v1/chat/completions",
        completion_window="24h",
        meta_data={"test": "local"},
    )

    # Find the job ID
    job_id = next(iter(job_manager._pending_jobs.keys()))

    # Verify job is in pending state
    assert job_id in job_manager._pending_jobs
    assert job_id not in job_manager._done_jobs

    # Cancel the job
    result = job_manager.cancel_job(job_id)
    assert result is True

    # Verify job moved to done state with cancelled status
    assert job_id not in job_manager._pending_jobs
    assert job_id in job_manager._done_jobs

    cancelled_job = job_manager._done_jobs[job_id]
    assert cancelled_job.status.state == BatchJobState.CANCELED


@pytest.mark.asyncio
async def test_job_cancellation_race_condition():
    """Test race condition handling where job completes before cancellation."""
    job_manager = JobManager()

    # Create a job
    await job_manager.create_job(
        session_id="test-session-2",
        input_file_id="test-file-2",
        api_endpoint="/v1/embeddings",
        completion_window="24h",
        meta_data={"test": "race"},
    )

    job_id = next(iter(job_manager._pending_jobs.keys()))

    # Simulate job completing by manually updating its status
    job = job_manager._pending_jobs[job_id]
    completed_status = BatchJobStatus(
        jobID=job_id,
        state=BatchJobState.COMPLETED,
        createdAt=datetime.now(),
        completedAt=datetime.now(),
    )
    completed_job = BatchJob(
        typeMeta=job.type_meta,
        metadata=job.metadata,
        spec=job.spec,
        status=completed_status,
    )
    job_manager._pending_jobs[job_id] = completed_job

    # Try to cancel already completed job
    result = job_manager.cancel_job(job_id)
    assert result is False  # Should fail because job is already completed

    # Job is removed from pending during cancellation attempt, but since it failed,
    # the job doesn't get moved to done state - it gets lost
    assert job_id not in job_manager._pending_jobs
    assert job_id not in job_manager._done_jobs
    assert job_id not in job_manager._in_progress_jobs


def test_cancel_nonexistent_job():
    """Test cancelling a job that doesn't exist."""
    job_manager = JobManager()

    # Try to cancel non-existent job
    result = job_manager.cancel_job("nonexistent-job-id")
    assert result is False


@pytest.mark.asyncio
async def test_cancel_job_already_done():
    """Test cancelling a job that's already in done state."""
    job_manager = JobManager()

    # Create a job
    await job_manager.create_job(
        session_id="test-session-3",
        input_file_id="test-file-3",
        api_endpoint="/v1/completions",
        completion_window="24h",
        meta_data={"test": "done"},
    )

    job_id = next(iter(job_manager._pending_jobs.keys()))

    # Move job to done state manually
    job = job_manager._pending_jobs[job_id]
    del job_manager._pending_jobs[job_id]
    job_manager._done_jobs[job_id] = job

    # Try to cancel job that's already done
    result = job_manager.cancel_job(job_id)
    assert result is True  # Changed: done jobs now return True


def test_job_committed_handler():
    """Test that job_committed_handler correctly adds jobs to pending."""
    job_manager = JobManager()

    # Create a mock BatchJob
    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="test-job",
            namespace="default",
            uid="test-uid-123",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="test-file-123",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS,
        ),
        status=BatchJobStatus(
            jobID="test-job-id",
            state=BatchJobState.CREATED,
            createdAt=datetime.now(),
        ),
    )

    # Call the handler
    job_manager.job_committed_handler(batch_job)

    # Verify job is in pending state
    assert "test-job-id" in job_manager._pending_jobs
    assert job_manager._pending_jobs["test-job-id"] == batch_job


def test_job_deleted_handler():
    """Test that job_deleted_handler correctly moves jobs to done state."""
    job_manager = JobManager()

    # Create a mock BatchJob in pending state
    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="test-job",
            namespace="default",
            uid="test-uid-456",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="test-file-456",
            endpoint=BatchJobEndpoint.EMBEDDINGS,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS,
        ),
        status=BatchJobStatus(
            jobID="test-job-id-2",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(),
        ),
    )

    # Add job to pending state
    job_manager._pending_jobs["test-job-id-2"] = batch_job

    # Call the deleted handler
    job_manager.job_deleted_handler(batch_job)

    # Verify job is removed from pending (job_deleted_handler removes jobs, doesn't move them)
    assert "test-job-id-2" not in job_manager._pending_jobs
    assert "test-job-id-2" not in job_manager._done_jobs
    assert "test-job-id-2" not in job_manager._in_progress_jobs


class MockJobEntityManager(JobEntityManager):
    """Mock JobEntityManager for testing async job creation."""

    def __init__(self, delay: float = 0.1):
        super().__init__()
        self.delay = delay  # Delay before calling committed handler
        self.submitted_jobs: List[tuple] = []  # Track submitted jobs
        self.should_fail = False  # Flag to simulate failures

    def submit_job(self, session_id: str, job: BatchJobSpec):
        """Mock job submission with async callback."""
        if self.should_fail:
            raise RuntimeError("Mock job submission failed")

        self.submitted_jobs.append((session_id, job))

        # Simulate async job creation with a delay
        asyncio.create_task(self._simulate_job_creation(session_id, job))

    async def _simulate_job_creation(self, session_id: str, job_spec: BatchJobSpec):
        """Simulate async job creation process."""
        # Wait for the configured delay
        await asyncio.sleep(self.delay)

        # Create a mock BatchJob with the session_id
        batch_job = BatchJob(
            sessionID=session_id,  # Use alias
            typeMeta=TypeMeta(apiVersion="v1", kind="BatchJob"),  # Use alias
            metadata=ObjectMeta(
                resourceVersion="1",
                creationTimestamp=datetime.now(),
                deletionTimestamp=None,
            ),
            spec=job_spec,
            status=BatchJobStatus(
                jobID=f"mock-job-{session_id}",
                state=BatchJobState.CREATED,
                createdAt=datetime.now(),
            ),
        )

        # Call the committed handler
        if self._job_committed_handler:
            self._job_committed_handler(batch_job)

    def get_job(self, job_id: str) -> Optional[BatchJob]:
        """Mock get_job implementation."""
        return None

    def list_jobs(self) -> List[BatchJob]:
        """Mock list_jobs implementation."""
        return []

    def cancel_job(self, job_id: str) -> bool:
        """Mock cancel_job implementation."""
        return True


@pytest.mark.asyncio
async def test_aysnc_create_job():
    """Test that JobEntityManager assigns job_id and calls handlers correctly."""
    # Create mock job entity manager
    mock_entity_manager = MockJobEntityManager(delay=0.05)

    # Create job manager with entity manager
    job_manager = JobManager(job_entity_manager=mock_entity_manager)

    # Create a job using the async method
    session_id = "test-session-async-1"
    job_id = await job_manager.create_job(
        session_id=session_id,
        input_file_id="test-input-1",
        api_endpoint="/v1/chat/completions",
        completion_window="24h",
        meta_data={"test": "async"},
        timeout=5.0,
    )

    # Verify job was created successfully
    assert job_id is not None
    assert job_id == f"mock-job-{session_id}"

    # Verify job was submitted to entity manager
    assert len(mock_entity_manager.submitted_jobs) == 1
    submitted_session_id, submitted_spec = mock_entity_manager.submitted_jobs[0]
    assert submitted_session_id == session_id
    assert submitted_spec.input_file_id == "test-input-1"

    # Verify job was added to pending jobs
    assert job_id in job_manager._pending_jobs
    job = job_manager._pending_jobs[job_id]
    assert job.session_id == session_id
    assert job.status.job_id == job_id

    # Verify the future was cleaned up
    assert session_id not in job_manager._creating_jobs


@pytest.mark.asyncio
async def test_async_create_job_with_timeout():
    """Test that create_job throws error when timeout occurs."""
    # Create mock entity manager with long delay (longer than timeout)
    mock_entity_manager = MockJobEntityManager(delay=2.0)
    job_manager = JobManager(job_entity_manager=mock_entity_manager)

    # Attempt to create job with short timeout
    session_id = "test-session-timeout"

    with pytest.raises(asyncio.TimeoutError):
        await job_manager.create_job(
            session_id=session_id,
            input_file_id="test-input-timeout",
            api_endpoint="/v1/completions",
            completion_window="24h",
            meta_data={},
            timeout=0.1,  # Very short timeout
        )

    # Verify job was submitted but future was cleaned up due to timeout
    assert len(mock_entity_manager.submitted_jobs) == 1
    assert session_id not in job_manager._creating_jobs

    # Verify no job was added to pending (since timeout occurred)
    assert len(job_manager._pending_jobs) == 0

    # Wait for job to be added.
    await asyncio.sleep(3.0)

    # Verify the job will still be valid and we do nothing about it.
    assert len(job_manager._pending_jobs) == 1
    for job_id in job_manager._pending_jobs:
        job = job_manager._pending_jobs[job_id]
        assert job.status.state == BatchJobState.CREATED


@pytest.mark.asyncio
async def test_aysnc_create_job_throws_error():
    """Test that create_job throws error when job submission fails."""
    # Create mock entity manager that fails
    mock_entity_manager = MockJobEntityManager()
    mock_entity_manager.should_fail = True
    job_manager = JobManager(job_entity_manager=mock_entity_manager)

    # Attempt to create job
    session_id = "test-session-fail"

    with pytest.raises(RuntimeError, match="Mock job submission failed"):
        await job_manager.create_job(
            session_id=session_id,
            input_file_id="test-input-fail",
            api_endpoint="/v1/chat/completions",
            completion_window="24h",
            meta_data={},
            timeout=5.0,
        )

    # Verify no job was submitted or added
    assert len(mock_entity_manager.submitted_jobs) == 0
    assert session_id not in job_manager._creating_jobs
    assert len(job_manager._pending_jobs) == 0


@pytest.mark.asyncio
async def test_multiple_concurrent_job_creation():
    """Test creating multiple jobs concurrently."""
    mock_entity_manager = MockJobEntityManager(delay=0.1)
    job_manager = JobManager(job_entity_manager=mock_entity_manager)

    # Create multiple jobs concurrently
    tasks = []
    session_ids = []

    for i in range(3):
        session_id = f"test-session-concurrent-{i}"
        session_ids.append(session_id)
        task = job_manager.create_job(
            session_id=session_id,
            input_file_id=f"test-input-{i}",
            api_endpoint="/v1/chat/completions",
            completion_window="24h",
            meta_data={"index": str(i)},
            timeout=5.0,
        )
        tasks.append(task)

    # Wait for all jobs to complete
    job_ids = await asyncio.gather(*tasks)

    # Verify all jobs were created successfully
    assert len(job_ids) == 3
    assert all(job_id is not None for job_id in job_ids)

    # Verify all jobs are in pending state
    for i, job_id in enumerate(job_ids):
        assert job_id in job_manager._pending_jobs
        job = job_manager._pending_jobs[job_id]
        assert job.session_id == session_ids[i]

    # Verify all futures were cleaned up
    assert len(job_manager._creating_jobs) == 0

    # Verify all jobs were submitted to entity manager
    assert len(mock_entity_manager.submitted_jobs) == 3
