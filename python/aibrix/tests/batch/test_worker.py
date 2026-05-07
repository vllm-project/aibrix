import asyncio
from types import SimpleNamespace
from unittest.mock import AsyncMock

import pytest

from aibrix.batch.job_entity import BatchJobState
from aibrix.batch.worker import BatchWorker, worker_main


@pytest.mark.asyncio
async def test_execute_batch_job_raises_when_final_state_failed():
    worker = BatchWorker()
    worker.driver = SimpleNamespace(
        job_manager=SimpleNamespace(job_committed_handler=AsyncMock())
    )

    batch_job = SimpleNamespace(
        job_id="job-1",
        spec=SimpleNamespace(
            input_file_id="input-1",
            endpoint="/v1/chat/completions",
        ),
    )
    final_job = SimpleNamespace(
        status=SimpleNamespace(
            failed=True,
            condition=SimpleNamespace(value="failed"),
        )
    )

    async def _wait_for_finalizing(job_id: str, max_wait: int = 600):
        return final_job

    worker.wait_for_finalizing = _wait_for_finalizing

    with pytest.raises(RuntimeError, match="Batch job failed in worker: failed"):
        await worker.execute_batch_job(batch_job)

    worker.driver.job_manager.job_committed_handler.assert_awaited_once_with(batch_job)


@pytest.mark.asyncio
async def test_run_returns_failure_when_execute_batch_job_fails(monkeypatch):
    worker = BatchWorker()
    batch_job = SimpleNamespace(
        status=SimpleNamespace(state=BatchJobState.IN_PROGRESS),
        spec=SimpleNamespace(
            input_file_id="input-1",
            endpoint="/v1/chat/completions",
        ),
    )

    monkeypatch.setattr(worker, "load_job_from_env", lambda: batch_job)
    monkeypatch.setattr(
        worker, "wait_for_in_progress", AsyncMock(return_value=batch_job)
    )
    worker.health_checker = SimpleNamespace(wait_for_ready=AsyncMock(return_value=True))
    monkeypatch.setattr(
        worker,
        "execute_batch_job",
        AsyncMock(side_effect=RuntimeError("Batch job failed in worker: failed")),
    )

    fake_driver = SimpleNamespace(
        start=AsyncMock(),
        stop=AsyncMock(),
    )

    from aibrix.batch import worker as worker_module

    monkeypatch.setattr(worker_module, "BatchDriver", lambda **kwargs: fake_driver)

    result = await worker.run()

    assert result == 1
    fake_driver.start.assert_awaited_once()
    assert fake_driver.stop.await_count >= 1


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
