from datetime import datetime

import pytest

from aibrix.batch.job_driver import BaseJobDriver, ExternalRuntime
from aibrix.batch.job_driver import base as base_module
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    ObjectMeta,
    TypeMeta,
)
from aibrix.batch.worker import SingleJobRunner


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
    return BaseJobDriver(SingleJobRunner(job), ExternalRuntime(None))


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
