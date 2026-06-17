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
import time
from datetime import datetime, timezone
from types import SimpleNamespace

import pytest

from aibrix.batch.batch_scheduler import BatchScheduler
from aibrix.batch.job_entity import BatchJobState, BatchJobStatus
from aibrix.context import InfrastructureContext

# Stand-in for the JobDriver admit() returns; the scheduler only forwards it.
_SENTINEL_DRIVER = object()


def _pending_job(job_id, due_epoch):
    """Stub pending job whose deadline (created_at + completion_window) equals
    ``due_epoch`` — created_at is epoch 0, so completion_window carries it."""
    return SimpleNamespace(
        job_id=job_id,
        status=SimpleNamespace(created_at=datetime.fromtimestamp(0, tz=timezone.utc)),
        spec=SimpleNamespace(completion_window=due_epoch),
    )


class FakeProgressManager:
    def __init__(self, job_id_status=None, pending=None, in_progress=None):
        self.job_id_status = job_id_status or {}
        self.validated_job_ids = []
        self.expired_job_ids = []
        self.failed_errors = {}
        self._pending = list(pending or [])
        self._in_progress = list(in_progress or [])

    async def get_job_status(self, job_id):
        if job_id not in self.job_id_status:
            return None
        return BatchJobStatus(
            jobID=job_id, state=self.job_id_status[job_id], createdAt=datetime.now()
        )

    async def admit(self, job_id):
        self.validated_job_ids.append(job_id)
        if job_id not in self.job_id_status:
            return None
        self.job_id_status[job_id] = BatchJobState.IN_PROGRESS
        # Mirror the real manager: admission removes the job from the pending
        # pool, so list_pending() no longer returns it.
        self._pending = [j for j in self._pending if j.job_id != job_id]
        return _SENTINEL_DRIVER

    async def expire_job(self, job_id):
        self.expired_job_ids.append(job_id)
        # An expired job is finalized in the registry, so it is no longer
        # schedulable afterwards.
        self.job_id_status.pop(job_id, None)
        self._pending = [j for j in self._pending if j.job_id != job_id]
        self._in_progress = [j for j in self._in_progress if j.job_id != job_id]
        return True

    async def list_pending(self):
        return list(self._pending)

    async def list_in_progress(self):
        return list(self._in_progress)


def _make_scheduler(progress_manager, pool_size=1):
    scheduler = BatchScheduler(
        InfrastructureContext(),
        progress_manager,
        pool_size=pool_size,
    )
    scheduler._idle_interval = 0
    return scheduler


@pytest.mark.asyncio
async def test_schedule_next_job_pops_fifo_and_validates():
    progress_manager = FakeProgressManager(
        {
            "job-1": BatchJobState.CREATED,
            "job-2": BatchJobState.CREATED,
        }
    )
    scheduler = _make_scheduler(progress_manager)
    scheduler.append_job("job-1")
    scheduler.append_job("job-2")

    first = await scheduler.schedule_next_job()
    second = await scheduler.schedule_next_job()
    assert first is not None and first[0] == "job-1"
    assert second is not None and second[0] == "job-2"
    assert progress_manager.validated_job_ids == ["job-1", "job-2"]


@pytest.mark.asyncio
async def test_schedule_next_job_skips_unadmittable_jobs():
    # "gone-job" is not admittable (admit returns None, e.g. it expired); the
    # scheduler must advance past it to the next admittable job.
    progress_manager = FakeProgressManager({"good-job": BatchJobState.CREATED})
    scheduler = _make_scheduler(progress_manager)
    scheduler.append_job("gone-job")
    scheduler.append_job("good-job")

    result = await scheduler.schedule_next_job()
    assert result is not None and result[0] == "good-job"
    # admit() is the gate: attempted on the gone job (None) then the good one.
    assert progress_manager.validated_job_ids == ["gone-job", "good-job"]


@pytest.mark.asyncio
async def test_expire_jobs_expires_due_jobs_via_manager():
    # expire_jobs derives deadlines from the registry's pending pool, not a
    # scheduler-side due list.
    now = time.time()
    progress_manager = FakeProgressManager(
        pending=[_pending_job("past", now - 1), _pending_job("future", now + 600)]
    )
    scheduler = _make_scheduler(progress_manager)

    await scheduler.expire_jobs()

    # Past-due jobs are expired through the manager (the registry is the single
    # source of truth); future jobs are untouched.
    assert progress_manager.expired_job_ids == ["past"]
    assert "future" not in progress_manager.expired_job_ids


def test_fifo_scheduling_policy_order_and_empty():
    from aibrix.batch.scheduling_policy import FIFOScheduling, SchedulingPolicy

    policy: SchedulingPolicy = FIFOScheduling()
    assert policy.empty() is True
    assert policy.next() is None

    policy.add("a")
    policy.add("b")
    assert policy.empty() is False
    assert policy.next() == "a"
    assert policy.next() == "b"
    assert policy.next() is None
    assert policy.empty() is True


@pytest.mark.asyncio
async def test_jobs_running_loop_dispatches_second_job_while_first_is_blocked(
    monkeypatch,
):
    scheduler = _make_scheduler(FakeProgressManager(), pool_size=2)
    first_job_entered = asyncio.Event()
    release_first_job = asyncio.Event()
    second_job_started = asyncio.Event()
    scheduled = []

    class _Driver:
        def __init__(self, job_id):
            self.job_id = job_id

        async def execute(self, job_id):
            assert job_id == self.job_id
            if job_id == "job-1":
                first_job_entered.set()
                await release_first_job.wait()
                return
            second_job_started.set()

    scheduled.extend(
        [
            ("job-1", _Driver("job-1")),
            ("job-2", _Driver("job-2")),
        ]
    )

    async def _schedule_next_job():
        if scheduled:
            return scheduled.pop(0)
        await asyncio.sleep(0)
        return None

    monkeypatch.setattr(scheduler, "schedule_next_job", _schedule_next_job)

    task = asyncio.create_task(scheduler.jobs_running_loop())
    try:
        await asyncio.wait_for(first_job_entered.wait(), timeout=1)
        await asyncio.wait_for(second_job_started.wait(), timeout=1)
    finally:
        release_first_job.set()
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
