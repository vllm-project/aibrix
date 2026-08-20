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
from datetime import datetime
from typing import cast
from unittest.mock import patch

import pytest

from aibrix.batch.job_driver import BaseJobDriver, ExternalRuntime
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
from aibrix.batch.worker import SingleJobRunner
from aibrix.context.infra import InfrastructureContext
from aibrix.metadata.core.metrics import Emitter, metrics_names


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

    async def terminate(self, deleted_job):
        del deleted_job
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

    await driver.validate_job(job)

    validated = await driver._progress_manager.get_job(job.job_id)
    assert validated is not None
    assert validated.status.request_counts.total == 3


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
