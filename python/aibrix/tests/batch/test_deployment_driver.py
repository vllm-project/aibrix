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
import contextlib
import io
from datetime import datetime, timezone
from types import SimpleNamespace
from typing import Optional

import pytest

import aibrix.batch.job_driver.deployment_driver as deployment_driver_module
from aibrix.batch.job_driver import DeploymentJobDriver
from aibrix.batch.job_driver.driver_factory import create_job_driver
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    JobEntityManager,
    ObjectMeta,
    TypeMeta,
)
from aibrix.batch.job_manager import JobManager
from aibrix.batch.scheduler import JobScheduler
from aibrix.context import InfrastructureContext


class FakeEntityManager(JobEntityManager):
    def __init__(self):
        super().__init__()

    async def submit_job(self, session_id: str, job: BatchJobSpec):
        return None

    async def update_job_ready(self, job: BatchJob):
        return None

    async def update_job_status(self, job: BatchJob):
        return None

    async def cancel_job(self, job: BatchJob):
        return None

    async def delete_job(self, job: BatchJob):
        return None

    def get_job(self, job_id: str) -> Optional[BatchJob]:
        return None

    def list_jobs(self) -> list[BatchJob]:
        return []


class FakeProgressManager:
    def __init__(self, job: BatchJob):
        self.job = job
        self.failed_messages: list[str] = []
        self.validated_job_ids: list[str] = []
        self.created_driver = None

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        return self.job if self.job.job_id == job_id else None

    async def validate_job(self, job_id: str, inference_client=None) -> bool:
        if self.job.job_id != job_id:
            return False
        self.validated_job_ids.append(job_id)
        return True

    async def mark_job_failed(self, job_id: str, error):
        self.failed_messages.append(str(error))
        self.job.status.state = BatchJobState.FINALIZED
        return self.job


class FakeAppsV1Api:
    def __init__(self):
        self.created: list[tuple[str, dict]] = []
        self.deleted: list[tuple[str, str]] = []

    def create_namespaced_deployment(self, namespace: str, body: dict):
        self.created.append((namespace, body))

    def read_namespaced_deployment_status(self, name: str, namespace: str):
        return SimpleNamespace(status=SimpleNamespace(available_replicas=1))

    def delete_namespaced_deployment(self, name: str, namespace: str):
        self.deleted.append((namespace, name))


class FakeCoreV1Api:
    def __init__(self):
        self.created: list[tuple[str, dict]] = []
        self.deleted: list[tuple[str, str]] = []

    def create_namespaced_service(self, namespace: str, body: dict):
        self.created.append((namespace, body))

    def delete_namespaced_service(self, name: str, namespace: str):
        self.deleted.append((namespace, name))


class FakeRenderer:
    def render(
        self,
        template_name: str,
        profile_name=None,
        template_version=None,
        deployment_name=None,
        gpu_type=None,
    ):
        assert template_name == "mock-template"
        return {
            "deployment": {
                "apiVersion": "apps/v1",
                "kind": "Deployment",
                "metadata": {"name": deployment_name, "namespace": "default"},
                "spec": {
                    "replicas": 1,
                    "selector": {"matchLabels": {"app": deployment_name}},
                    "template": {
                        "metadata": {"labels": {"app": deployment_name}},
                        "spec": {"containers": [{"name": "llm-engine"}]},
                    },
                },
            },
            "service": {
                "apiVersion": "v1",
                "kind": "Service",
                "metadata": {"name": "template-service", "namespace": "default"},
                "spec": {
                    "selector": {"app": deployment_name},
                    "ports": [{"port": 8000, "targetPort": 8000}],
                    "type": "ClusterIP",
                },
            },
        }


@pytest.mark.asyncio
async def test_kubernetes_service_inference_client_logs_single_warning_on_gateway_success(
    monkeypatch,
):
    client = deployment_driver_module.KubernetesServiceInferenceClient(
        core_v1_api=FakeCoreV1Api(),
        namespace="default",
        service_name="svc",
        model_name="model-a",
        service_port=8000,
        base_url="http://svc.default.svc.cluster.local:8000",
    )
    warnings = []

    def _warning(message, **kwargs):
        warnings.append((message, kwargs))

    monkeypatch.setattr(deployment_driver_module.logger, "warning", _warning)

    def _proxy_fail(endpoint, request_data):
        raise RuntimeError("proxy boom")

    async def _gateway_success(endpoint, request_data):
        return {"ok": True, "model": request_data["model"]}

    client._proxy_inference_request = _proxy_fail
    client._gateway_inference_request = _gateway_success

    result = await client.inference_request("/v1/chat/completions", {"prompt": "hi"})

    assert result == {"ok": True, "model": "model-a"}
    assert len(warnings) == 1
    assert warnings[0][0] == "Inference request succeeded via gateway after fallback"
    assert warnings[0][1]["succeeded_via"] == "gateway"
    assert warnings[0][1]["attempts_failed"] == ["service proxy failed: proxy boom"]


@pytest.mark.asyncio
async def test_kubernetes_service_inference_client_logs_single_warning_on_port_forward_success(
    monkeypatch,
):
    client = deployment_driver_module.KubernetesServiceInferenceClient(
        core_v1_api=FakeCoreV1Api(),
        namespace="default",
        service_name="svc",
        model_name="model-a",
        service_port=8000,
        base_url="http://svc.default.svc.cluster.local:8000",
    )
    warnings = []

    def _warning(message, **kwargs):
        warnings.append((message, kwargs))

    monkeypatch.setattr(deployment_driver_module.logger, "warning", _warning)

    def _proxy_fail(endpoint, request_data):
        raise RuntimeError("proxy boom")

    async def _gateway_fail(endpoint, request_data):
        raise RuntimeError("gateway boom")

    async def _fallback_success(endpoint, request_data):
        return {"ok": True, "model": request_data["model"]}

    client._proxy_inference_request = _proxy_fail
    client._gateway_inference_request = _gateway_fail
    client._fallback_inference_request = _fallback_success

    result = await client.inference_request("/v1/chat/completions", {"prompt": "hi"})

    assert result == {"ok": True, "model": "model-a"}
    assert len(warnings) == 1
    assert (
        warnings[0][0] == "Inference request succeeded via port-forward after fallback"
    )
    assert warnings[0][1]["succeeded_via"] == "port-forward"
    assert warnings[0][1]["attempts_failed"] == [
        "service proxy failed: proxy boom",
        "gateway failed: gateway boom",
    ]


def test_kubernetes_service_inference_client_port_forward_parses_assigned_local_port(
    monkeypatch,
):
    client = deployment_driver_module.KubernetesServiceInferenceClient(
        core_v1_api=FakeCoreV1Api(),
        namespace="default",
        service_name="svc",
        model_name="model-a",
        service_port=8000,
        base_url="http://svc.default.svc.cluster.local:8000",
    )
    commands = []
    assigned_port = 39123

    class _FakeProcess:
        def __init__(self):
            self.stdout = io.StringIO(
                f"Forwarding from 127.0.0.1:{assigned_port} -> 8000\n"
            )

        def poll(self):
            return None

        def terminate(self):
            return None

    def _popen(command, **kwargs):
        commands.append(command)
        return _FakeProcess()

    monkeypatch.setattr(deployment_driver_module.subprocess, "Popen", _popen)

    base_url = client._start_port_forward()

    assert commands == [
        [
            "kubectl",
            "-n",
            "default",
            "port-forward",
            "service/svc",
            ":8000",
        ]
    ]
    assert base_url == f"http://127.0.0.1:{assigned_port}"


def _make_job(job_id: str = "job-123456789abc") -> BatchJob:
    spec = BatchJobSpec.from_strings(
        input_file_id="input-file-1",
        endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
        completion_window="24h",
        aibrix={
            "model_template": {"name": "mock-template"},
            "planner_decision": {
                "provision_id": "reservation-1",
                "provision_resource_deadline": 3600,
                "resource_details": [
                    {
                        "resource_type": "deployment",
                        "endpoint_cluster": "cluster-a",
                        "gpu_type": "H100",
                        "worker_num": 1,
                    }
                ],
            },
        },
    )
    status = BatchJobStatus.model_validate(
        {
            "jobID": job_id,
            "state": BatchJobState.IN_PROGRESS,
            "createdAt": datetime.now(timezone.utc),
            "inProgressAt": datetime.now(timezone.utc),
        }
    )
    return BatchJob(
        sessionID="session-1",
        typeMeta=TypeMeta(apiVersion="batch/v1", kind="Job"),
        metadata=ObjectMeta.model_validate({"name": "job", "namespace": "default"}),
        spec=spec,
        status=status,
    )


def _make_infrastructure_context(
    apps_v1_api=object(), core_v1_api=object()
) -> InfrastructureContext:
    return InfrastructureContext(
        template_registry=None,
        profile_registry=None,
        apps_v1_api=apps_v1_api,
        core_v1_api=core_v1_api,
    )


@pytest.fixture(autouse=True)
def reset_deployment_driver_singleton():
    deployment_driver_module._deployment_job_driver = None
    yield
    deployment_driver_module._deployment_job_driver = None


@pytest.mark.asyncio
async def test_deployment_driver_creates_runtime_and_finalizes_with_temp_files():
    job = _make_job()
    job.status.temp_output_file_id = "temp-out"
    job.status.temp_error_file_id = "temp-err"
    progress_manager = FakeProgressManager(job)
    entity_manager = FakeEntityManager()
    apps_api = FakeAppsV1Api()
    core_api = FakeCoreV1Api()
    driver = DeploymentJobDriver(
        _make_infrastructure_context(apps_v1_api=apps_api, core_v1_api=core_api),
        progress_manager=progress_manager,
        entity_manager=entity_manager,
        renderer=FakeRenderer(),
    )

    called = {"prepare": 0, "finalize": 0, "base_url": None}

    async def _prepare_job(_job):
        called["prepare"] += 1
        return _job

    async def _execute_worker(job_id):
        called["base_url"] = driver._inference_client.base_url
        progress_manager.job.status.state = BatchJobState.FINALIZING
        return progress_manager.job

    async def _finalize_job(_job):
        called["finalize"] += 1
        _job.status.state = BatchJobState.FINALIZED
        return _job

    driver.prepare_job = _prepare_job
    driver.execute_worker = _execute_worker
    driver.finalize_job = _finalize_job

    result = await driver.execute_job(job.job_id)

    assert result.status.state == BatchJobState.FINALIZED
    assert called["prepare"] == 0
    assert called["finalize"] == 1
    assert (
        called["base_url"]
        == "http://batch-job-12345678-svc.default.svc.cluster.local:8000"
    )
    assert len(apps_api.created) == 1
    assert len(core_api.created) == 1
    created_deployment = apps_api.created[0][1]
    created_service = core_api.created[0][1]
    assert (
        created_deployment["metadata"]["labels"]["batch.job.aibrix.ai/job-id"]
        == job.job_id
    )
    assert (
        created_deployment["spec"]["selector"]["matchLabels"][
            "batch.job.aibrix.ai/job-id"
        ]
        == job.job_id
    )
    assert (
        created_service["spec"]["selector"]["batch.job.aibrix.ai/job-id"] == job.job_id
    )
    assert (
        created_deployment["metadata"]["labels"]["model.aibrix.ai/name"]
        == "batch-job-12345678-svc"
    )
    assert (
        created_deployment["spec"]["selector"]["matchLabels"]["model.aibrix.ai/name"]
        == "batch-job-12345678-svc"
    )
    assert (
        created_service["metadata"]["labels"]["model.aibrix.ai/name"]
        == "batch-job-12345678-svc"
    )
    assert (
        created_service["spec"]["selector"]["model.aibrix.ai/name"]
        == "batch-job-12345678-svc"
    )
    assert core_api.deleted == [("default", "batch-job-12345678-svc")]
    assert apps_api.deleted == [("default", "batch-job-12345678-engine")]


@pytest.mark.asyncio
async def test_deployment_driver_job_deleted_interrupts_execution_and_tears_down():
    job = _make_job("job-delete-1234")
    progress_manager = FakeProgressManager(job)
    entity_manager = FakeEntityManager()
    apps_api = FakeAppsV1Api()
    core_api = FakeCoreV1Api()
    driver = DeploymentJobDriver(
        _make_infrastructure_context(apps_v1_api=apps_api, core_v1_api=core_api),
        progress_manager=progress_manager,
        entity_manager=entity_manager,
        renderer=FakeRenderer(),
    )

    entered = asyncio.Event()

    async def _prepare_job(_job):
        _job.status.temp_output_file_id = "temp-out"
        _job.status.temp_error_file_id = "temp-err"
        return _job

    async def _execute_worker(_job_id):
        entered.set()
        await asyncio.sleep(3600)
        return progress_manager.job

    async def _finalize_job(_job):
        raise AssertionError("finalize_job should not run after deletion")

    driver.prepare_job = _prepare_job
    driver.execute_worker = _execute_worker
    driver.finalize_job = _finalize_job

    task = asyncio.create_task(driver.execute_job(job.job_id))
    await asyncio.wait_for(entered.wait(), timeout=1)
    deleted = await driver._job_deleted_handler(job)
    assert deleted is True
    await task

    assert core_api.deleted == [("default", "batch-job-delete-1-svc")]
    assert apps_api.deleted == [("default", "batch-job-delete-1-engine")]


def test_create_job_driver_uses_deployment_driver_for_scheduler_jobs(monkeypatch):
    """Protect driver selection for resource_type=deployment.

    If the factory regresses and falls back to the local/simple path,
    metadata-server jobs that request deployment execution will silently
    stop using DeploymentJobDriver.
    """
    job = _make_job()
    entity_manager = FakeEntityManager()
    deployment_sentinel = object()
    simple_sentinel = object()

    monkeypatch.setattr(
        "aibrix.batch.job_driver.driver_factory.DeploymentJobDriver",
        lambda context, progress_manager, entity_manager, **kwargs: deployment_sentinel,
    )
    monkeypatch.setattr(
        "aibrix.batch.job_driver.driver_factory.SimpleJobDriver",
        lambda progress_manager, entity_manager: simple_sentinel,
    )

    driver = create_job_driver(
        _make_infrastructure_context(),
        progress_manager=FakeProgressManager(job),
        entity_manager=entity_manager,
        job=job,
    )

    assert driver is deployment_sentinel


def test_create_job_driver_passes_infrastructure_context_to_deployment_driver(
    monkeypatch,
):
    """Protect infrastructure propagation into DeploymentJobDriver.

    DeploymentJobDriver now depends on the shared infrastructure context for
    registries and Kubernetes APIs. This catches regressions where the
    factory still chooses DeploymentJobDriver but forgets to forward that
    context.
    """
    job = _make_job()
    entity_manager = FakeEntityManager()
    captured = {}

    def _deployment_driver(context, progress_manager, entity_manager, **kwargs):
        captured["context"] = context
        captured["progress_manager"] = progress_manager
        captured["entity_manager"] = entity_manager
        captured["kwargs"] = kwargs
        return object()

    monkeypatch.setattr(
        "aibrix.batch.job_driver.driver_factory.DeploymentJobDriver",
        _deployment_driver,
    )

    create_job_driver(
        _make_infrastructure_context(
            apps_v1_api="apps-api",
            core_v1_api="core-api",
        ),
        progress_manager=FakeProgressManager(job),
        entity_manager=entity_manager,
        job=job,
    )

    assert captured["context"].apps_v1_api == "apps-api"
    assert captured["context"].core_v1_api == "core-api"
    assert captured["entity_manager"] is entity_manager


@pytest.mark.asyncio
async def test_scheduler_uses_create_job_driver_for_deployment_jobs(monkeypatch):
    job = _make_job()
    entity_manager = FakeEntityManager()
    context = _make_infrastructure_context()
    progress_manager = JobManager(context)
    progress_manager._job_entity_manager = entity_manager
    assert job.job_id is not None
    progress_manager._pending_jobs[job.job_id] = job
    created = {}

    class _Driver:
        async def validate_job(self, job_arg):
            return None

        async def execute_job(self, job_id):
            created["job_id"] = job_id

    def _create_job_driver(
        context_arg,
        progress_manager_arg,
        entity_manager_arg,
        job_arg,
        inference_client_arg=None,
        **kwargs,
    ):
        created["context"] = context_arg
        created["progress_manager"] = progress_manager_arg
        created["entity_manager"] = entity_manager_arg
        created["job"] = job_arg
        created["inference_client"] = inference_client_arg
        created["kwargs"] = kwargs
        return _Driver()

    async def _one_job():
        return job.job_id

    monkeypatch.setattr(
        "aibrix.batch.job_manager.create_job_driver",
        _create_job_driver,
    )
    await progress_manager.validate_job(job.job_id)
    scheduler = JobScheduler(context, progress_manager, entity_manager, 1)
    monkeypatch.setattr(scheduler, "round_robin_get_job", _one_job)

    task = asyncio.create_task(scheduler.jobs_running_loop())
    try:
        for _ in range(20):
            if created.get("job_id") == job.job_id:
                break
            await asyncio.sleep(0)
        assert created["context"] is context
        assert created["progress_manager"] is progress_manager
        assert created["entity_manager"] is entity_manager
        assert created["job"].job_id == job.job_id
        assert created["inference_client"] is None
        assert created["job_id"] == job.job_id
    finally:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task
