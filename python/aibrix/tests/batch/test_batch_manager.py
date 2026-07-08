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
from datetime import datetime, timedelta
from typing import List, Optional

import pytest

# Set required environment variable before importing
os.environ.setdefault("SECRET_KEY", "test-secret-key-for-testing")

from aibrix.batch.batch_manager import BatchManager
from aibrix.batch.job_driver import BaseJobDriver, TerminateResult
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchJobStatusCopy,
    CompletionWindow,
    Condition,
    ConditionStatus,
    ConditionType,
    JobRuntimeRef,
    ObjectMeta,
    RequestCountStats,
    TypeMeta,
)
from aibrix.batch.state import JobEntityManager, JobMetaInfo
from aibrix.context import InfrastructureContext
from tests.fake.batch_runtime import FakeRuntime


def _job_manager() -> BatchManager:
    return BatchManager(InfrastructureContext())


def _set_current_loop_name(name: str) -> None:
    setattr(asyncio.get_running_loop(), "name", name)


class _CapturingScheduler:
    def __init__(self) -> None:
        self.appended_job_ids: list[str] = []

    def append_job(self, job_id: str) -> None:
        self.appended_job_ids.append(job_id)


def _in_progress_meta_job(job_id: str, total_requests: int) -> JobMetaInfo:
    job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name=f"job-{job_id}",
            namespace="default",
            uid=f"uid-{job_id}",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id=f"input-{job_id}",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID=job_id,
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(),
            inProgressAt=datetime.now(),
        ),
    )
    job.status.request_counts.total = total_requests
    return JobMetaInfo(job)


class _FakeJobDriver:
    def __init__(
        self,
        terminate_result: TerminateResult = TerminateResult.ACCEPTED,
        meta_job: JobMetaInfo | None = None,
    ) -> None:
        self.terminate_result = terminate_result
        self.meta_job = meta_job
        self.terminate_calls: list[str | None] = []
        self.cleanup_calls: list[str | None] = []

    async def terminate(self, job: BatchJob) -> TerminateResult:
        self.terminate_calls.append(job.job_id)
        if (
            self.terminate_result == TerminateResult.ACCEPTED
            and self.meta_job is not None
        ):
            self.meta_job.status = job.status.model_copy(deep=True)
        return self.terminate_result

    async def cleanup(self, job: BatchJob) -> None:
        self.cleanup_calls.append(job.job_id)


@pytest.mark.asyncio
async def test_update_job_local_status_execution_only_preserves_worker_snapshot():
    job_manager = _job_manager()
    meta_job = _in_progress_meta_job("job-execution-only", total_requests=10)
    meta_job.status.set_runtime_ref(
        "base",
        JobRuntimeRef(driverType="base", ownerRef="worker-1", attempt=1),
    )
    meta_job.status.status_copies = {
        "worker-1": BatchJobStatusCopy(
            state=BatchJobState.IN_PROGRESS,
            requestCounts=RequestCountStats(
                total=10,
                launched=4,
                completed=3,
                failed=1,
            ),
        )
    }
    job_manager._in_progress_jobs[meta_job.job_id] = meta_job

    status = meta_job.status.model_copy(deep=True)
    status.remove_runtime_ref("base")
    status.request_counts = RequestCountStats()

    updated = await job_manager.update_job_local_status(
        meta_job.job_id,
        "worker-1",
        status,
        update_keys={"execution"},
    )

    assert updated.status.execution is None
    assert updated.status.status_copies is not None
    assert updated.status.status_copies["worker-1"].request_counts.total == 10
    assert updated.status.status_copies["worker-1"].request_counts.launched == 4
    assert updated.status.status_copies["worker-1"].request_counts.completed == 3
    assert updated.status.status_copies["worker-1"].request_counts.failed == 1
    assert updated.status.status_copies["worker-1"].updated is False


@pytest.mark.asyncio
async def test_local_job_cancellation():
    """Test cancelling a local job (without entity manager)."""
    # Create job manager without entity manager
    job_manager = _job_manager()

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
    result = await job_manager.cancel_job(job_id)
    assert result == TerminateResult.ACCEPTED

    # Verify job moved to done state with cancelled status
    assert job_id not in job_manager._pending_jobs
    assert job_id in job_manager._done_jobs

    cancelled_job = job_manager._done_jobs[job_id]
    assert cancelled_job.status.state == BatchJobState.FINALIZED
    assert cancelled_job.status.cancelled
    # OpenAI parity: a finalized-cancelled batch must stamp cancelled_at, not
    # leave it null (regression guard for the cancel timestamp bug).
    assert cancelled_job.status.cancelled_at is not None
    assert cancelled_job.status.completed_at is None


@pytest.mark.asyncio
async def test_cancel_nonexistent_job():
    """Test cancelling a job that doesn't exist."""
    job_manager = _job_manager()

    # Try to cancel non-existent job
    result = await job_manager.cancel_job("nonexistent-job-id")
    assert result == TerminateResult.REJECTED


@pytest.mark.asyncio
async def test_cancel_job_already_done():
    """Test cancelling a job that's already in done state."""
    job_manager = _job_manager()

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
    result = await job_manager.cancel_job(job_id)
    assert result == TerminateResult.REJECTED


@pytest.mark.asyncio
async def test_cancel_in_progress_job_calls_live_driver_terminate_before_persisting():
    job_manager = _job_manager()
    meta_job = _in_progress_meta_job("test-job-id-driver-cancel", total_requests=1)
    driver = _FakeJobDriver(meta_job=meta_job)
    meta_job._job_driver = driver
    job_manager._in_progress_jobs[meta_job.job_id] = meta_job

    result = await job_manager.cancel_job(meta_job.job_id)

    assert result == TerminateResult.ACCEPTED
    assert driver.terminate_calls == [meta_job.job_id]
    assert meta_job.status.state == BatchJobState.CANCELLING


@pytest.mark.asyncio
async def test_cancel_in_progress_job_is_denied_when_live_driver_rejects_terminate():
    job_manager = _job_manager()
    meta_job = _in_progress_meta_job("test-job-id-driver-deny", total_requests=1)
    driver = _FakeJobDriver(terminate_result=TerminateResult.REJECTED)
    meta_job._job_driver = driver
    job_manager._in_progress_jobs[meta_job.job_id] = meta_job

    result = await job_manager.cancel_job(meta_job.job_id)

    assert result == TerminateResult.REJECTED
    assert driver.terminate_calls == [meta_job.job_id]
    assert meta_job.status.state == BatchJobState.IN_PROGRESS


@pytest.mark.asyncio
async def test_job_committed_handler():
    """Test that job_committed_handler correctly adds jobs to pending."""
    job_manager = _job_manager()

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
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="test-job-id",
            state=BatchJobState.CREATED,
            createdAt=datetime.now(),
        ),
    )

    # Call the handler
    await job_manager.job_committed_handler(batch_job)

    # Verify job is in pending state
    assert "test-job-id" in job_manager._pending_jobs
    assert job_manager._pending_jobs["test-job-id"] == batch_job


@pytest.mark.asyncio
async def test_job_committed_handler_recovers_session_scoped_job_without_future():
    job_manager = _job_manager()

    batch_job = BatchJob(
        sessionID="recovered-session",
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="test-job",
            namespace="default",
            uid="test-uid-124",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="test-file-124",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="recovered-job-id",
            state=BatchJobState.CREATED,
            createdAt=datetime.now(),
        ),
    )

    result = await job_manager.job_committed_handler(batch_job)

    assert result is True
    assert "recovered-job-id" in job_manager._pending_jobs


@pytest.mark.asyncio
async def test_job_committed_handler_recovers_done_job_without_scheduling():
    job_manager = _job_manager()

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="done-job",
            namespace="default",
            uid="done-uid-1",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="done-file",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="done-job-id",
            state=BatchJobState.FINALIZED,
            createdAt=datetime.now(),
            finalizedAt=datetime.now(),
        ),
    )

    result = await job_manager.job_committed_handler(batch_job)

    assert result is False
    assert job_manager._done_jobs["done-job-id"] == batch_job


@pytest.mark.asyncio
async def test_job_committed_handler_expires_overdue_job_before_scheduling():
    job_manager = _job_manager()
    scheduler = _CapturingScheduler()
    job_manager.set_scheduler(scheduler)

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="expired-before-scheduling",
            namespace="default",
            uid="expired-before-scheduling-uid",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="expired-file",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=1,
        ),
        status=BatchJobStatus(
            jobID="expired-before-scheduling-id",
            state=BatchJobState.CREATED,
            createdAt=datetime.now() - timedelta(seconds=5),
        ),
    )

    result = await job_manager.job_committed_handler(batch_job)

    assert result is True
    assert scheduler.appended_job_ids == []
    assert batch_job.job_id not in job_manager._pending_jobs
    assert batch_job.job_id in job_manager._done_jobs
    expired = job_manager._done_jobs[batch_job.job_id]
    assert expired.status.finished
    assert expired.status.condition == ConditionType.EXPIRED
    assert expired.status.expired_at is not None


@pytest.mark.asyncio
async def test_job_committed_handler_finishes_unfinished_expired_job_before_scheduling():
    job_manager = _job_manager()
    scheduler = _CapturingScheduler()
    job_manager.set_scheduler(scheduler)

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="expired-by-condition",
            namespace="default",
            uid="expired-by-condition-uid",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="expired-condition-file",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="expired-by-condition-id",
            state=BatchJobState.CREATED,
            createdAt=datetime.now(),
        ),
    )
    batch_job.status.add_condition(
        Condition(
            type=ConditionType.EXPIRED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=datetime.now(),
        )
    )

    result = await job_manager.job_committed_handler(batch_job)

    assert result is True
    assert scheduler.appended_job_ids == []
    assert batch_job.job_id not in job_manager._pending_jobs
    assert batch_job.job_id in job_manager._done_jobs
    expired = job_manager._done_jobs[batch_job.job_id]
    assert expired.status.finished
    assert expired.status.condition == ConditionType.EXPIRED
    assert expired.status.expired_at is not None
    assert len(expired.status.conditions or []) == 1


@pytest.mark.asyncio
async def test_validate_job_finalizes_worker_style_validation_failure(monkeypatch):
    # Manager coverage stops at the admit/finalize boundary: once
    # BaseJobDriver.validate_job raises, BatchManager must finalize the job
    # into the done set. The detailed invalid-input permutations live in
    # tests/batch/test_base_job_driver.py.
    job_manager = _job_manager()

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="test-job",
            namespace="default",
            uid="test-uid-789",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="missing-file",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="test-worker-job-id",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(),
            inProgressAt=None,
        ),
    )
    job_manager._pending_jobs["test-worker-job-id"] = batch_job

    async def _fail_validate(self, job):
        raise BatchJobError(
            code=BatchJobErrorCode.INVALID_INPUT_FILE,
            message="input file not found",
        )

    monkeypatch.setattr(
        "aibrix.batch.job_driver.base.BaseJobDriver.validate_job",
        _fail_validate,
    )

    result = await job_manager.admit("test-worker-job-id")

    assert result is None
    failed_job = job_manager._done_jobs["test-worker-job-id"]
    assert failed_job.status.state == BatchJobState.FINALIZED
    assert failed_job.status.failed


@pytest.mark.asyncio
async def test_mark_job_failed_finalizes_when_any_output_artifact_prepared():
    job_manager = _job_manager()
    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="test-job",
            namespace="default",
            uid="test-uid-partial-output",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="test-file-partial-output",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="test-partial-output-job-id",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(),
            tempOutputFileID="temp-output-id",
            tempErrorFileID="temp-error-id",
        ),
    )
    job_manager._in_progress_jobs[batch_job.job_id] = JobMetaInfo(batch_job)

    result = await job_manager.mark_job_failed(
        batch_job.job_id,
        BatchJobError(code=BatchJobErrorCode.UNKNOWN_ERROR, message="boom"),
    )

    assert result.status.state == BatchJobState.IN_PROGRESS
    assert result.status.condition == ConditionType.FAILED
    assert result.status.finalizing_at is None
    assert result.status.finalized_at is None


@pytest.mark.asyncio
async def test_expire_job_finalizes_pending_job():
    """A past-due pending job is expired into the done pool with the expired
    condition + expired_at, instead of being silently left in pending."""
    job_manager = _job_manager()
    await job_manager.create_job(
        session_id="test-session-expire",
        input_file_id="test-file-expire",
        api_endpoint="/v1/chat/completions",
        completion_window="24h",
        meta_data={},
    )
    job_id = next(iter(job_manager._pending_jobs.keys()))

    result = await job_manager.expire_job(job_id)

    assert result is True
    assert job_id not in job_manager._pending_jobs
    assert job_id in job_manager._done_jobs
    expired = job_manager._done_jobs[job_id]
    assert expired.status.state == BatchJobState.FINALIZED
    assert expired.status.finished
    assert expired.status.expired_at is not None
    assert expired.status.condition == ConditionType.EXPIRED


@pytest.mark.asyncio
async def test_expire_job_marks_admitted_job_expired_without_immediate_finalization():
    """An admitted job is also allowed to expire.

    The in-progress entry stays live so the runtime can observe the expired
    condition and finish its stop/finalizing flow instead of being moved
    straight to the done pool by the cleanup loop.
    """
    job_manager = _job_manager()
    await job_manager.create_job(
        session_id="test-session-expire2",
        input_file_id="test-file-expire2",
        api_endpoint="/v1/chat/completions",
        completion_window="24h",
        meta_data={},
    )
    job_id = next(iter(job_manager._pending_jobs.keys()))
    # Simulate admission: the job left pending for in-progress.
    job = job_manager._pending_jobs.pop(job_id)
    job.status.state = BatchJobState.IN_PROGRESS
    job_manager._in_progress_jobs[job_id] = job

    result = await job_manager.expire_job(job_id)

    assert result is True
    assert job_id in job_manager._in_progress_jobs
    assert job_id not in job_manager._done_jobs
    expiring = job_manager._in_progress_jobs[job_id]
    assert expiring.status.state == BatchJobState.IN_PROGRESS
    assert not expiring.status.finished
    assert expiring.status.expired_at is None
    assert expiring.status.condition == ConditionType.EXPIRED


@pytest.mark.asyncio
async def test_expire_job_terminates_live_driver_for_admitted_job():
    job_manager = _job_manager()
    meta_job = _in_progress_meta_job("test-job-id-expire-live-driver", total_requests=1)
    meta_job.status.set_runtime_ref(
        "fake",
        JobRuntimeRef(driverType="fake", ownerRef="owner-1", attempt=1),
    )
    driver = _FakeJobDriver(meta_job=meta_job)
    meta_job._job_driver = driver
    job_manager._in_progress_jobs[meta_job.job_id] = meta_job

    result = await job_manager.expire_job(meta_job.job_id)

    assert result is True
    assert driver.terminate_calls == [meta_job.job_id]
    assert meta_job.status.condition == ConditionType.EXPIRED
    assert meta_job.status.state == BatchJobState.IN_PROGRESS


@pytest.mark.asyncio
async def test_expire_job_skips_already_finalizing_job():
    job_manager = _job_manager()
    meta_job = _in_progress_meta_job("test-job-id-expire-finalizing", total_requests=1)
    meta_job.status.state = BatchJobState.FINALIZING
    meta_job.status.finalizing_at = datetime.now()
    driver = _FakeJobDriver(meta_job=meta_job)
    meta_job._job_driver = driver
    job_manager._in_progress_jobs[meta_job.job_id] = meta_job

    result = await job_manager.expire_job(meta_job.job_id)

    assert result is False
    assert driver.terminate_calls == []
    assert meta_job.job_id in job_manager._in_progress_jobs
    assert meta_job.job_id not in job_manager._done_jobs
    assert meta_job.status.state == BatchJobState.FINALIZING
    assert meta_job.status.condition is None


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("job_id", "owner_ref", "initial_bucket"),
    [
        ("test-job-id-expire-orphaned", "owner-2", "in_progress"),
        ("test-job-id-expire-recovered-pending", "owner-3", "pending"),
    ],
)
async def test_expired_job_cleans_up_during_recovery(
    monkeypatch,
    job_id: str,
    owner_ref: str,
    initial_bucket: str,
):
    job_manager = _job_manager()
    recovered_job = _in_progress_meta_job(job_id, total_requests=1)
    recovered_job.status.set_runtime_ref(
        "fake",
        JobRuntimeRef(driverType="fake", ownerRef=owner_ref, attempt=1),
    )
    if initial_bucket == "pending":
        job_manager._pending_jobs[recovered_job.job_id] = recovered_job
    else:
        job_manager._in_progress_jobs[recovered_job.job_id] = recovered_job
    cleanup_runtime = FakeRuntime()
    cleanup_driver = BaseJobDriver(job_manager, cleanup_runtime, recovered_job)

    monkeypatch.setattr(
        "aibrix.batch.batch_manager.create_job_driver",
        lambda context, progress_manager, entity_manager, job, inference_client: (
            cleanup_driver
        ),
    )

    result = await job_manager.expire_job(recovered_job.job_id)

    assert result is True
    assert cleanup_runtime.cleanup_job_ids == [recovered_job.job_id]
    assert cleanup_runtime.teardown_handles == ["fake-handle"]
    assert recovered_job.job_id not in job_manager._pending_jobs
    assert recovered_job.job_id not in job_manager._in_progress_jobs
    assert recovered_job.job_id in job_manager._done_jobs
    assert job_manager._done_jobs[recovered_job.job_id].status.execution is None
    assert (
        job_manager._done_jobs[recovered_job.job_id].status.condition
        == ConditionType.EXPIRED
    )
    assert (
        job_manager._done_jobs[recovered_job.job_id].status.state
        == BatchJobState.FINALIZED
    )


@pytest.mark.asyncio
async def test_admit_job_persists_validated_request_count(monkeypatch):
    mock_entity_manager = MockJobEntityManager(delay=0.0)

    _set_current_loop_name("test_validate_job_persists_validated_request_count")
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(mock_entity_manager)

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="validated-job",
            namespace="default",
            uid="validated-uid",
            creationTimestamp=datetime.now(),
            resourceVersion="1",
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="test-input-validated",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="validated-job-id",
            state=BatchJobState.CREATED,
            createdAt=datetime.now(),
        ),
    )
    job_id = batch_job.job_id
    job_manager._pending_jobs[job_id] = batch_job
    mock_entity_manager.jobs[job_id] = batch_job

    class _ValidatedDriver:
        def __init__(self, progress_manager):
            self._progress_manager = progress_manager

        async def validate_job(self, job):
            validated_status = job.status.model_copy(deep=True)
            validated_status.request_counts.total = 3
            await self._progress_manager.mark_job_validated(
                job.job_id, validated_status
            )

    monkeypatch.setattr(
        "aibrix.batch.batch_manager.create_job_driver",
        lambda context, progress_manager, entity_manager, job, inference_client: (
            _ValidatedDriver(progress_manager)
        ),
    )

    jod_driver = await job_manager.admit(job_id)

    assert jod_driver is not None  # assert job admitted
    validated_job = await mock_entity_manager.get_job(job_id)
    assert validated_job is not None
    assert validated_job.status.request_counts.total == 3
    in_progress_job = job_manager._in_progress_jobs[job_id]
    assert in_progress_job.status.request_counts.total == 3
    assert in_progress_job.status.state == BatchJobState.IN_PROGRESS


@pytest.mark.asyncio
async def test_mark_job_failed_is_idempotent_for_done_job():
    job_manager = _job_manager()

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="failed-job",
            namespace="default",
            uid="failed-uid",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="test-file-failed",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="done-job-id",
            state=BatchJobState.FINALIZED,
            createdAt=datetime.now(),
            failedAt=datetime.now(),
        ),
    )
    job_manager._done_jobs["done-job-id"] = batch_job

    result = await job_manager.mark_job_failed(
        "done-job-id",
        BatchJobError(
            code=BatchJobErrorCode.INFERENCE_FAILED,
            message="duplicate failure",
        ),
    )

    assert result is batch_job
    assert job_manager._done_jobs["done-job-id"] is batch_job
    assert batch_job.status.state == BatchJobState.FINALIZED
    assert batch_job.status.failed_at is not None


@pytest.mark.asyncio
async def test_job_deleted_handler():
    """Test that job_deleted_handler correctly moves jobs to done state."""
    job_manager = _job_manager()

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
            endpoint=BatchJobEndpoint.EMBEDDINGS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
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
    await job_manager.job_deleted_handler(batch_job)

    # Verify job is removed from pending (job_deleted_handler removes jobs, doesn't move them)
    assert "test-job-id-2" not in job_manager._pending_jobs
    assert "test-job-id-2" not in job_manager._done_jobs
    assert "test-job-id-2" not in job_manager._in_progress_jobs


@pytest.mark.asyncio
async def test_job_deleted_handler_marks_in_progress_job_cancelling():
    job_manager = _job_manager()
    meta_job = _in_progress_meta_job("test-job-id-3", total_requests=1)
    meta_job._job_driver = _FakeJobDriver()
    job_manager._in_progress_jobs["test-job-id-3"] = meta_job

    result = await job_manager.job_deleted_handler(meta_job)

    assert result is True
    assert "test-job-id-3" in job_manager._in_progress_jobs
    assert (
        job_manager._in_progress_jobs["test-job-id-3"].status.state
        == BatchJobState.CANCELLING
    )


@pytest.mark.asyncio
async def test_job_deleted_handler_deletes_after_finalized_when_cancel_triggered():
    job_manager = _job_manager()
    meta_job = _in_progress_meta_job("test-job-id-4", total_requests=1)
    meta_job._job_driver = _FakeJobDriver()
    job_manager._in_progress_jobs["test-job-id-4"] = meta_job

    result = await job_manager.job_deleted_handler(meta_job)

    assert result is True
    assert "test-job-id-4" in job_manager._pending_deleted_jobs
    cancelling_job = job_manager._in_progress_jobs["test-job-id-4"]
    assert cancelling_job.status.state == BatchJobState.CANCELLING
    finalized_job = cancelling_job.copy()
    finalized_job.status.state = BatchJobState.FINALIZED
    finalized_job.status.finalized_at = datetime.now()
    finalized_job.status.cancelled_at = finalized_job.status.finalized_at

    await job_manager.job_updated_handler(cancelling_job, finalized_job)

    assert "test-job-id-4" not in job_manager._pending_deleted_jobs
    assert "test-job-id-4" not in job_manager._in_progress_jobs
    assert "test-job-id-4" not in job_manager._done_jobs


class MockJobEntityManager(JobEntityManager):
    """Mock JobEntityManager for testing async job creation."""

    def __init__(self, delay: float = 0.1):
        super().__init__()
        self.delay = delay  # Delay before calling committed handler
        self.submitted_jobs: List[tuple] = []  # Track submitted jobs
        self.should_fail = False  # Flag to simulate failures
        self.jobs: dict[str, BatchJob] = {}

    async def submit_job(
        self, session_id: str, job: BatchJobSpec, request_count: int = 0
    ):
        """Mock job submission with async callback."""
        print(f"start time: {datetime.now()}")
        if self.should_fail:
            raise RuntimeError("Mock job submission failed")

        self.submitted_jobs.append((session_id, job, request_count))

        # Simulate async job creation with a delay
        await self._simulate_job_creation(session_id, job)
        print(f"end time: {datetime.now()}")

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
                state=BatchJobState.IN_PROGRESS,  # Set to in_progress to skip job validation and preparetion.
                createdAt=datetime.now(),
            ),
        )
        job_id = batch_job.job_id
        assert job_id is not None
        self.jobs[job_id] = batch_job

        # Call the committed handler
        await self.job_committed(batch_job)

    async def get_job(
        self, job_id: str, force_reload: bool = False
    ) -> Optional[BatchJob]:
        """Mock get_job implementation."""
        if job_id in self.active_jobs and not force_reload:
            return self.active_jobs[job_id]
        job = self.jobs.get(job_id)
        await self._publish_active_job_on_cache_miss(job)
        if job is not None and not job.status.finished:
            return self.active_jobs.get(job_id, job)
        return job

    async def update_job_ready(self, job: BatchJob) -> None:
        """Mock update_job_ready implementation."""
        job_id = job.job_id
        assert job_id is not None
        self.jobs[job_id] = job

    async def update_job_status(self, job: BatchJob) -> None:
        """Mock update_job_status implementation."""
        job_id = job.job_id
        assert job_id is not None
        old_job = self.jobs.get(job_id)
        self.jobs[job_id] = job
        if old_job is not None:
            await self.job_updated(old_job, job)

    async def list_jobs(
        self,
        after: Optional[str] = None,
        limit: int = JobEntityManager.DEFAULT_JOB_PAGE_LIMIT,
    ) -> List[BatchJob]:
        """Mock list_jobs implementation."""
        jobs = list(self.active_jobs.values())
        jobs.sort(key=lambda job: job.status.created_at, reverse=True)
        return self._paginate_jobs(jobs, after=after, limit=limit)

    async def cancel_job(self, job: BatchJob):
        """Mock cancel_job implementation."""
        job_id = job.job_id
        assert job_id is not None
        old_job = self.jobs.get(job_id)
        self.jobs[job_id] = job
        if old_job is not None:
            await self.job_updated(old_job, job)

    async def delete_job(self, job: BatchJob):
        """Mock cancel_job implementation."""
        job_id = job.job_id
        assert job_id is not None
        old_job = self.jobs.pop(job_id, None)
        if old_job is not None:
            await self.job_deleted(old_job)

    def _paginate_jobs(
        self,
        jobs: list[BatchJob],
        after: Optional[str] = None,
        limit: Optional[int] = None,
    ) -> list[BatchJob]:
        if after:
            after_index = next(
                (
                    index
                    for index, job in enumerate(jobs)
                    if job.job_id == after or job.status.job_id == after
                ),
                -1,
            )
            if after_index < 0:
                return []
            jobs = jobs[after_index + 1 :]
        if limit is not None:
            jobs = jobs[:limit]
        return jobs


@pytest.mark.asyncio
async def test_async_create_job():
    """Test that JobEntityManager assigns job_id and calls handlers correctly."""
    # Create mock job entity manager
    mock_entity_manager = MockJobEntityManager(delay=0.05)

    # Create job manager with entity manager
    _set_current_loop_name("test_async_create_job")
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(mock_entity_manager)

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
    submitted_session_id, submitted_spec, submitted_request_count = (
        mock_entity_manager.submitted_jobs[0]
    )
    assert submitted_session_id == session_id
    assert submitted_spec.input_file_id == "test-input-1"
    assert submitted_request_count == 0

    # Verify job was added to progress jobs since MockJobEntityManager set initial state to in_progress
    assert job_id in job_manager._in_progress_jobs
    job = job_manager._in_progress_jobs[job_id]
    assert job.session_id == session_id
    assert job.status.job_id == job_id

    # Verify the future was cleaned up
    assert session_id not in job_manager._bridge._creating_jobs


@pytest.mark.asyncio
async def test_get_job_republishes_untracked_entity_manager_job():
    _set_current_loop_name("test_get_job_republishes")
    entity_manager = MockJobEntityManager(delay=0.0)
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(entity_manager)
    scheduler = _CapturingScheduler()
    job_manager.set_scheduler(scheduler)

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="recovery-job",
            namespace="default",
            uid="recovery-uid",
            creationTimestamp=datetime.now(),
        ),
        spec=BatchJobSpec(
            input_file_id="f-recovery",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="recovery-job-id",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(),
        ),
    )
    entity_manager.jobs[batch_job.job_id] = batch_job

    resolved_job = await job_manager.get_job(batch_job.job_id)

    assert resolved_job is not None
    assert resolved_job.job_id == batch_job.job_id
    assert batch_job.job_id in scheduler.appended_job_ids
    assert batch_job.job_id in job_manager._pending_jobs


@pytest.mark.asyncio
async def test_expire_job_persists_via_entity_manager():
    """Regression: under the db-default an entity manager is ALWAYS wired, so a
    pending job past its completion window must still be expired AND persisted
    via the store. Previously expire_job early-returned whenever an entity
    manager was present, silently skipping expiry."""
    recorded: List[BatchJob] = []

    class _RecordingEM(MockJobEntityManager):
        async def update_job_status(self, job: BatchJob) -> None:
            recorded.append(job)

    _set_current_loop_name("test_expire_job_persists")
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(_RecordingEM(delay=0.0))

    job = BatchJob(
        sessionID="sess-expire",
        typeMeta=TypeMeta(apiVersion="v1", kind="BatchJob"),
        metadata=ObjectMeta(resourceVersion="1", creationTimestamp=datetime.now()),
        spec=BatchJobSpec(
            input_file_id="f-expire",
            endpoint="/v1/chat/completions",
            completion_window=86400,
        ),
        status=BatchJobStatus(
            jobID="job-expire",
            state=BatchJobState.CREATED,
            createdAt=datetime.now(),
        ),
    )
    job_manager._pending_jobs[job.job_id] = job

    assert await job_manager.expire_job(job.job_id) is True
    assert len(recorded) == 1
    assert recorded[0].status.state == BatchJobState.FINALIZED
    assert recorded[0].status.expired_at is not None


@pytest.mark.asyncio
async def test_admit_persists_in_progress_transition(monkeypatch):
    """The VALIDATING->IN_PROGRESS transition in admit() must be flushed to the
    metastore so the console reflects execution start. Without it the job
    appears stuck at 'scheduling' (the CREATED mapping) until finalize flushes
    every timestamp at once ("flash in")."""
    recorded: List[BatchJob] = []

    class _RecordingEM(MockJobEntityManager):
        async def update_job_status(self, job: BatchJob) -> None:
            recorded.append(job.model_copy(deep=True))
            await super().update_job_status(job)

    _set_current_loop_name("test_admit_persists_in_progress")
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(_RecordingEM(delay=0.0))

    async def _ok_validate(self, job):
        validated_status = job.status.model_copy(deep=True)
        validated_status.request_counts.total = 1
        await self._progress_manager.mark_job_validated(job.job_id, validated_status)

    monkeypatch.setattr(
        "aibrix.batch.job_driver.base.BaseJobDriver.validate_job",
        _ok_validate,
    )

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="admit-job",
            namespace="default",
            uid="admit-uid",
            creationTimestamp=datetime.now(),
        ),
        spec=BatchJobSpec(
            input_file_id="f-admit",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="admit-job-id",
            state=BatchJobState.CREATED,
            createdAt=datetime.now(),
            inProgressAt=None,
        ),
    )
    job_manager._pending_jobs["admit-job-id"] = batch_job
    assert job_manager._job_entity_manager is not None
    job_manager._job_entity_manager.jobs["admit-job-id"] = batch_job.model_copy(
        deep=True
    )

    driver = await job_manager.admit("admit-job-id")

    assert driver is not None
    meta = job_manager._in_progress_jobs["admit-job-id"]
    assert meta.status.state == BatchJobState.IN_PROGRESS
    assert meta.status.in_progress_at is not None
    # admit() may persist the initial VALIDATING snapshot before the
    # validating->in-progress transition; the key regression guard is that an
    # IN_PROGRESS snapshot is persisted during admit().
    in_progress_updates = [
        job for job in recorded if job.status.state == BatchJobState.IN_PROGRESS
    ]
    assert len(in_progress_updates) == 1
    assert in_progress_updates[0].status.in_progress_at is not None


@pytest.mark.asyncio
@pytest.mark.parametrize(
    ("recovered_state", "has_in_progress_at"),
    [
        (BatchJobState.IN_PROGRESS, True),
        (BatchJobState.FINALIZING, True),
    ],
)
async def test_admit_recovered_job_skips_validation_persist_roundtrip(
    monkeypatch, recovered_state: BatchJobState, has_in_progress_at: bool
):
    recorded: List[BatchJob] = []
    validate_calls: list[str] = []

    class _RecordingEM(MockJobEntityManager):
        async def update_job_status(self, job: BatchJob) -> None:
            recorded.append(job.model_copy(deep=True))
            await super().update_job_status(job)

    class _RecoveredDriver:
        async def validate_job(self, job: BatchJob) -> None:
            validate_calls.append(job.job_id)

    _set_current_loop_name("test_admit_recovered_job")
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(_RecordingEM(delay=0.0))

    fake_driver = _RecoveredDriver()
    monkeypatch.setattr(
        "aibrix.batch.batch_manager.create_job_driver",
        lambda context, progress_manager, entity_manager, job, inference_client: (
            fake_driver
        ),
    )

    recovered_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="recovered-job",
            namespace="default",
            uid="recovered-uid",
            creationTimestamp=datetime.now(),
            resourceVersion=None,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="f-recovered",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="recovered-job-id",
            state=recovered_state,
            createdAt=datetime.now(),
            inProgressAt=datetime.now() if has_in_progress_at else None,
        ),
    )
    job_manager._pending_jobs["recovered-job-id"] = recovered_job
    assert isinstance(job_manager._job_entity_manager, MockJobEntityManager)
    job_manager._job_entity_manager.jobs["recovered-job-id"] = recovered_job.model_copy(
        deep=True
    )

    driver = await job_manager.admit("recovered-job-id")

    assert driver is fake_driver
    assert validate_calls == []
    assert recorded == []
    assert "recovered-job-id" not in job_manager._pending_jobs
    assert "recovered-job-id" in job_manager._in_progress_jobs
    resumed = job_manager._in_progress_jobs["recovered-job-id"]
    assert resumed.status.state == recovered_state


@pytest.mark.asyncio
async def test_finalizing_transition_persists():
    """The explicit mark_job_finalizing() transition must be flushed to the
    metastore so the console can show 'finalizing' before the terminal flush.
    The per-request completion bitmap is covered by test_job_progress_tracker."""

    recorded: List[BatchJob] = []

    class _RecordingEM(MockJobEntityManager):
        async def update_job_status(self, job: BatchJob) -> None:
            recorded.append(job.model_copy(deep=True))

    _set_current_loop_name("test_finalizing_persists")
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(_RecordingEM(delay=0.0))

    batch_job = BatchJob(
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta(
            name="fin-job",
            namespace="default",
            uid="fin-uid",
            creationTimestamp=datetime.now(),
        ),
        spec=BatchJobSpec(
            input_file_id="f-fin",
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID="fin-job-id",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(),
            inProgressAt=datetime.now(),
        ),
    )
    batch_job.status.request_counts.total = 1
    meta = JobMetaInfo(batch_job)
    job_manager._in_progress_jobs["fin-job-id"] = meta
    job_manager._job_entity_manager.jobs["fin-job-id"] = batch_job.model_copy(deep=True)

    await job_manager.mark_job_finalizing("fin-job-id")

    assert meta.status.state == BatchJobState.FINALIZING
    assert meta.status.finalizing_at is not None
    assert len(recorded) == 1
    assert recorded[0].status.state == BatchJobState.FINALIZING
    assert recorded[0].status.finalizing_at is not None


@pytest.mark.asyncio
async def test_async_create_job_with_timeout():
    """Test that create_job throws error when timeout occurs."""
    # Create mock entity manager with long delay (longer than timeout)
    mock_entity_manager = MockJobEntityManager(delay=2.0)

    _set_current_loop_name("test_async_create_job_with_timeout")
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(mock_entity_manager)

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
    assert session_id not in job_manager._bridge._creating_jobs

    # Verify no job was added to _in_progress_jobs (since timeout occurred)
    assert len(job_manager._in_progress_jobs) == 0

    # Wait for job to be added.
    await asyncio.sleep(3.0)

    # Verify the job will be ignore by job_manager
    assert len(job_manager._in_progress_jobs) == 0
    all_jobs = await job_manager.list_jobs()
    assert len(all_jobs) == 0


@pytest.mark.asyncio
async def test_async_create_job_throws_error():
    """Test that create_job throws error when job submission fails."""
    # Create mock entity manager that fails
    mock_entity_manager = MockJobEntityManager()
    mock_entity_manager.should_fail = True

    _set_current_loop_name("test_async_create_job_throws_error")
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(mock_entity_manager)

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
    assert session_id not in job_manager._bridge._creating_jobs
    assert len(job_manager._pending_jobs) == 0


@pytest.mark.asyncio
async def test_multiple_concurrent_job_creation():
    """Test creating multiple jobs concurrently."""
    mock_entity_manager = MockJobEntityManager(delay=0.1)

    _set_current_loop_name("test_multiple_concurrent_job_creation")
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(mock_entity_manager)

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
        assert job_id in job_manager._in_progress_jobs
        job = job_manager._in_progress_jobs[job_id]
        assert job.session_id == session_ids[i]

    # Verify all futures were cleaned up
    assert len(job_manager._bridge._creating_jobs) == 0

    # Verify all jobs were submitted to entity manager
    assert len(mock_entity_manager.submitted_jobs) == 3


def _listed_job(job_id: str, created_at: datetime) -> BatchJob:
    return BatchJob(
        sessionID=f"session-{job_id}",
        typeMeta=TypeMeta(apiVersion="v1", kind="BatchJob"),
        metadata=ObjectMeta(
            resourceVersion="1",
            creationTimestamp=created_at,
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
            input_file_id=f"input-{job_id}",
            completion_window=CompletionWindow.TWENTY_FOUR_HOURS.expires_at(),
        ),
        status=BatchJobStatus(
            jobID=job_id,
            state=BatchJobState.CREATED,
            createdAt=created_at,
        ),
    )


@pytest.mark.asyncio
async def test_list_jobs_paginates_without_entity_manager():
    job_manager = _job_manager()
    first = _listed_job("job-1", datetime(2024, 1, 1, 0, 0, 1))
    second = _listed_job("job-2", datetime(2024, 1, 1, 0, 0, 2))
    third = _listed_job("job-3", datetime(2024, 1, 1, 0, 0, 3))

    job_manager._pending_jobs[first.job_id] = first
    job_manager._in_progress_jobs[second.job_id] = second
    job_manager._done_jobs[third.job_id] = third

    page1 = await job_manager.list_jobs(limit=2)
    assert [job.job_id for job in page1] == ["job-3", "job-2"]

    page2 = await job_manager.list_jobs(after="job-2", limit=2)
    assert [job.job_id for job in page2] == ["job-1"]

    assert await job_manager.list_jobs(after="missing-job", limit=2) == []


@pytest.mark.asyncio
async def test_list_jobs_delegates_pagination_to_entity_manager():
    mock_entity_manager = MockJobEntityManager(delay=0.0)
    job_manager = _job_manager()
    await job_manager.set_job_entity_manager(mock_entity_manager)

    first = _listed_job("job-1", datetime(2024, 1, 1, 0, 0, 1))
    second = _listed_job("job-2", datetime(2024, 1, 1, 0, 0, 2))
    third = _listed_job("job-3", datetime(2024, 1, 1, 0, 0, 3))

    mock_entity_manager.active_jobs = {
        first.job_id: first,
        second.job_id: second,
        third.job_id: third,
    }

    page1 = await job_manager.list_jobs(limit=2)
    assert [job.job_id for job in page1] == ["job-3", "job-2"]

    page2 = await job_manager.list_jobs(after="job-2", limit=2)
    assert [job.job_id for job in page2] == ["job-1"]
