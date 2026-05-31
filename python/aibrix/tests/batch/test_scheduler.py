import time
from datetime import datetime

import pytest

from aibrix.batch.job_entity import BatchJobState, BatchJobStatus
from aibrix.batch.scheduler import BasicCongestionControl, JobScheduler
from aibrix.context import InfrastructureContext


class FakeProgressManager:
    def __init__(self, job_id_status=None):
        self.job_id_status = job_id_status or {}
        self.validated_job_ids = []

    async def get_job_status(self, job_id):
        if job_id not in self.job_id_status:
            return None
        return BatchJobStatus(
            jobID=job_id, state=self.job_id_status[job_id], createdAt=datetime.now()
        )

    async def validate_job(self, job_id, inference_client=None):
        self.validated_job_ids.append(job_id)
        if job_id not in self.job_id_status:
            raise ValueError(f"job_id {job_id} not found in job_id_status")
        self.job_id_status[job_id] = BatchJobState.IN_PROGRESS
        return True


def _make_scheduler(pool_size, progress_manager):
    scheduler = JobScheduler(
        InfrastructureContext(),
        progress_manager,
        None,
        pool_size,
        cc_controller=BasicCongestionControl(pool_size),
    )
    scheduler._inference_client = None
    scheduler.interval = 0
    return scheduler


@pytest.mark.asyncio
async def test_round_robin_get_job_prioritizes_existing_pool_jobs():
    progress_manager = FakeProgressManager(
        {
            "finished-job": BatchJobState.FINALIZED,
            "in-progress-job": BatchJobState.IN_PROGRESS,
            "new-job1": BatchJobState.CREATED,
            "new-job2": BatchJobState.CREATED,
            "new-job3": BatchJobState.CREATED,
        },
    )
    scheduler = _make_scheduler(3, progress_manager)
    scheduler._CC_controller._running_job_pool = [
        "finished-job",
        "in-progress-job",
        None,
    ]
    scheduler._CC_controller._running_job_idx = 0
    scheduler.append_job("new-job1", time.time() + 60)
    scheduler.append_job("new-job2", time.time() + 60)
    scheduler.append_job("new-job3", time.time() + 60)

    next_job_id = await scheduler.round_robin_get_job()

    assert next_job_id == "new-job1"
    assert scheduler._CC_controller._running_job_pool == [
        "new-job1",
        "in-progress-job",
        "new-job2",
    ]
    await progress_manager.validate_job("new-job1")
    progress_manager.job_id_status["in-progress-job"] = BatchJobState.FINALIZED

    next_job_id = await scheduler.round_robin_get_job()
    assert next_job_id == "new-job2"
    assert scheduler._CC_controller._running_job_pool == [
        "new-job1",
        "new-job3",
        "new-job2",
    ]


@pytest.mark.asyncio
async def test_round_robin_get_job_fills_only_empty_slots():
    progress_manager = FakeProgressManager(
        {
            "running-job-a": BatchJobState.FINALIZED,
            "running-job-b": BatchJobState.IN_PROGRESS,
            "new-job-1": BatchJobState.CREATED,
            "new-job-2": BatchJobState.CREATED,
        }
    )
    scheduler = _make_scheduler(3, progress_manager)
    scheduler._CC_controller._running_job_pool = [
        "running-job-a",
        "running-job-b",
        None,
    ]
    scheduler.append_job("new-job-1", time.time() + 60)
    scheduler.append_job("new-job-2", time.time() + 60)

    next_job_id = await scheduler.round_robin_get_job()

    assert next_job_id == "new-job-1"
    assert scheduler._CC_controller._running_job_pool == [
        "new-job-1",
        "running-job-b",
        "new-job-2",
    ]
