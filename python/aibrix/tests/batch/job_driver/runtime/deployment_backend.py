from __future__ import annotations

import asyncio
from typing import Any

from kubernetes import client

import aibrix.batch.job_driver.runtime.k8s_deployment as deployment_runtime_module
from aibrix.batch.client.channel import EchoChannel
from aibrix.batch.client.errors import InferenceError, InferenceErrorCode
from aibrix.batch.client.sources import NoopEndpointSource
from tests.batch.job_driver.runtime.backends import (
    RuntimePatchBackend,
    RuntimePatchTarget,
    register_runtime_patch_backend,
)

ORIGINAL_DEPLOYMENT_TEARDOWN_RUNTIME = (
    deployment_runtime_module.DeploymentRuntime._teardown_runtime
)


class InterruptibleEchoChannel(EchoChannel):
    def __init__(
        self,
        *,
        delay: float = 0.0,
        availability_check=None,
        id: str = "echo",
    ) -> None:
        super().__init__(delay=delay, id=id)
        self._availability_check = availability_check

    async def send(self, request):
        if self._delay:
            await asyncio.sleep(self._delay)
        if callable(self._availability_check) and not self._availability_check():
            raise InferenceError(
                InferenceErrorCode.NO_ENDPOINT,
                f"fake runtime endpoint {self.id} is unavailable",
                retryable=False,
            )
        return request.payload


class FastNoopEndpointSource(NoopEndpointSource):
    def __init__(self, context, *, availability_check=None, channel_id: str = "echo"):
        self._context = context
        super().__init__(
            delay=context.values.get(
                "endpoint_source_delay_seconds",
                0.0,
            )
        )
        self._channel = InterruptibleEchoChannel(
            delay=context.values.get("endpoint_source_delay_seconds", 0.0),
            availability_check=availability_check,
            id=channel_id,
        )


class FakeDeploymentAppsV1Api:
    def __init__(self):
        self.created: list[tuple[str, dict[str, Any]]] = []
        self.deleted: list[tuple[str, str]] = []
        self._existing: set[tuple[str, str]] = set()

    def create_namespaced_deployment(self, namespace: str, body: dict[str, Any]):
        self.created.append((namespace, body))
        self._existing.add((namespace, body["metadata"]["name"]))

    def read_namespaced_deployment_status(self, name: str, namespace: str):
        if (namespace, name) not in self._existing:
            raise client.ApiException(status=404)
        return type(
            "DeploymentStatus",
            (),
            {"status": type("Status", (), {"available_replicas": 1})()},
        )()

    def delete_namespaced_deployment(self, name: str, namespace: str):
        if (namespace, name) not in self._existing:
            raise client.ApiException(status=404)
        self.deleted.append((namespace, name))
        self._existing.remove((namespace, name))

    def deployment_exists(self, namespace: str, name: str) -> bool:
        return (namespace, name) in self._existing


class FakeDeploymentCoreV1Api:
    def __init__(self):
        self.created: list[tuple[str, dict[str, Any]]] = []
        self.deleted: list[tuple[str, str]] = []
        self._existing: set[tuple[str, str]] = set()

    def create_namespaced_service(self, namespace: str, body: dict[str, Any]):
        self.created.append((namespace, body))
        self._existing.add((namespace, body["metadata"]["name"]))

    def read_namespaced_service(self, name: str, namespace: str):
        if (namespace, name) not in self._existing:
            raise client.ApiException(status=404)
        metadata = type("Metadata", (), {"name": name, "namespace": namespace})()
        return type("Service", (), {"metadata": metadata})()

    def delete_namespaced_service(self, name: str, namespace: str):
        if (namespace, name) not in self._existing:
            raise client.ApiException(status=404)
        self.deleted.append((namespace, name))
        self._existing.remove((namespace, name))

    def service_exists(self, namespace: str, name: str) -> bool:
        return (namespace, name) in self._existing


class FakeDeploymentRenderer:
    def render(
        self,
        job_id: str,
        spec,
        prividerSpec,
    ):
        del spec, prividerSpec
        deployment_name = f"batch-{job_id[:8]}-engine"
        return {
            "deployment": {
                "apiVersion": "apps/v1",
                "kind": "Deployment",
                "metadata": {
                    "name": deployment_name,
                    "namespace": "default",
                    "labels": {"model.aibrix.ai/name": deployment_name},
                },
                "spec": {
                    "replicas": 1,
                    "selector": {
                        "matchLabels": {
                            "app": deployment_name,
                            "model.aibrix.ai/name": deployment_name,
                        }
                    },
                    "template": {
                        "metadata": {
                            "labels": {
                                "app": deployment_name,
                                "model.aibrix.ai/name": deployment_name,
                            }
                        },
                        "spec": {"containers": [{"name": "llm-engine"}]},
                    },
                },
            },
            "service": {
                "apiVersion": "v1",
                "kind": "Service",
                "metadata": {
                    "name": deployment_name,
                    "namespace": "default",
                    "labels": {"model.aibrix.ai/name": deployment_name},
                },
                "spec": {
                    "selector": {"model.aibrix.ai/name": deployment_name},
                    "ports": [{"port": 8000, "targetPort": 8000}],
                    "type": "ClusterIP",
                },
            },
        }


def configure_local_metastore_deployment_backend(app, monkeypatch) -> None:
    context = app.state.batch_driver._context
    shared_state = getattr(monkeypatch, "_aibrix_fake_deployment_backend_state", None)
    if shared_state is None:
        shared_state = {
            "apps_v1_api": FakeDeploymentAppsV1Api(),
            "core_v1_api": FakeDeploymentCoreV1Api(),
            "deployment_teardown_calls": [],
            "deployment_endpoint_source_builds": [],
        }
        setattr(monkeypatch, "_aibrix_fake_deployment_backend_state", shared_state)

    apps_v1_api = shared_state["apps_v1_api"]
    core_v1_api = shared_state["core_v1_api"]
    context.apps_v1_api = apps_v1_api
    context.core_v1_api = core_v1_api
    context.values["deployment_apps_v1_api"] = apps_v1_api
    context.values["deployment_core_v1_api"] = core_v1_api
    context.values["deployment_teardown_calls"] = shared_state[
        "deployment_teardown_calls"
    ]
    context.values["deployment_endpoint_source_builds"] = shared_state[
        "deployment_endpoint_source_builds"
    ]

    monkeypatch.setattr(
        deployment_runtime_module.DeploymentRuntime,
        "_build_renderer",
        staticmethod(lambda _context: FakeDeploymentRenderer()),
    )

    def _build_test_endpoint_source(self, handle):
        self._context.values["deployment_endpoint_source_builds"].append(
            handle.deployment_name
        )
        return FastNoopEndpointSource(
            self._context,
            availability_check=lambda: (
                apps_v1_api.deployment_exists(handle.namespace, handle.deployment_name)
                and core_v1_api.service_exists(handle.namespace, handle.deployment_name)
            ),
            channel_id=handle.deployment_name,
        )

    async def _recording_teardown(self, runtime):
        self._context.values["deployment_teardown_calls"].append(
            {
                "job_id": self._active_job_id,
                "deployment_name": runtime.deployment_name,
            }
        )
        return await ORIGINAL_DEPLOYMENT_TEARDOWN_RUNTIME(self, runtime)

    monkeypatch.setattr(
        deployment_runtime_module.DeploymentRuntime,
        "_build_endpoint_source",
        _build_test_endpoint_source,
    )
    monkeypatch.setattr(
        deployment_runtime_module.DeploymentRuntime,
        "_teardown_runtime",
        _recording_teardown,
    )


register_runtime_patch_backend(
    "deployment",
    RuntimePatchBackend(
        runtime_class=deployment_runtime_module.DeploymentRuntime,
        create=RuntimePatchTarget(
            "aibrix.batch.job_driver.runtime.k8s_deployment."
            "DeploymentRuntime._provision",
            deployment_runtime_module.DeploymentRuntime._provision,
        ),
        teardown=RuntimePatchTarget(
            "aibrix.batch.job_driver.runtime.k8s_deployment."
            "DeploymentRuntime._teardown",
            deployment_runtime_module.DeploymentRuntime._teardown,
        ),
        delete_wait=RuntimePatchTarget(
            "aibrix.batch.job_driver.runtime.k8s_deployment."
            "DeploymentRuntime._wait_teared_down",
            deployment_runtime_module.DeploymentRuntime._wait_teared_down,
        ),
        should_teardown=RuntimePatchTarget(
            "aibrix.batch.job_driver.runtime.k8s_deployment."
            "DeploymentRuntime._should_teardown_runtime_handle",
            deployment_runtime_module.DeploymentRuntime._should_teardown_runtime_handle,
        ),
    ),
)
