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
"""K8sJobRuntime: create a suspended Job, poll it to completion (no kopf),
delete it. Exercised against a fake BatchV1Api — no real cluster."""

import asyncio
from types import SimpleNamespace

import pytest

from aibrix.batch.job_driver.runtime.k8s_job import K8sJobHandle, K8sJobRuntime


class _FakeBatchV1Api:
    def __init__(self):
        self.created = []
        self.deleted = []
        self.conditions = []  # what read_namespaced_job_status reports

    def create_namespaced_job(self, namespace, body):
        self.created.append((namespace, body))
        return body

    def read_namespaced_job_status(self, name, namespace):
        return SimpleNamespace(status=SimpleNamespace(conditions=self.conditions))

    def delete_namespaced_job(self, name, namespace, propagation_policy):
        self.deleted.append(name)


class _FakeRenderer:
    def render(
        self, session_id, spec, prepared_job=None, parallelism=None, job_name=None
    ):
        return {
            "metadata": {"name": "batch-job-1", "namespace": "ns-x"},
            "spec": {"suspend": True},
        }


def _cond(type_, status="True", reason="", message=""):
    return SimpleNamespace(type=type_, status=status, reason=reason, message=message)


def _runtime(api):
    ctx = SimpleNamespace(
        batch_v1_api=api, template_registry=None, profile_registry=None
    )
    return K8sJobRuntime(
        ctx,
        renderer=_FakeRenderer(),
        done_timeout_seconds=2,
        poll_interval_seconds=0,
    )


def _job():
    return SimpleNamespace(
        session_id="sess",
        spec=SimpleNamespace(),
        job_id="jid",
        status=SimpleNamespace(get_runtime_ref=lambda key: None),
    )


@pytest.mark.asyncio
async def test_provision_creates_suspended_job():
    api = _FakeBatchV1Api()
    handle = await _runtime(api)._provision(_job(), "jid")
    assert handle == K8sJobHandle(namespace="ns-x", job_name="batch-job-1")
    assert len(api.created) == 1
    namespace, body = api.created[0]
    assert namespace == "ns-x"
    assert body["spec"]["suspend"] is True


@pytest.mark.asyncio
async def test_build_runtime_ref_uses_job_handle_metadata():
    api = _FakeBatchV1Api()
    runtime = _runtime(api)
    job = _job()

    await runtime._provision(job, "jid")
    runtime_ref = runtime._build_runtime_ref(job)

    assert runtime_ref is not None
    assert runtime_ref.driver_type == "k8s-job"
    assert runtime_ref.owner_ref == "ns-x/batch-job-1"
    assert runtime_ref.reconnect_payload == {
        "namespace": "ns-x",
        "jobName": "batch-job-1",
    }


@pytest.mark.asyncio
async def test_wait_until_done_complete():
    api = _FakeBatchV1Api()
    api.conditions = [_cond("Complete")]
    out = await _runtime(api)._wait_until_done(K8sJobHandle("ns-x", "batch-job-1"))
    assert out.succeeded is True


@pytest.mark.asyncio
async def test_wait_until_done_failed():
    api = _FakeBatchV1Api()
    api.conditions = [_cond("Failed", reason="DeadlineExceeded", message="too slow")]
    out = await _runtime(api)._wait_until_done(K8sJobHandle("ns-x", "batch-job-1"))
    assert out.succeeded is False
    assert out.reason == "DeadlineExceeded"


@pytest.mark.asyncio
async def test_teardown_deletes_job():
    api = _FakeBatchV1Api()
    await _runtime(api)._teardown(K8sJobHandle("ns-x", "batch-job-1"))
    assert api.deleted == ["batch-job-1"]


@pytest.mark.asyncio
async def test_deletion_cancels_wait():
    api = _FakeBatchV1Api()  # no terminal condition → would poll forever
    rt = _runtime(api)
    job = _job()
    job.spec.opts = {}
    wait_entered = asyncio.Event()
    original_wait_until_done = rt._wait_until_done

    async def _persist_runtime_ref(job, **kwargs):
        del kwargs
        return job

    async def _wait_until_done(handle):
        wait_entered.set()
        return await original_wait_until_done(handle)

    rt._persist_runtime_ref = _persist_runtime_ref
    rt._wait_until_done = _wait_until_done

    async def _run_session():
        async with rt.session(job, "jid"):
            await rt.await_completion()

    # Start session
    task = asyncio.create_task(_run_session())
    # Wait until the session is inside await_completion(); otherwise terminate()
    # could race ahead of the wait loop and make this cancellation assertion flaky.
    await asyncio.wait_for(wait_entered.wait(), timeout=1)

    # Trigger termination
    await rt.terminate(SimpleNamespace(job_id="jid"))

    with pytest.raises(asyncio.CancelledError):
        await task

    assert rt.cancelled() is True
    assert api.deleted == ["batch-job-1"]
