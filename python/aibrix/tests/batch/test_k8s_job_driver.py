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
(source=None + provisions) then moves to FINALIZING; on_prepared un-suspends."""

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


def _job():
    return SimpleNamespace(
        status=SimpleNamespace(state=BatchJobState.IN_PROGRESS), job_id="jid"
    )


@pytest.mark.asyncio
async def test_execute_waits_then_moves_to_finalizing_on_success():
    job = _job()
    rt = _FakeRuntime(Completion(succeeded=True))
    driver = BaseJobDriver(_FakePM(job), rt)
    result = await driver.run_job("jid", Endpoint(source=None))
    assert result.status.state == BatchJobState.FINALIZING


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
        spec=SimpleNamespace(compute_provider="KubernetesJob"),
    )
    ctx = SimpleNamespace(
        batch_v1_api=object(), template_registry=None, profile_registry=None
    )
    em = SimpleNamespace(on_job_deleted=lambda handler: None)
    driver = create_job_driver(ctx, _FakePM(None), entity_manager=em, job=job)
    assert isinstance(driver, BaseJobDriver)
    assert isinstance(driver._runtime, K8sJobRuntime)
