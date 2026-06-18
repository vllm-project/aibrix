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

from aibrix.batch.client import (
    CapacitySignal,
    DispatchEngine,
    InferenceError,
    InferenceErrorCode,
)
from aibrix.batch.job_driver import BaseJobDriver, ExternalRuntime
from aibrix.batch.job_entity import (
    AibrixMetadata,
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
    def __init__(self, fail_indices: Optional[set[int]] = None):
        self._fail_indices = fail_indices or set()
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
        if request.ref[0] in self._fail_indices:
            raise InferenceError(
                InferenceErrorCode.TRANSPORT_ERROR,
                "injected failure",
                retryable=False,
            )
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


class _CapturingEngine:
    def __init__(self):
        self.run_kwargs = None

    async def run(self, requests, on_result, **kwargs):
        self.run_kwargs = kwargs
        async for request in requests:
            await on_result(request, {"usage": {"prompt_tokens": 1}}, None)


def _driver(
    job: BatchJob,
    *,
    capacity: int = 2,
    fail_indices: Optional[set[int]] = None,
):
    channel = _SlowChannel(fail_indices=fail_indices)
    driver = BaseJobDriver(SingleJobRunner(job), ExternalRuntime(None))
    driver._engine = DispatchEngine(_Source(channel, capacity), max_retries=0)
    return driver, channel


def test_retry_config_prefers_job_client_and_falls_back_to_env(monkeypatch):
    from aibrix.batch.job_driver import base as base_module

    monkeypatch.setenv("AIBRIX_BATCH_INFERENCE_MAX_RETRIES", "9")
    monkeypatch.setenv("AIBRIX_BATCH_NO_ENDPOINT_MAX_RETRIES", "11")
    monkeypatch.setenv("AIBRIX_BATCH_RETRY_BASE_DELAY_SECONDS", "2")
    monkeypatch.setenv("AIBRIX_BATCH_RETRY_MAX_DELAY_SECONDS", "10")
    job = _make_job(total=1)
    job.spec.aibrix = AibrixMetadata(
        client={
            "retry_policy": {
                "max_retries": 5,
                "max_delay_seconds": 6,
            }
        }
    )
    driver = BaseJobDriver(SingleJobRunner(job), ExternalRuntime(None))

    retry = driver._retry_config_for_job(job)

    assert retry.max_retries == 5
    assert retry.no_endpoint_retries() == 11
    assert retry.base_delay_seconds == 2
    assert retry.max_delay_seconds == 6
    assert base_module._inference_max_retries() == 9


def test_retry_backoff_delays_are_configurable_from_env(monkeypatch):
    from aibrix.batch.job_driver import base as base_module

    monkeypatch.setenv("AIBRIX_BATCH_RETRY_BASE_DELAY_SECONDS", "2")
    monkeypatch.setenv("AIBRIX_BATCH_RETRY_MAX_DELAY_SECONDS", "10")

    assert base_module._retry_base_delay_seconds() == 2.0
    assert base_module._retry_max_delay_seconds() == 10.0


def test_dispatch_kwargs_preserve_adaptive_capacity_factor_when_cap_absent(
    monkeypatch,
):
    monkeypatch.setenv("AIBRIX_BATCH_ADAPTIVE_MAX_FACTOR", "12")
    job = _make_job(total=1)
    driver = BaseJobDriver(SingleJobRunner(job), ExternalRuntime(None))

    kwargs = driver._dispatch_run_kwargs_for_job(job)

    assert kwargs == {
        "adaptive_concurrency": True,
        "adaptive_max_factor": 12.0,
    }


def test_dispatch_kwargs_use_fixed_max_concurrency_when_adaptive_disabled():
    job = _make_job(total=1)
    job.spec.aibrix = AibrixMetadata(
        client={
            "max_concurrency": 64,
            "adaptive_concurrency": False,
            "adaptive_max_factor": 16,
        }
    )
    driver = BaseJobDriver(SingleJobRunner(job), ExternalRuntime(None))

    kwargs = driver._dispatch_run_kwargs_for_job(job)

    assert kwargs == {
        "adaptive_concurrency": False,
        "adaptive_max_factor": 16,
        "max_concurrency": 64,
    }


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
async def test_execute_worker_passes_client_concurrency_as_absolute_adaptive_cap(
    monkeypatch,
):
    job = _make_job(total=1)
    job.spec.aibrix = AibrixMetadata(
        client={
            "max_concurrency": 64,
            "adaptive_concurrency": True,
            "adaptive_max_factor": 16,
        }
    )
    driver = BaseJobDriver(SingleJobRunner(job), ExternalRuntime(None))
    engine = _CapturingEngine()
    driver._engine = engine
    requests = [{"_request_index": 0, "custom_id": "req-0", "body": {"i": 0}}]
    _patch_storage(monkeypatch, requests)

    result = await driver.execute_worker(job.job_id)

    assert result.status.state == BatchJobState.FINALIZING
    assert engine.run_kwargs is not None
    assert engine.run_kwargs["adaptive_concurrency"] is True
    assert engine.run_kwargs["adaptive_max_concurrency"] == 64
    assert engine.run_kwargs["adaptive_max_factor"] == 16
    assert "max_concurrency" not in engine.run_kwargs


@pytest.mark.asyncio
async def test_execute_worker_reconciles_storage_done_requests(monkeypatch):
    job = _make_job(total=2)
    driver, _ = _driver(job, capacity=2)
    requests = [{"_request_index": 1, "custom_id": "req-1", "body": {"i": 1}}]
    _patch_storage(monkeypatch, requests, done={0})

    result = await driver.execute_worker(job.job_id)

    assert result.status.state == BatchJobState.FINALIZING
    assert result.status.request_counts.completed == 2


@pytest.mark.asyncio
async def test_execute_worker_stats_fallback_preserves_failed_count(monkeypatch):
    job = _make_job(total=3)
    driver, _ = _driver(job, capacity=2, fail_indices={1})
    requests = [
        {"_request_index": i, "custom_id": f"req-{i}", "body": {"i": i}}
        for i in range(3)
    ]
    _patch_storage(monkeypatch, requests)

    async def complete_without_progress(job_id, req_id, failed=False):
        return job

    driver._progress_manager.complete_job_request = complete_without_progress

    result = await driver.execute_worker(job.job_id)

    assert result.status.state == BatchJobState.FINALIZING
    assert result.status.request_counts.completed == 2
    assert result.status.request_counts.failed == 1


@pytest.mark.asyncio
async def test_reconcile_storage_done_checks_are_parallel(monkeypatch):
    job = _make_job(total=4)
    driver, _ = _driver(job, capacity=2)
    inflight = 0
    peak = 0

    async def is_request_done(_job, request_index):
        nonlocal inflight, peak
        inflight += 1
        peak = max(peak, inflight)
        await asyncio.sleep(0)
        inflight -= 1
        return request_index in {0, 1, 2, 3}

    from aibrix.batch.job_driver import base as base_module

    monkeypatch.setattr(base_module.storage, "is_request_done", is_request_done)

    result = await driver._sync_completed_requests_from_storage(job.job_id, job)

    assert peak > 1
    assert result.status.state == BatchJobState.FINALIZING
    assert result.status.request_counts.completed == 4
