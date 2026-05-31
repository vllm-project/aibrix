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
from datetime import datetime, timezone
from types import SimpleNamespace
from typing import Optional

import pytest

from aibrix.batch.batch_manager import BatchManager
from aibrix.batch.batch_scheduler import BatchScheduler
from aibrix.batch.job_driver import BaseJobDriver, DeploymentRuntime, ExternalRuntime
from aibrix.batch.job_driver.driver_factory import create_job_driver
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    ObjectMeta,
    TypeMeta,
)
from aibrix.batch.manifest.renderer import RenderError
from aibrix.batch.state import JobEntityManager
from aibrix.context import InfrastructureContext


class FakeEntityManager(JobEntityManager):
    def __init__(self):
        super().__init__()

    async def submit_job(
        self, session_id: str, job: BatchJobSpec, request_count: int = 0
    ):
        return None

    async def update_job_ready(self, job: BatchJob):
        return None

    async def update_job_status(self, job: BatchJob):
        return None

    async def cancel_job(self, job: BatchJob):
        return None

    async def delete_job(self, job: BatchJob):
        return None

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        return None

    async def list_jobs(self) -> list[BatchJob]:
        return []


class FakeProgressManager:
    def __init__(self, job: BatchJob):
        self.job = job
        self.failed_messages: list[str] = []
        self.validated_job_ids: list[str] = []
        self.created_driver = None

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        return self.job if self.job.job_id == job_id else None

    async def validate_job(self, job_id: str, endpoint_source=None) -> bool:
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
        job_id: str,
        spec: BatchJobSpec,
        provider_spec,
    ):
        assert job_id is not None
        assert spec.model_template_name == "mock-template"
        return {
            "deployment": {
                "apiVersion": "apps/v1",
                "kind": "Deployment",
                "metadata": {
                    "name": "rendered-deployment",
                    "namespace": "default",
                    "labels": {"model.aibrix.ai/name": "rendered-model"},
                },
                "spec": {
                    "replicas": 1,
                    "selector": {
                        "matchLabels": {
                            "app": "rendered-app",
                            "model.aibrix.ai/name": "rendered-model",
                        }
                    },
                    "template": {
                        "metadata": {
                            "labels": {
                                "app": "rendered-app",
                                "model.aibrix.ai/name": "rendered-model",
                            }
                        },
                        "spec": {"containers": [{"name": "llm-engine"}]},
                    },
                },
            },
            "service": {
                "apiVersion": "v1",
                "kind": "Service",
                "metadata": {"name": "rendered-service", "namespace": "default"},
                "spec": {
                    "selector": {
                        "app": "rendered-app",
                        "model.aibrix.ai/name": "rendered-model",
                    },
                    "ports": [{"port": 8000, "targetPort": 8000}],
                    "type": "ClusterIP",
                },
            },
        }


def _make_job(job_id: str = "job-123456789abc") -> BatchJob:
    spec = BatchJobSpec.from_strings(
        input_file_id="input-file-1",
        endpoint=BatchJobEndpoint.CHAT_COMPLETIONS.value,
        completion_window="24h",
        aibrix={
            "model_template": {"name": "mock-template"},
            "compute": {"provider": "Kubernetes"},
            "planner_decision": {
                "provision_id": "reservation-1",
                "provision_resource_deadline": 3600,
                "resource_details": [
                    {
                        "endpoint_cluster": "cluster-a",
                        "gpu_type": "H100",
                        "replica": 1,
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


def _make_deployment_driver(
    context, progress_manager, entity_manager, renderer=None
) -> BaseJobDriver:
    """The converged driver: BaseJobDriver wired to a DeploymentRuntime. The
    deployment-owns-the-run behaviors (no re-raise, always aggregate, internal
    default failure) are derived from DeploymentRuntime.provisions=True."""
    runtime = DeploymentRuntime(context, entity_manager, renderer)
    return BaseJobDriver(progress_manager, runtime)


@pytest.mark.asyncio
async def test_deployment_driver_allows_missing_template_registry_at_construction():
    driver = _make_deployment_driver(
        _make_infrastructure_context(),
        progress_manager=FakeProgressManager(_make_job()),
        entity_manager=FakeEntityManager(),
    )

    assert driver._runtime._renderer is not None


@pytest.mark.asyncio
async def test_deployment_driver_reports_missing_template_registry_at_render_time():
    driver = _make_deployment_driver(
        _make_infrastructure_context(),
        progress_manager=FakeProgressManager(_make_job()),
        entity_manager=FakeEntityManager(),
    )

    with pytest.raises(RenderError, match="template registry is not configured"):
        await driver._runtime._provision(_make_job(), "job-render-test")


@pytest.mark.asyncio
async def test_deployment_driver_creates_runtime_and_finalizes_with_temp_files():
    job = _make_job()
    job.status.temp_output_file_id = "temp-out"
    job.status.temp_error_file_id = "temp-err"
    progress_manager = FakeProgressManager(job)
    entity_manager = FakeEntityManager()
    apps_api = FakeAppsV1Api()
    core_api = FakeCoreV1Api()
    driver = _make_deployment_driver(
        _make_infrastructure_context(apps_v1_api=apps_api, core_v1_api=core_api),
        progress_manager=progress_manager,
        entity_manager=entity_manager,
        renderer=FakeRenderer(),
    )

    called = {"prepare": 0, "finalize": 0, "base_url": None, "model_name": None}

    async def _prepare_job(_job):
        called["prepare"] += 1
        return _job

    async def _execute_worker(job_id):
        called["base_url"] = driver._runtime._active_handle.base_url
        called["model_name"] = driver._active_model_name
        progress_manager.job.status.state = BatchJobState.FINALIZING
        return progress_manager.job

    async def _finalize_job(_job):
        called["finalize"] += 1
        _job.status.state = BatchJobState.FINALIZED
        return _job

    driver.prepare_job = _prepare_job
    driver.execute_worker = _execute_worker
    driver.finalize_job = _finalize_job

    await driver.execute(job.job_id)

    assert job.status.state == BatchJobState.FINALIZED
    assert called["prepare"] == 0
    assert called["finalize"] == 1
    assert (
        called["base_url"] == "http://rendered-service.default.svc.cluster.local:8000"
    )
    assert called["model_name"] == "rendered-model"
    assert len(apps_api.created) == 1
    assert len(core_api.created) == 1
    created_deployment = apps_api.created[0][1]
    created_service = core_api.created[0][1]
    assert (
        created_deployment["metadata"]["labels"]["model.aibrix.ai/name"]
        == "rendered-model"
    )
    assert (
        created_deployment["spec"]["selector"]["matchLabels"]["model.aibrix.ai/name"]
        == "rendered-model"
    )
    assert (
        created_service["spec"]["selector"]["model.aibrix.ai/name"] == "rendered-model"
    )
    assert core_api.deleted == [("default", "rendered-service")]
    assert apps_api.deleted == [("default", "rendered-deployment")]


@pytest.mark.asyncio
async def test_deployment_driver_job_deleted_interrupts_execution_and_tears_down():
    job = _make_job("job-delete-1234")
    progress_manager = FakeProgressManager(job)
    entity_manager = FakeEntityManager()
    apps_api = FakeAppsV1Api()
    core_api = FakeCoreV1Api()
    driver = _make_deployment_driver(
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

    task = asyncio.create_task(driver.execute(job.job_id))
    await asyncio.wait_for(entered.wait(), timeout=1)
    deleted = await driver._runtime._job_deleted_handler(job)
    assert deleted is True
    await task

    assert core_api.deleted == [("default", "rendered-service")]
    assert apps_api.deleted == [("default", "rendered-deployment")]


@pytest.mark.asyncio
async def test_create_job_driver_uses_deployment_runtime_for_kubernetes_provider():
    """Protect Runtime selection for compute.provider=Kubernetes.

    If the factory regresses and falls back to the local/standalone path,
    metadata-server jobs that request deployment execution silently stop
    being wired to a DeploymentRuntime.
    """
    job = _make_job()
    entity_manager = FakeEntityManager()

    driver = create_job_driver(
        _make_infrastructure_context(),
        progress_manager=FakeProgressManager(job),
        entity_manager=entity_manager,
        job=job,
    )

    assert isinstance(driver, BaseJobDriver)
    assert isinstance(driver._runtime, DeploymentRuntime)


@pytest.mark.asyncio
async def test_create_job_driver_passes_infrastructure_context_to_deployment_runtime():
    """Protect infrastructure propagation into the DeploymentRuntime.

    The runtime depends on the shared infrastructure context for registries
    and Kubernetes APIs. This catches regressions where the factory selects
    the right runtime but forgets to forward that context.
    """
    job = _make_job()
    entity_manager = FakeEntityManager()

    driver = create_job_driver(
        _make_infrastructure_context(
            apps_v1_api="apps-api",
            core_v1_api="core-api",
        ),
        progress_manager=FakeProgressManager(job),
        entity_manager=entity_manager,
        job=job,
    )

    runtime = driver._runtime
    assert isinstance(runtime, DeploymentRuntime)
    assert runtime._apps_v1_api == "apps-api"
    assert runtime._core_v1_api == "core-api"
    assert runtime._entity_manager is entity_manager


@pytest.mark.asyncio
async def test_scheduler_uses_create_job_driver_for_deployment_jobs(monkeypatch):
    job = _make_job()
    entity_manager = FakeEntityManager()
    context = _make_infrastructure_context()
    progress_manager = BatchManager(context)
    progress_manager._job_entity_manager = entity_manager
    assert job.job_id is not None
    progress_manager._pending_jobs[job.job_id] = job
    created = {}

    class _Driver:
        async def validate_job(self, job_arg):
            return None

        async def execute(self, job_id):
            created["job_id"] = job_id

    def _create_job_driver(
        context_arg,
        progress_manager_arg,
        entity_manager_arg,
        job_arg,
        endpoint_source_arg=None,
        **kwargs,
    ):
        created["context"] = context_arg
        created["progress_manager"] = progress_manager_arg
        created["entity_manager"] = entity_manager_arg
        created["job"] = job_arg
        created["endpoint_source"] = endpoint_source_arg
        created["kwargs"] = kwargs
        return _Driver()

    async def _one_job():
        return (job.job_id, driver)

    monkeypatch.setattr(
        "aibrix.batch.batch_manager.create_job_driver",
        _create_job_driver,
    )
    driver = await progress_manager.admit(job.job_id)
    scheduler = BatchScheduler(context, progress_manager, 1)
    monkeypatch.setattr(scheduler, "schedule_next_job", _one_job)

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
        assert created["endpoint_source"] is None
        assert created["job_id"] == job.job_id
    finally:
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task


# ── converged-driver behavior (replaces the deleted Deployment/K8sJob subclasses) ──


@pytest.mark.asyncio
async def test_provisioning_runtime_drives_converged_failure_policy():
    """A provisioning runtime makes the base driver own the whole run: no
    re-raise, always aggregate, internal default failure. This is the
    load-bearing replacement for the old DeploymentJobDriver overrides."""
    runtime = DeploymentRuntime(_make_infrastructure_context(), FakeEntityManager())
    driver = BaseJobDriver(FakeProgressManager(_make_job()), runtime)
    assert driver._reraise_on_failure is False
    assert driver._aggregate_always is True
    assert driver._default_failure_code == BatchJobErrorCode.INTERNAL_ERROR


def test_standalone_runtime_drives_inline_failure_policy():
    """A non-provisioning runtime is the scheduler-driven inline path: re-raise,
    aggregate only when this driver prepared, inference default failure."""
    driver = BaseJobDriver(FakeProgressManager(_make_job()), ExternalRuntime(None))
    assert driver._reraise_on_failure is True
    assert driver._aggregate_always is False
    assert driver._default_failure_code == BatchJobErrorCode.INFERENCE_FAILED


def test_factory_unknown_provider_raises_invalid_driver():
    job = SimpleNamespace(spec=SimpleNamespace(compute_provider="providr"))
    with pytest.raises(BatchJobError) as excinfo:
        create_job_driver(
            _make_infrastructure_context(),
            FakeProgressManager(_make_job()),
            entity_manager=FakeEntityManager(),
            job=job,
        )
    assert excinfo.value.code == BatchJobErrorCode.INVALID_DRIVER


def test_factory_kubernetes_without_entity_manager_raises_invalid_driver():
    job = SimpleNamespace(spec=SimpleNamespace(compute_provider="Kubernetes"))
    with pytest.raises(BatchJobError) as excinfo:
        create_job_driver(
            _make_infrastructure_context(),
            FakeProgressManager(_make_job()),
            entity_manager=None,
            job=job,
        )
    assert excinfo.value.code == BatchJobErrorCode.INVALID_DRIVER


def test_factory_external_provider_uses_local_runtime():
    job = SimpleNamespace(spec=SimpleNamespace(compute_provider="External"))
    sentinel = object()
    driver = create_job_driver(
        _make_infrastructure_context(),
        FakeProgressManager(_make_job()),
        entity_manager=FakeEntityManager(),
        job=job,
        endpoint_source=sentinel,
    )
    assert isinstance(driver, BaseJobDriver)
    assert isinstance(driver._runtime, ExternalRuntime)
    assert driver._runtime._source is sentinel
