import asyncio
import json
from datetime import datetime
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

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
from aibrix.batch.worker import BatchWorker, SingleJobRunner, worker_main


def _make_job(state: BatchJobState) -> BatchJob:
    job = BatchJob(
        typeMeta=TypeMeta(apiVersion="v1", kind="BatchJob"),
        metadata=ObjectMeta(
            resourceVersion="1",
            creationTimestamp=datetime.now(),
            deletionTimestamp=None,
        ),
        spec=BatchJobSpec(
            input_file_id="input-1",
            endpoint="/v1/chat/completions",
            completion_window=86400,
        ),
        status=BatchJobStatus(jobID="job-1", state=state, createdAt=datetime.now()),
    )
    return job


def test_load_job_from_env_restores_aibrix_client(monkeypatch):
    monkeypatch.setenv("JOB_NAME", "batch-job")
    monkeypatch.setenv("JOB_NAMESPACE", "default")
    monkeypatch.setenv("JOB_UID", "job-1")
    monkeypatch.setenv("BATCH_INPUT_FILE_ID", "file-1")
    monkeypatch.setenv("BATCH_ENDPOINT", "/v1/chat/completions")
    monkeypatch.setenv("BATCH_OUTPUT_FILE_ID", "out")
    monkeypatch.setenv("BATCH_TEMP_OUTPUT_FILE_ID", "tmp-out")
    monkeypatch.setenv("BATCH_ERROR_FILE_ID", "err")
    monkeypatch.setenv("BATCH_TEMP_ERROR_FILE_ID", "tmp-err")
    monkeypatch.setenv("LLM_READY_ENDPOINT", "http://localhost:8000/health")
    monkeypatch.setenv(
        "BATCH_AIBRIX",
        json.dumps(
            {
                "client": {
                    "max_concurrency": 64,
                    "adaptive_concurrency": True,
                    "adaptive_max_factor": 8,
                    "retry_policy": {"max_retries": 5},
                }
            }
        ),
    )

    job = BatchWorker().load_job_from_env()

    assert job.spec.aibrix is not None
    assert job.spec.aibrix.client is not None
    assert job.spec.aibrix.client.max_concurrency == 64
    assert job.spec.aibrix.client.retry_policy is not None
    assert job.spec.aibrix.client.retry_policy.max_retries == 5


@pytest.mark.asyncio
async def test_single_job_runner_marks_failed_and_done():
    # IN_PROGRESS -> FINALIZING on failure, recording the error.
    runner = SingleJobRunner(_make_job(BatchJobState.IN_PROGRESS))
    failed = await runner.mark_job_failed(
        "job-1", BatchJobError(code=BatchJobErrorCode.UNKNOWN_ERROR, message="boom")
    )
    assert failed.status.state == BatchJobState.FINALIZING
    assert failed.status.errors and failed.status.errors[0].message == "boom"

    # FINALIZING -> FINALIZED on done.
    finalizing_job = _make_job(BatchJobState.FINALIZING)
    runner2 = SingleJobRunner(finalizing_job)
    done = await runner2.mark_job_done(finalizing_job)
    assert done.status.state == BatchJobState.FINALIZED

    # get_job resolves by id only.
    assert (await runner2.get_job("job-1")) is not None
    assert (await runner2.get_job("missing")) is None


@pytest.mark.asyncio
async def test_single_job_runner_completes_out_of_order():
    job = _make_job(BatchJobState.IN_PROGRESS)
    job.status.request_counts.total = 2
    runner = SingleJobRunner(job)

    await runner.complete_job_request("job-1", 1)
    assert job.status.state == BatchJobState.IN_PROGRESS
    await runner.complete_job_request("job-1", 0)
    assert job.status.state == BatchJobState.FINALIZING


@pytest.mark.asyncio
async def test_run_returns_failure_when_execution_fails(monkeypatch):
    worker = BatchWorker()
    worker.llm_engine_base_url = "http://localhost:8000"
    batch_job = _make_job(BatchJobState.IN_PROGRESS)

    monkeypatch.setattr(worker, "load_job_from_env", lambda: batch_job)
    monkeypatch.setattr(
        worker, "wait_for_in_progress", AsyncMock(return_value=batch_job)
    )
    worker.health_checker = SimpleNamespace(wait_for_ready=AsyncMock(return_value=True))

    from aibrix.batch import worker as worker_module

    # The single-job execution raises; run() must swallow it and return 1.
    failing_driver = SimpleNamespace(
        execute=AsyncMock(side_effect=RuntimeError("boom"))
    )
    monkeypatch.setattr(worker_module, "SingleJobRunner", lambda job: object())
    monkeypatch.setattr(worker_module, "GatewayEndpointSource", lambda url: object())
    monkeypatch.setattr(worker_module, "ExternalRuntime", lambda src: object())
    monkeypatch.setattr(
        worker_module, "BaseJobDriver", lambda runner, runtime: failing_driver
    )

    result = await worker.run()

    assert result == 1
    failing_driver.execute.assert_awaited_once()


@pytest.mark.asyncio
async def test_worker_main_returns_failure_and_kills_engine(monkeypatch):
    loop = asyncio.get_running_loop()
    monkeypatch.setattr(loop, "add_signal_handler", lambda *args, **kwargs: None)

    fake_worker = SimpleNamespace(run=AsyncMock(return_value=1))
    killed = []

    from aibrix.batch import worker as worker_module

    monkeypatch.setattr(worker_module, "BatchWorker", lambda: fake_worker)
    monkeypatch.setattr(worker_module, "kill_llm_engine", lambda: killed.append(True))

    result = await worker_main()

    assert result == 1
    assert killed == [True]
