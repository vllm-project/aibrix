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
"""KubernetesJob backend: a self-hosting Runtime under the converged
BaseJobDriver. The base execute phase awaits the provisioned Job to completion
(source=None + provisions) then finalizes to aggregate the worker's output;
on_prepared un-suspends."""

from types import SimpleNamespace

import pytest

from aibrix.batch.job_driver.base import BaseJobDriver
from aibrix.batch.job_driver.driver_factory import create_job_driver
from aibrix.batch.job_driver.runtime import Completion, Endpoint
from aibrix.batch.job_driver.runtime.k8s_job import K8sJobHandle, K8sJobRuntime
from aibrix.batch.job_entity import BatchJobError, BatchJobErrorCode, BatchJobState


class _FakeRuntime:
    """A self-hosting provisioning runtime: no endpoint, await_completion."""

    provisions = True

    def __init__(self, completion=None):
        self._completion = completion
        self.prepared = False

    def cancelled(self):
        return False

    async def on_prepared(self):
        self.prepared = True

    async def await_completion(self):
        return self._completion


class _FakePM:
    def __init__(self, job):
        self._job = job

    async def get_job(self, job_id):
        return self._job

    async def mark_job_finalizing(self, job_id):
        self._job.status.state = BatchJobState.FINALIZING
        return self._job

    async def mark_job_done(self, job):
        job.status.state = BatchJobState.FINALIZED
        return job


def _job():
    return SimpleNamespace(
        status=SimpleNamespace(state=BatchJobState.IN_PROGRESS), job_id="jid"
    )


@pytest.mark.asyncio
async def test_self_hosted_run_finalizes_on_success(monkeypatch):
    """A successful self-hosted run aggregates the worker's output through
    finalize_job, so the persisted job records the real request_counts / usage
    instead of only flipping to FINALIZING with the stale in-memory copy (#2263).
    The worker ran in worker_mode and left the per-request done markers intact
    for this aggregation."""
    job = _job()
    rt = _FakeRuntime(Completion(succeeded=True))
    driver = BaseJobDriver(_FakePM(job), rt)

    aggregated = []

    async def _record_finalize(j):
        aggregated.append(j)

    monkeypatch.setattr(
        "aibrix.batch.job_driver.base.storage.finalize_job_output_data",
        _record_finalize,
        raising=False,
    )

    result = await driver.run_job("jid", Endpoint(source=None))

    assert aggregated == [job]
    assert result.status.state == BatchJobState.FINALIZED


@pytest.mark.asyncio
async def test_execute_raises_on_failed_job():
    job = _job()
    rt = _FakeRuntime(
        Completion(succeeded=False, reason="DeadlineExceeded", message="too slow")
    )
    driver = BaseJobDriver(_FakePM(job), rt)
    with pytest.raises(BatchJobError) as excinfo:
        await driver.run_job("jid", Endpoint(source=None))
    assert excinfo.value.code == BatchJobErrorCode.INFERENCE_FAILED


@pytest.mark.asyncio
async def test_on_prepared_unsuspends_the_job():
    patched = {}

    class _BatchApi:
        def patch_namespaced_job(self, name, namespace, body):
            patched["name"] = name
            patched["namespace"] = namespace
            patched["body"] = body

    ctx = SimpleNamespace(
        batch_v1_api=_BatchApi(), template_registry=None, profile_registry=None
    )
    em = SimpleNamespace(on_job_deleted=lambda handler: None)
    runtime = K8sJobRuntime(ctx, em)
    runtime._active_handle = K8sJobHandle(namespace="ns", job_name="jn")

    await runtime.on_prepared()

    assert patched["name"] == "jn"
    assert patched["namespace"] == "ns"
    assert patched["body"] == {"spec": {"suspend": False}}


def test_factory_selects_k8s_job_runtime_for_kubernetes_job_provider():
    job = SimpleNamespace(
        spec=SimpleNamespace(runtime_target="KubernetesJob"),
    )
    ctx = SimpleNamespace(
        batch_v1_api=object(), template_registry=None, profile_registry=None
    )
    em = SimpleNamespace(on_job_deleted=lambda handler: None)
    driver = create_job_driver(ctx, _FakePM(None), entity_manager=em, job=job)
    assert isinstance(driver, BaseJobDriver)
    assert isinstance(driver._runtime, K8sJobRuntime)
