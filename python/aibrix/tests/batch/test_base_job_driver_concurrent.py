# Copyright 2026 The Aibrix Team.
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
from datetime import datetime
from typing import Optional

import pytest

from aibrix.batch.client import CapacitySignal, DispatchEngine
from aibrix.batch.job_driver import BaseJobDriver, ExternalRuntime
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    ObjectMeta,
    RequestCountStats,
    TypeMeta,
)
from aibrix.batch.worker import SingleJobRunner


def _make_job(total: int) -> BatchJob:
    return BatchJob(
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
        status=BatchJobStatus(
            jobID="job-1",
            state=BatchJobState.IN_PROGRESS,
            createdAt=datetime.now(),
            requestCounts=RequestCountStats(total=total),
        ),
    )


class _SlowChannel:
    def __init__(self):
        self.inflight = 0
        self.peak = 0

    @property
    def id(self):
        return "slow"

    async def send(self, request):
        self.inflight += 1
        self.peak = max(self.peak, self.inflight)
        await asyncio.sleep(0.01)
        self.inflight -= 1
        return {
            "echo": request.payload,
            "usage": {"prompt_tokens": 1, "completion_tokens": 2},
        }

    async def aclose(self):
        return None


class _Source:
    def __init__(self, channel, capacity: int):
        self._channel = channel
        self._capacity = capacity

    async def channels(self):
        return [self._channel]

    async def capacity(self):
        return CapacitySignal(count=self._capacity)

    async def wait_capacity_change(self, previous):
        await asyncio.Future()
        raise AssertionError("unreachable")

    async def aclose(self):
        await self._channel.aclose()


def _driver(job: BatchJob, *, capacity: int = 2):
    channel = _SlowChannel()
    driver = BaseJobDriver(SingleJobRunner(job), ExternalRuntime(None))
    driver._engine = DispatchEngine(_Source(channel, capacity), max_retries=0)
    return driver, channel


def _patch_storage(monkeypatch, requests, done: Optional[set[int]] = None):
    done = done or set()
    outputs = {}

    async def read_job_next_request(_job, _start_index=0):
        for request in requests:
            yield dict(request)

    async def write_job_output_data(_job, request_index, output_data):
        outputs[request_index] = output_data
        done.add(request_index)

    async def is_request_done(_job, request_index):
        return request_index in done

    from aibrix.batch.job_driver import base as base_module

    monkeypatch.setattr(
        base_module.storage, "read_job_next_request", read_job_next_request
    )
    monkeypatch.setattr(
        base_module.storage, "write_job_output_data", write_job_output_data
    )
    monkeypatch.setattr(base_module.storage, "is_request_done", is_request_done)
    return outputs, done


@pytest.mark.asyncio
async def test_execute_worker_uses_concurrent_dispatch_for_known_total(monkeypatch):
    job = _make_job(total=4)
    driver, channel = _driver(job, capacity=2)
    requests = [
        {"_request_index": i, "custom_id": f"req-{i}", "body": {"i": i}}
        for i in range(4)
    ]
    outputs, _ = _patch_storage(monkeypatch, requests)

    result = await driver.execute_worker(job.job_id)

    assert result.status.state == BatchJobState.FINALIZING
    assert result.status.request_counts.completed == 4
    assert set(outputs) == {0, 1, 2, 3}
    assert channel.peak == 2
    assert result.status.usage.input_tokens == 4
    assert result.status.usage.output_tokens == 8
    assert result.status.usage.total_tokens == 12


@pytest.mark.asyncio
async def test_execute_worker_reconciles_storage_done_requests(monkeypatch):
    job = _make_job(total=2)
    driver, _ = _driver(job, capacity=2)
    requests = [{"_request_index": 1, "custom_id": "req-1", "body": {"i": 1}}]
    _patch_storage(monkeypatch, requests, done={0})

    result = await driver.execute_worker(job.job_id)

    assert result.status.state == BatchJobState.FINALIZING
    assert result.status.request_counts.completed == 2
