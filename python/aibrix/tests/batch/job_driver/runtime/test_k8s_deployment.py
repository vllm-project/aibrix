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
from datetime import datetime, timezone
from types import SimpleNamespace

import pytest
from kubernetes.client import ApiException

from aibrix.batch.client.sources import (
    InClusterEndpointSource,
    PortForwardEndpointSource,
)
from aibrix.batch.job_driver import (
    BaseJobDriver,
    DeploymentRuntime,
    ExternalRuntime,
    TerminateResult,
)
from aibrix.batch.job_driver.driver_factory import create_job_driver
from aibrix.batch.job_driver.runtime.k8s_deployment import DeploymentHandle
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    JobRuntimeRef,
    ObjectMeta,
    TypeMeta,
)
from aibrix.batch.manifest.renderer import RenderError
from aibrix.context import InfrastructureContext


class FakeAppsV1Api:
    def __init__(self, *, available_replicas: int = 1):
        self.created: list[tuple[str, dict]] = []
        self.deleted: list[tuple[str, str]] = []
        self.available_replicas = available_replicas

    def create_namespaced_deployment(self, namespace: str, body: dict):
        self.created.append((namespace, body))

    def read_namespaced_deployment_status(self, name: str, namespace: str):
        del name, namespace
        return SimpleNamespace(
            status=SimpleNamespace(available_replicas=self.available_replicas)
        )

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
    def __init__(self):
        self.provider_specs = []

    def render(self, job_id: str, spec: BatchJobSpec, provider_spec):
        assert job_id is not None
        assert spec.model_template_name == "mock-template"
        self.provider_specs.append(provider_spec)
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
            "runtime": {"target": "Kubernetes"},
            "resource_allocation": {
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


def _make_job_without_resource_details() -> BatchJob:
    job = _make_job()
    assert job.spec.aibrix is not None
    assert job.spec.aibrix.resource_allocation is not None
    job.spec.aibrix.resource_allocation.resource_details = None
    return job


def _make_infrastructure_context(
    apps_v1_api=object(), core_v1_api=object()
) -> InfrastructureContext:
    return InfrastructureContext(
        template_registry=None,
        profile_registry=None,
        apps_v1_api=apps_v1_api,
        core_v1_api=core_v1_api,
    )


def _make_runtime(
    *,
    apps_v1_api=None,
    core_v1_api=None,
    renderer=None,
    ready_timeout_seconds: int = 300,
) -> DeploymentRuntime:
    return DeploymentRuntime(
        _make_infrastructure_context(
            apps_v1_api=apps_v1_api or FakeAppsV1Api(),
            core_v1_api=core_v1_api or FakeCoreV1Api(),
        ),
        renderer=renderer,
        ready_timeout_seconds=ready_timeout_seconds,
    )


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_allows_missing_template_registry_at_construction():
    runtime = DeploymentRuntime(_make_infrastructure_context())

    assert runtime._renderer is not None


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_reports_missing_template_registry_at_provision_time():
    runtime = DeploymentRuntime(_make_infrastructure_context())

    with pytest.raises(RenderError, match="template registry is not configured"):
        await runtime._provision(_make_job(), "job-render-test")


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_provision_creates_resources_and_handle():
    apps_api = FakeAppsV1Api()
    core_api = FakeCoreV1Api()
    runtime = _make_runtime(
        apps_v1_api=apps_api,
        core_v1_api=core_api,
        renderer=FakeRenderer(),
    )

    handle = await runtime._provision(_make_job(), "job-123456789abc")

    assert handle.namespace == "default"
    assert handle.deployment_name == "rendered-deployment"
    assert handle.service_name == "rendered-service"
    assert handle.model_name == "rendered-model"
    assert handle.base_url == "http://rendered-service.default.svc.cluster.local:8000"
    assert handle.service_port == 8000
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

    await runtime._teardown(handle)
    assert core_api.deleted == [("default", "rendered-service")]
    assert apps_api.deleted == [("default", "rendered-deployment")]


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_defaults_when_resource_details_absent():
    renderer = FakeRenderer()
    apps_api = FakeAppsV1Api()
    runtime = _make_runtime(
        apps_v1_api=apps_api,
        core_v1_api=FakeCoreV1Api(),
        renderer=renderer,
    )

    await runtime._provision(_make_job_without_resource_details(), "job-123456789abc")

    assert len(renderer.provider_specs) == 1
    assert renderer.provider_specs[0].replica is None
    assert apps_api.created[0][1]["spec"]["replicas"] == 1


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_builds_execution_ref_with_current_payload_shape():
    job = _make_job()
    runtime = _make_runtime(renderer=FakeRenderer())
    await runtime._provision(job, job.job_id)

    execution = runtime._build_runtime_ref(job)

    assert execution is not None
    assert execution.driver_type == "k8s-deployment"
    assert execution.owner_ref == "default:rendered-deployment"
    assert execution.reconnect_payload == {
        "namespace": "default",
        "deploymentName": "rendered-deployment",
        "serviceName": "rendered-service",
        "modelName": "rendered-model",
        "baseUrl": "http://rendered-service.default.svc.cluster.local:8000",
        "servicePort": 8000,
        "replicas": 1,
    }


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_reconnect_accepts_current_payload_keys():
    job = _make_job()
    runtime = _make_runtime(renderer=FakeRenderer())

    handle = await runtime._reconnect(
        job,
        job.job_id,
        JobRuntimeRef(
            driverType="k8s-deployment",
            ownerRef="default:rendered-deployment",
            reconnectPayload={
                "namespace": "default",
                "deploymentName": "rendered-deployment",
                "serviceName": "rendered-service",
                "modelName": "rendered-model",
                "baseUrl": "http://rendered-service.default.svc.cluster.local:8000",
                "servicePort": 8000,
                "replicas": 1,
            },
        ),
    )

    assert handle is not None
    assert handle.namespace == "default"
    assert handle.deployment_name == "rendered-deployment"
    assert handle.service_name == "rendered-service"
    assert handle.model_name == "rendered-model"
    assert runtime._active_handle == handle


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_check_liveness_accepts_ready_deployment():
    runtime = _make_runtime(apps_v1_api=FakeAppsV1Api(available_replicas=1))

    await runtime._check_liveness(
        DeploymentHandle(
            namespace="default",
            deployment_name="rendered-deployment",
            service_name="rendered-service",
            model_name="rendered-model",
            base_url="http://rendered-service.default.svc.cluster.local:8000",
            service_port=8000,
            replicas=1,
        )
    )


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_check_liveness_accepts_existing_unready_deployment():
    runtime = _make_runtime(apps_v1_api=FakeAppsV1Api(available_replicas=0))

    await runtime._check_liveness(
        DeploymentHandle(
            namespace="default",
            deployment_name="rendered-deployment",
            service_name="rendered-service",
            model_name="rendered-model",
            base_url="http://rendered-service.default.svc.cluster.local:8000",
            service_port=8000,
            replicas=1,
        )
    )


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_check_liveness_reports_missing_deployment():
    class _MissingAppsV1Api(FakeAppsV1Api):
        def read_namespaced_deployment_status(self, name: str, namespace: str):
            del name, namespace
            raise ApiException(status=404, reason="Not Found")

    runtime = _make_runtime(apps_v1_api=_MissingAppsV1Api())

    with pytest.raises(BatchJobError) as exc_info:
        await runtime._check_liveness(
            DeploymentHandle(
                namespace="default",
                deployment_name="rendered-deployment",
                service_name="rendered-service",
                model_name="rendered-model",
                base_url="http://rendered-service.default.svc.cluster.local:8000",
                service_port=8000,
                replicas=1,
            )
        )

    assert exc_info.value.code == BatchJobErrorCode.RESOURCE_NOTFOUND_ERROR


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_connect_uses_in_cluster_source(monkeypatch):
    runtime = _make_runtime(renderer=FakeRenderer())
    handle = DeploymentHandle(
        namespace="default",
        deployment_name="rendered-deployment",
        service_name="rendered-service",
        model_name="rendered-model",
        base_url="http://rendered-service.default.svc.cluster.local:8000",
        service_port=8000,
        replicas=1,
    )

    monkeypatch.setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
    endpoint = await runtime._connect(handle)

    assert isinstance(endpoint.source, InClusterEndpointSource)
    assert endpoint.model_name == "rendered-model"


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_connect_uses_port_forward_source_outside_cluster(
    monkeypatch,
):
    runtime = _make_runtime(renderer=FakeRenderer())
    handle = DeploymentHandle(
        namespace="default",
        deployment_name="rendered-deployment",
        service_name="rendered-service",
        model_name="rendered-model",
        base_url="http://rendered-service.default.svc.cluster.local:8000",
        service_port=8000,
        replicas=1,
    )

    monkeypatch.delenv("KUBERNETES_SERVICE_HOST", raising=False)
    endpoint = await runtime._connect(handle)

    assert isinstance(endpoint.source, PortForwardEndpointSource)
    assert endpoint.model_name == "rendered-model"


@pytest.mark.asyncio
async def test_k8s_deployment_runtime_terminate_cancels_session_and_tears_down():
    job = _make_job("job-delete-1234")
    apps_api = FakeAppsV1Api()
    core_api = FakeCoreV1Api()
    runtime = _make_runtime(
        apps_v1_api=apps_api,
        core_v1_api=core_api,
        renderer=FakeRenderer(),
    )

    async def _update_job_local_status(job_id, worker_id, status, update_keys=None):
        del worker_id, update_keys
        return SimpleNamespace(job_id=job_id, status=status)

    progress_manager = SimpleNamespace(update_job_local_status=_update_job_local_status)
    wait_entered = asyncio.Event()

    async def _blocked_wait_ready(handle, wait_mode="provision"):
        del handle, wait_mode
        wait_entered.set()
        await runtime._stop_requested.wait()
        raise asyncio.CancelledError

    runtime._wait_ready = _blocked_wait_ready

    async def _run_session():
        async with runtime.session(
            job,
            job.job_id,
            progress_manager=progress_manager,
            worker_id_generator=lambda owner_ref: f"{owner_ref}-worker-1",
        ):
            raise AssertionError("session body should not run after termination")

    task = asyncio.create_task(_run_session())
    await asyncio.wait_for(wait_entered.wait(), timeout=1)

    deleted = await runtime.terminate(job)

    assert deleted is TerminateResult.ACCEPTED
    with pytest.raises(asyncio.CancelledError):
        await task
    assert core_api.deleted == [("default", "rendered-service")]
    assert apps_api.deleted == [("default", "rendered-deployment")]


@pytest.mark.asyncio
async def test_create_job_driver_uses_k8s_deployment_runtime_for_kubernetes_target():
    job = _make_job()

    driver = create_job_driver(
        _make_infrastructure_context(),
        progress_manager=SimpleNamespace(),
        entity_manager=None,
        job=job,
    )

    assert isinstance(driver, BaseJobDriver)
    assert isinstance(driver._runtime, DeploymentRuntime)


@pytest.mark.asyncio
async def test_create_job_driver_passes_infrastructure_context_to_k8s_deployment_runtime():
    job = _make_job()

    driver = create_job_driver(
        _make_infrastructure_context(
            apps_v1_api="apps-api",
            core_v1_api="core-api",
        ),
        progress_manager=SimpleNamespace(),
        entity_manager=None,
        job=job,
    )

    runtime = driver._runtime
    assert isinstance(runtime, DeploymentRuntime)
    assert runtime._apps_v1_api == "apps-api"
    assert runtime._core_v1_api == "core-api"


def test_provisioning_runtime_drives_base_failure_policy():
    runtime = DeploymentRuntime(_make_infrastructure_context())
    driver = BaseJobDriver(SimpleNamespace(), runtime)

    assert driver._reraise_on_failure is False
    assert driver._default_failure_code == BatchJobErrorCode.INTERNAL_ERROR


def test_standalone_runtime_drives_inline_failure_policy():
    driver = BaseJobDriver(SimpleNamespace(), ExternalRuntime(None))

    assert driver._reraise_on_failure is True
    assert driver._default_failure_code == BatchJobErrorCode.INFERENCE_FAILED


def test_factory_unknown_runtime_raises_invalid_driver():
    job = SimpleNamespace(spec=SimpleNamespace(runtime_target="providr"))

    with pytest.raises(BatchJobError) as excinfo:
        create_job_driver(
            _make_infrastructure_context(),
            SimpleNamespace(),
            entity_manager=None,
            job=job,
        )

    assert excinfo.value.code == BatchJobErrorCode.INVALID_DRIVER


def test_factory_kubernetes_without_k8s_context_raises_invalid_driver():
    job = SimpleNamespace(spec=SimpleNamespace(runtime_target="Kubernetes"))

    with pytest.raises(BatchJobError) as excinfo:
        create_job_driver(
            InfrastructureContext(),
            SimpleNamespace(),
            entity_manager=None,
            job=job,
        )

    assert excinfo.value.code == BatchJobErrorCode.INVALID_DRIVER
    assert "--enable-k8s-support" in excinfo.value.message


def test_factory_external_runtime_uses_injected_endpoint_source():
    job = SimpleNamespace(spec=SimpleNamespace(runtime_target="External"))
    sentinel = object()

    driver = create_job_driver(
        _make_infrastructure_context(),
        SimpleNamespace(),
        entity_manager=None,
        job=job,
        endpoint_source=sentinel,
    )

    assert isinstance(driver, BaseJobDriver)
    assert isinstance(driver._runtime, ExternalRuntime)
    assert driver._runtime._source is sentinel
