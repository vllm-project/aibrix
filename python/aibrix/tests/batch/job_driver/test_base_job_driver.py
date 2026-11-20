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
from contextlib import asynccontextmanager
from datetime import datetime, timedelta
from typing import cast
from unittest.mock import patch

import pytest

from aibrix.batch.batch_manager import BatchManager
from aibrix.batch.client.engine import DispatchStatsSnapshot
from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.batch.job_driver import (
    BaseJobDriver,
    ExternalRuntime,
    NoopRuntime,
    TerminateResult,
)
from aibrix.batch.job_driver import base as base_module
from aibrix.batch.job_driver.running_jobs import RunningJobs
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    Condition,
    ConditionStatus,
    ConditionType,
    ObjectMeta,
    TypeMeta,
)
from aibrix.batch.job_entity import batch_job as batch_job_module
from aibrix.batch.state import JobMetaInfo
from aibrix.batch.worker import SingleJobRunner
from aibrix.context.infra import InfrastructureContext
from aibrix.metadata.core.metrics import Emitter, T, metrics_names


def _make_job(
    *,
    file_id: str = "input-1",
    endpoint: str = "/v1/chat/completions",
) -> BatchJob:
    return BatchJob(
        typeMeta=TypeMeta(apiVersion="v1", kind="BatchJob"),
        metadata=ObjectMeta(
            resourceVersion="1",
            creationTimestamp=datetime.now(),
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id=file_id,
            endpoint=endpoint,
            completion_window=86400,
        ),
        status=BatchJobStatus(
            jobID="job-1",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(),
        ),
    )


def _make_driver(job: BatchJob) -> BaseJobDriver:
    return BaseJobDriver(
        InfrastructureContext(),
        SingleJobRunner(job),
        ExternalRuntime(None),
    )


@pytest.mark.asyncio
async def test_accepted_cancel_before_session_finalizes_as_cancelled():
    context = InfrastructureContext()
    manager = BatchManager(context)
    job = JobMetaInfo(_make_job())
    job.status.request_counts.total = 1
    runtime = NoopRuntime(context)
    driver = BaseJobDriver(context, manager, runtime, job)
    job._job_driver = driver
    manager._in_progress_jobs[job.job_id] = job

    with patch.object(
        manager, "update_job_status", wraps=manager.update_job_status
    ) as update_status:
        result = await manager.cancel_job(job.job_id)
        stored = await manager.get_job(job.job_id)
        assert stored is not None
        cancelling_status = stored.status.model_copy(deep=True)
        assert result == TerminateResult.ACCEPTED
        assert await driver.terminate(stored) == TerminateResult.ALREADY_REQUESTED

        await asyncio.wait_for(driver.execute(job.job_id), timeout=5)

        finished = await manager.get_job(job.job_id)
        assert finished is not None
        assert finished.status.state == BatchJobState.FINALIZED
        assert finished.status.condition == ConditionType.CANCELLED
        assert finished.status.cancelled_at is not None
        assert finished.status.completed_at is None
        assert cancelling_status.state == BatchJobState.CANCELLING
        assert cancelling_status.check_condition(ConditionType.CANCELLED)
        update_status.assert_awaited_once()


@pytest.mark.asyncio
@pytest.mark.parametrize("validation_fails", [False, True], ids=["success", "failure"])
async def test_cancel_validating_job_with_live_driver_is_accepted(validation_fails):
    context = InfrastructureContext()
    manager = BatchManager(context)
    job = _make_job()
    job.status.state = BatchJobState.CREATED
    manager._pending_jobs[job.job_id] = job
    driver = BaseJobDriver(context, manager, NoopRuntime(context), job)
    validation_started = asyncio.Event()
    release_validation = asyncio.Event()

    async def blocked_validation(*args):
        validation_started.set()
        await release_validation.wait()
        return 1, "invalid input" if validation_fails else None

    with (
        patch("aibrix.batch.batch_manager.create_job_driver", return_value=driver),
        patch.object(
            base_module.storage, "read_job_input_info", return_value=(1, True)
        ),
        patch.object(
            base_module.storage,
            "validate_job_input_file",
            side_effect=blocked_validation,
        ),
    ):
        admission = asyncio.create_task(manager.admit(job.job_id))
        try:
            await asyncio.wait_for(validation_started.wait(), timeout=5)
            assert await manager.cancel_job(job.job_id) == TerminateResult.ACCEPTED
            stored = await manager.get_job(job.job_id)
            assert stored is not None
            assert stored.status.state == BatchJobState.CANCELLING
            assert stored.status.check_condition(ConditionType.CANCELLED)
            release_validation.set()
            admitted_driver = await asyncio.wait_for(admission, timeout=5)
            assert admitted_driver is (None if validation_fails else driver)
            stored = await manager.get_job(job.job_id)
            assert stored is not None
            assert stored.status.in_progress_at is None
            if not validation_fails:
                assert stored.status.state == BatchJobState.CANCELLING
                await asyncio.wait_for(driver.execute(job.job_id), timeout=5)
            finished = await manager.get_job(job.job_id)
            assert finished is not None
            assert finished.status.state == BatchJobState.FINALIZED
            assert finished.status.check_condition(ConditionType.CANCELLED)
            assert finished.status.completed_at is None
            if validation_fails:
                assert finished.status.condition == ConditionType.FAILED
                assert finished.status.failed_at is not None
                assert finished.status.cancelled_at is None
                assert finished.status.errors is not None
                assert (
                    finished.status.errors[0].code == BatchJobErrorCode.VALIDATION_ERROR
                )
            else:
                assert finished.status.condition == ConditionType.CANCELLED
                assert finished.status.cancelled_at is not None
        finally:
            admission.cancel()
            await asyncio.gather(admission, return_exceptions=True)


@pytest.mark.asyncio
@pytest.mark.parametrize("state", [BatchJobState.CREATED, BatchJobState.VALIDATING])
async def test_cancel_without_live_driver_finalizes_immediately(state):
    manager = BatchManager(InfrastructureContext())
    job = _make_job()
    job.status.state = state
    manager._pending_jobs[job.job_id] = job
    assert await manager.cancel_job(job.job_id) == TerminateResult.ACCEPTED
    finished = await manager.get_job(job.job_id)
    assert finished is not None
    assert finished.status.state == BatchJobState.FINALIZED
    assert finished.status.condition == ConditionType.CANCELLED
    assert finished.status.cancelled_at is not None
    assert finished.status.completed_at is None


@pytest.mark.asyncio
@pytest.mark.parametrize(
    "state",
    [BatchJobState.CANCELLING, BatchJobState.FINALIZING, BatchJobState.FINALIZED],
)
async def test_late_validation_preserves_current_job(state):
    manager = BatchManager(InfrastructureContext())
    job = JobMetaInfo(_make_job())
    stale_status = job.status.model_copy(deep=True)
    stale_status.state = BatchJobState.VALIDATING
    stale_status.request_counts.total = 1
    job.status.state = state
    if state == BatchJobState.FINALIZED:
        manager._done_jobs[job.job_id] = job
    else:
        manager._in_progress_jobs[job.job_id] = job
    assert await manager.mark_job_validated(job.job_id, stale_status) is job
    assert job.status.state == state
    assert job.status.request_counts.total == 0
    assert job.status.in_progress_at is None


@pytest.mark.asyncio
async def test_accepted_cancel_after_session_finalizes_as_cancelled():
    context = InfrastructureContext()
    manager = BatchManager(context)
    job = JobMetaInfo(_make_job())
    job.status.request_counts.total = 1
    runtime = NoopRuntime(context)
    driver = BaseJobDriver(context, manager, runtime, job)
    job._job_driver = driver
    manager._in_progress_jobs[job.job_id] = job
    finish_stopped_job = driver._finish_stopped_job

    async def complete_requests(*args, **kwargs):
        assert runtime._active_task is not None
        status = job.status.model_copy(deep=True)
        status.request_counts.completed = 1
        return await manager.update_job_status(job.job_id, status)

    async def cancel_after_session(executed_job):
        assert runtime._active_task is None
        assert runtime._progress_manager is None
        assert executed_job.status.state == BatchJobState.IN_PROGRESS
        assert executed_job.status.request_counts.completed == 1
        assert executed_job.status.condition == ConditionType.COMPLETED
        assert await manager.cancel_job(job.job_id) == TerminateResult.ACCEPTED
        return await finish_stopped_job(executed_job)

    with (
        patch.object(driver, "_prepare_execution_job", return_value=job),
        patch.object(driver, "run_job", side_effect=complete_requests),
        patch.object(
            driver, "_finish_stopped_job", side_effect=cancel_after_session
        ) as finish,
    ):
        await asyncio.wait_for(driver.execute(job.job_id), timeout=5)
        finish.assert_awaited_once()

    finished = await manager.get_job(job.job_id)
    assert finished is not None
    assert finished.status.request_counts.completed == 1
    assert finished.status.state == BatchJobState.FINALIZED
    assert finished.status.condition == ConditionType.CANCELLED
    assert finished.status.cancelled_at is not None
    assert finished.status.completed_at is None


def test_assign_worker_id_normalizes_runtime_owner_ref_slashes():
    driver = BaseJobDriver(
        InfrastructureContext(),
        cast(RunningJobs, None),
    )
    driver._worker_token = "token1234"

    worker_id = driver._assign_worker_id("cluster-a/default/workload-1")

    assert worker_id == "cluster-a-default-workload-1-token1234"


def test_ensure_batch_job_error_records_unknown_exception_source():
    def raise_unknown():
        raise RuntimeError("driver boom")

    try:
        raise_unknown()
    except RuntimeError as exc:
        error = BaseJobDriver._ensure_batch_job_error(exc)

    assert error.code == BatchJobErrorCode.INTERNAL_ERROR.value
    assert error.message.startswith("RuntimeError: driver boom")
    assert "(source: test_base_job_driver.py:" in error.message
    assert error.param is None
    assert error.line is None


def test_log_failed_includes_input_line_and_custom_id():
    driver = BaseJobDriver(
        InfrastructureContext(),
        cast(RunningJobs, None),
    )
    job = _make_job()
    job.status.job_id = "job-79"
    error = BatchJobError(
        code=BatchJobErrorCode.INTERNAL_ERROR,
        message="RuntimeError: boom",
        param="custom_id=req-79",
        line=79,
    )

    with patch.object(base_module.logger, "error") as mock_error:
        driver._log_failed(job, error)

    mock_error.assert_called_once_with(
        "Failed to execute job",
        job_id="job-79",
        error_code=BatchJobErrorCode.INTERNAL_ERROR.value,
        error="RuntimeError: boom",
        line=79,
        param="custom_id=req-79",
    )


def test_make_failure_error_prefers_explicit_job_context_for_normalization_log():
    driver = BaseJobDriver(
        InfrastructureContext(),
        cast(RunningJobs, None),
    )

    with patch.object(batch_job_module.logger, "error") as mock_error:
        try:
            raise RuntimeError("driver boom")
        except RuntimeError as exc:
            error = driver._make_failure_error(exc, job_id="job-explicit")

    assert error.code == BatchJobErrorCode.INFERENCE_FAILED.value
    assert error.message.startswith("RuntimeError: driver boom")
    _, kwargs = mock_error.call_args
    assert kwargs["job_id"] == "job-explicit"


def test_emit_finalization_phase_metric_uses_driver_boundary(monkeypatch):
    metric_calls: list[tuple[str, float, dict[str, str]]] = []

    def _record_timer(name, value, *tags):
        metric_calls.append((name, value, {tag.name: tag.value for tag in tags}))

    monkeypatch.setattr(Emitter, "timer", _record_timer)
    job = _make_job()
    driver = _make_driver(job)
    finalizing_at = datetime(2024, 1, 1, 0, 0, 0)
    finalization_done_at = finalizing_at + timedelta(seconds=7)

    driver._emit_finalization_phase_metric(
        job,
        finalization_started_at=finalizing_at,
        finalization_ended_at=finalization_done_at,
    )

    assert len(metric_calls) == 1
    name, value, tags = metric_calls[0]
    assert (
        name == f"{metrics_names.METRIC_METADATA_BATCH_API_JOB_EXECUTION_PHASE_TIME}.ms"
    )
    assert value == 7000.0
    assert tags["phase"] == "finalization"
    assert tags["phase_order"] == "060"
    assert tags["event"] == "Batch job latency telemetry"
    assert tags["log_map"] == "phase_duration_ms"
    assert tags["log_key"] == "finalization"


class _DeadlineStopRuntime:
    provisions = True

    def __init__(self) -> None:
        self._deadline_reached = False

    def cancelled(self) -> bool:
        return False

    def runtime_deadline_reached(self) -> bool:
        return self._deadline_reached

    def execution_key(self, job: BatchJob) -> str | None:
        del job
        return "fake"

    @asynccontextmanager
    async def session(self, job, job_id, **kwargs):
        del job, job_id, kwargs
        yield base_module.Endpoint(source=None)

    async def on_prepared(self) -> None:
        return None

    async def await_completion(self):
        self._deadline_reached = True
        raise asyncio.CancelledError

    async def terminate(self, deleted_job, *, progress_manager=None):
        del deleted_job, progress_manager
        return base_module.TerminateResult.REJECTED

    async def cleanup(self, job):
        del job
        return None


def _patch_validation(
    monkeypatch,
    *,
    exists: bool = True,
    total: int = 1,
    validation_error: str | None = None,
):
    async def read_job_input_info(_job):
        del _job
        return object(), exists

    async def validate_job_input_file(file_id, endpoint):
        del file_id, endpoint
        return total, validation_error

    monkeypatch.setattr(base_module.storage, "read_job_input_info", read_job_input_info)
    monkeypatch.setattr(
        base_module.storage,
        "validate_job_input_file",
        validate_job_input_file,
    )


# ---- BaseJobDriver.validate_job owns semantic input validation. ----


def test_should_stop_before_proceed_when_job_expired():
    job = _make_job()
    driver = _make_driver(job)
    job.status.add_condition(
        Condition(
            type=ConditionType.EXPIRED,
            status=ConditionStatus.TRUE,
            lastTransitionTime=datetime.now(),
        )
    )

    assert driver._should_stop_before_proceed(job) is True


def test_emit_request_completion_metrics_counts_finished_requests(monkeypatch):
    job = _make_job()
    driver = _make_driver(job)
    metric_calls: list[tuple[str, float, tuple[str, ...]]] = []
    base_tags = (
        job.spec.endpoint,
        str(job.spec.completion_window),
        job.job_id,
        "none",
    )

    def _record_counter(name, value, *tags):
        metric_calls.append((name, value, tuple(tag.value for tag in tags)))

    monkeypatch.setattr(Emitter, "counter", _record_counter)

    driver._emit_request_completion_metrics(job, completed=3, failed=2)

    assert metric_calls == [
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_COMPLETED,
            3,
            (*base_tags, "success"),
        ),
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_COMPLETED,
            2,
            (*base_tags, "fail"),
        ),
    ]


@pytest.mark.asyncio
async def test_sync_completed_request_tasks_aggregates_failure_metrics(monkeypatch):
    job = _make_job()
    driver = _make_driver(job)
    metric_calls: list[tuple[str, float, dict[str, str]]] = []

    def _record_counter(name, value, *tags):
        metric_calls.append((name, value, {tag.name: tag.value for tag in tags}))

    async def _persist_worker_status(current_job):
        return current_job

    monkeypatch.setattr(Emitter, "counter", _record_counter)
    monkeypatch.setattr(driver, "_persist_worker_status", _persist_worker_status)

    http_500_tags = driver._request_failure_metric_tags(
        InferenceError(
            InferenceErrorCode.HTTP_ERROR,
            "backend rejected request",
            status_code=500,
        )
    )
    transport_tags = driver._request_failure_metric_tags(
        InferenceError(
            InferenceErrorCode.TRANSPORT_ERROR,
            "connection reset",
        )
    )

    synced, completed = await driver._sync_completed_request_tasks(
        job,
        [
            (1, True, http_500_tags),
            (2, True, http_500_tags),
            (3, True, transport_tags),
            (4, False, None),
            (5, True, None),
        ],
    )

    assert synced is job
    assert completed == 5
    assert metric_calls == [
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_COMPLETED,
            1,
            {
                "endpoint": job.spec.endpoint,
                "completion_window": str(job.spec.completion_window),
                "job_id": job.job_id,
                "console_job_id": "none",
                "result": "success",
            },
        ),
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_COMPLETED,
            4,
            {
                "endpoint": job.spec.endpoint,
                "completion_window": str(job.spec.completion_window),
                "job_id": job.job_id,
                "console_job_id": "none",
                "result": "fail",
            },
        ),
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_FAILURE,
            2,
            {
                "endpoint": job.spec.endpoint,
                "completion_window": str(job.spec.completion_window),
                "job_id": job.job_id,
                "console_job_id": "none",
                "error_code": "http_error",
                "status_code": "500",
            },
        ),
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_FAILURE,
            1,
            {
                "endpoint": job.spec.endpoint,
                "completion_window": str(job.spec.completion_window),
                "job_id": job.job_id,
                "console_job_id": "none",
                "error_code": "transport_error",
                "status_code": "none",
            },
        ),
    ]


def test_emit_dispatch_telemetry_emits_overview_gauges(monkeypatch):
    job = _make_job()
    driver = _make_driver(job)
    metric_calls: list[tuple[str, float, dict[str, str]]] = []

    def _record_gauge(name, value, *tags):
        metric_calls.append((name, value, {tag.name: tag.value for tag in tags}))

    monkeypatch.setattr(Emitter, "gauge", _record_gauge)

    driver._emit_dispatch_telemetry(
        job.job_id,
        DispatchStatsSnapshot(
            started=4,
            completed=3,
            failed=1,
            inflight=2,
            limit=8,
            max_inflight=5,
            window_started=4,
            window_completed=3,
            window_failed=1,
            avg_latency_seconds=0.4,
            p90_latency_seconds=0.8,
            avg_queue_wait_seconds=0.11,
            p90_queue_wait_seconds=0.33,
            avg_qps_wait_seconds=0.01,
            p90_qps_wait_seconds=0.02,
            avg_send_latency_seconds=0.04,
            p90_send_latency_seconds=0.05,
        ),
        1.0,
        final=False,
    )

    metric_map = {name: (value, tags) for name, value, tags in metric_calls}

    inflight_value, inflight_tags = metric_map[
        "metadata.batch.dispatch.telemetry.inflight"
    ]
    assert inflight_value == 2.0
    assert inflight_tags["job_id"] == job.job_id
    assert inflight_tags["event"] == "Batch dispatch telemetry summary"
    assert inflight_tags["log_context"] == "job_id"
    assert inflight_tags["log_field"] == "inflight"

    queue_value, queue_tags = metric_map[
        "metadata.batch.dispatch.telemetry.p90_queue_wait_seconds"
    ]
    assert queue_value == 0.33
    assert queue_tags["job_id"] == job.job_id
    assert queue_tags["log_field"] == "p90_queue_wait_seconds"

    summary_value, summary_tags = metric_map[
        "metadata.batch.dispatch.telemetry.summary"
    ]
    assert summary_value == 1.0
    assert summary_tags["job_id"] == job.job_id
    assert summary_tags["event"] == "Batch dispatch telemetry summary"
    assert summary_tags["log_context"] == "job_id"
    assert summary_tags["final"] == "false"
    assert T("flush", "true").value == summary_tags["flush"]


def test_dispatch_telemetry_enabled_uses_env_switch(monkeypatch):
    monkeypatch.setattr(base_module.envs, "BATCH_DISPATCH_TELEMETRY_ENABLED", False)
    assert base_module._dispatch_telemetry_enabled() is False

    monkeypatch.setattr(base_module.envs, "BATCH_DISPATCH_TELEMETRY_ENABLED", True)
    assert base_module._dispatch_telemetry_enabled() is True

    monkeypatch.setattr(
        base_module.envs,
        "BATCH_DISPATCH_TELEMETRY_INTERVAL_SECONDS",
        12.5,
    )
    assert base_module._telemetry_interval_seconds() == 12.5


def test_request_usage_metrics_emit_when_request_finishes(monkeypatch):
    job = _make_job()
    driver = _make_driver(job)
    metric_calls: list[tuple[str, float, tuple[str, ...]]] = []
    base_tags = (
        job.spec.endpoint,
        str(job.spec.completion_window),
        job.job_id,
        "none",
    )

    def _record_counter(name, value, *tags):
        metric_calls.append((name, value, tuple(tag.value for tag in tags)))

    monkeypatch.setattr(Emitter, "counter", _record_counter)

    emitted_usage = driver._accumulate_usage(
        job.job_id,
        "req-1",
        {
            "prompt_tokens": 11,
            "completion_tokens": 7,
            "prompt_tokens_details": {"cached_tokens": 5},
            "completion_tokens_details": {"reasoning_tokens": 3},
        },
    )
    driver._emit_request_usage_metrics(job, emitted_usage)

    duplicate_usage = driver._accumulate_usage(
        job.job_id,
        "req-1",
        {
            "prompt_tokens": 11,
            "completion_tokens": 7,
        },
    )
    driver._emit_request_usage_metrics(job, duplicate_usage)

    assert metric_calls == [
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_USAGE_TOKENS,
            11,
            (*base_tags, "input_token"),
        ),
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_USAGE_TOKENS,
            7,
            (*base_tags, "output_token"),
        ),
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_CACHED_TOKENS,
            5,
            base_tags,
        ),
        (
            metrics_names.METRIC_METADATA_BATCH_JOBDRIVER_REQUEST_REASONING_TOKENS,
            3,
            base_tags,
        ),
    ]


@pytest.mark.asyncio
async def test_validate_job_records_request_count_after_success(monkeypatch):
    # A successful validation pass must persist the counted request total onto
    # the in-memory job snapshot before execution starts.
    job = _make_job()
    driver = _make_driver(job)
    _patch_validation(monkeypatch, total=3)
    metric_calls: list[tuple[str, float, dict[str, str]]] = []

    def _record_timer(name, value, *tags):
        metric_calls.append((name, value, {tag.name: tag.value for tag in tags}))

    monkeypatch.setattr(Emitter, "timer", _record_timer)

    await driver.validate_job(job)

    validated = await driver._progress_manager.get_job(job.job_id)
    assert validated is not None
    assert validated.status.request_counts.total == 3
    assert len(metric_calls) == 1
    name, value, tags = metric_calls[0]
    assert (
        name == f"{metrics_names.METRIC_METADATA_BATCH_API_JOB_EXECUTION_PHASE_TIME}.ms"
    )
    assert value >= 0
    assert tags["job_id"] == job.job_id
    assert tags["phase"] == "validation"
    assert tags["phase_order"] == "020"
    assert tags["event"] == "Batch job latency telemetry"
    assert tags["log_map"] == "phase_duration_ms"
    assert tags["log_key"] == "validation"


@pytest.mark.asyncio
async def test_validate_job_rejects_unknown_input_file(monkeypatch):
    # NOTE: When the input file does not exist, the local storage's
    # readline_iter currently yields zero lines instead of raising
    # FileNotFoundError, so the route reports the input as empty rather
    # than not-found. This documents the current behavior; if storage
    # is updated to surface FileNotFoundError, the assertion should
    # tighten to match "not found".
    job = _make_job(file_id="does-not-exist")
    driver = _make_driver(job)
    _patch_validation(monkeypatch, exists=False)

    with pytest.raises(BatchJobError) as excinfo:
        await driver.validate_job(job)

    assert excinfo.value.code == BatchJobErrorCode.INVALID_INPUT_FILE
    assert excinfo.value.message == "input file not found"


@pytest.mark.asyncio
async def test_validate_job_rejects_input_missing_custom_id(monkeypatch):
    job = _make_job()
    driver = _make_driver(job)
    _patch_validation(
        monkeypatch,
        total=0,
        validation_error="Line 1: Missing required field 'custom_id'",
    )

    with pytest.raises(BatchJobError) as excinfo:
        await driver.validate_job(job)

    assert excinfo.value.code == BatchJobErrorCode.VALIDATION_ERROR
    assert "custom_id" in excinfo.value.message


@pytest.mark.asyncio
async def test_validate_job_rejects_input_url_mismatching_batch_endpoint(
    monkeypatch,
):
    job = _make_job()
    driver = _make_driver(job)
    _patch_validation(
        monkeypatch,
        total=0,
        validation_error=(
            "Line 1: Request URL '/v1/embeddings' does not match batch endpoint "
            "'/v1/chat/completions'"
        ),
    )
    # mismatch

    with pytest.raises(BatchJobError) as excinfo:
        await driver.validate_job(job)

    assert excinfo.value.code == BatchJobErrorCode.VALIDATION_ERROR
    assert "does not match" in excinfo.value.message


@pytest.mark.asyncio
async def test_validate_job_rejects_embeddings_input_missing_input_field(
    monkeypatch,
):
    # Embeddings body must carry 'input'; here we omit it.
    job = _make_job(endpoint="/v1/embeddings")
    driver = _make_driver(job)
    _patch_validation(
        monkeypatch,
        total=0,
        validation_error="Line 1: Missing required field 'input' for /v1/embeddings",
    )

    with pytest.raises(BatchJobError) as excinfo:
        await driver.validate_job(job)

    assert excinfo.value.code == BatchJobErrorCode.VALIDATION_ERROR
    assert "input" in excinfo.value.message


@pytest.mark.asyncio
async def test_validate_job_rejects_chat_messages_not_list(monkeypatch):
    job = _make_job()
    driver = _make_driver(job)
    _patch_validation(
        monkeypatch,
        total=0,
        validation_error=(
            "Line 1: Field 'messages' must be a list for /v1/chat/completions"
        ),
    )

    with pytest.raises(BatchJobError) as excinfo:
        await driver.validate_job(job)

    assert excinfo.value.code == BatchJobErrorCode.VALIDATION_ERROR
    assert "messages" in excinfo.value.message


@pytest.mark.asyncio
async def test_validate_job_rejects_empty_input_file(monkeypatch):
    job = _make_job()
    driver = _make_driver(job)
    _patch_validation(monkeypatch, total=0, validation_error=None)

    with pytest.raises(BatchJobError) as excinfo:
        await driver.validate_job(job)

    assert excinfo.value.code == BatchJobErrorCode.EMPTY_INPUT_FILE
    assert excinfo.value.message == "input file is empty"


def test_result_ids_preserve_distinct_uuid_values(monkeypatch):
    """Result ids must keep the full UUID, not a truncated prefix.

    The id used to be ``uuid.uuid4().hex[:5]``. Five hex digits are only
    ~1M values, so ids repeated within a batch of a few thousand lines (about
    38% at 1,000 lines, 98.6% at 3,000; see #3368), and consumers indexing
    results by ``id`` merged unrelated records.

    The two UUIDs below share their first five hex digits, so a ``[:5]``
    truncation yields the same id for both and this test fails; the full
    ``.hex`` keeps them distinct.
    """
    from uuid import UUID

    job = _make_job()
    driver = _make_driver(job)
    first = UUID("12345000-0000-4000-8000-000000000001")
    second = UUID("12345fff-ffff-4fff-8fff-ffffffffffff")
    values = iter([first, second])
    monkeypatch.setattr(base_module.uuid, "uuid4", lambda: next(values))

    ids = [
        driver._build_response(
            f"request-{i}",
            job.job_id,
            i,
            job.spec,
            request_output="ok",
        )["id"]
        for i in range(2)
    ]

    assert ids == [first.hex, second.hex]
