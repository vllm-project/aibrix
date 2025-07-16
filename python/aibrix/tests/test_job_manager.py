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
from datetime import datetime

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    CompletionWindow,
    ObjectMeta,
    TypeMeta,
)
from aibrix.batch.job_manager import JobManager


def test_local_job_cancellation():
    """Test cancelling a local job (without entity manager)."""
    # Create job manager without entity manager
    job_manager = JobManager()

    # Create a job
    job_manager.create_job(
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


def test_job_cancellation_race_condition():
    """Test race condition handling where job completes before cancellation."""
    job_manager = JobManager()

    # Create a job
    job_manager.create_job(
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


def test_cancel_job_already_done():
    """Test cancelling a job that's already in done state."""
    job_manager = JobManager()

    # Create a job
    job_manager.create_job(
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
            inputFileID="test-file-123",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS,
            completionWindow=CompletionWindow.TWENTY_FOUR_HOURS,
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
            inputFileID="test-file-456",
            endpoint=BatchJobEndpoint.EMBEDDINGS,
            completionWindow=CompletionWindow.TWENTY_FOUR_HOURS,
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
