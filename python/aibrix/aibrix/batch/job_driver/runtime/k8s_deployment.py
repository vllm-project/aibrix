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
"""Deployment-backed execution as a Runtime.

``DeploymentRuntime`` provisions a per-job Deployment+Service, waits for it to be
ready, hands the driver an HTTP endpoint to send requests to, and tears it all
down — implementing the Runtime phases (provision / wait_ready / connect /
teardown) plus cancellation on job deletion. The factory then wires it to a
plain ``BaseJobDriver``; because it provisions, the base derives the
deployment-owns-the-run behaviors (always aggregate, no re-raise to a scheduler,
internal default failure). It registers under the ``Kubernetes`` provider key.
"""

from __future__ import annotations

import asyncio
import os
from dataclasses import dataclass
from typing import Any, Dict, Optional

from kubernetes.client import ApiException

from aibrix.batch.client import EndpointSource
from aibrix.batch.client.sources import (
    DiscoveryEndpointSource,
    InClusterEndpointSource,
    K8sEndpointSliceDiscovery,
    PortForwardEndpointSource,
)
from aibrix.batch.job_driver.runtime import Endpoint, RuntimeBase, register_runtime
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    JobRuntimeRef,
    ResourceDetail,
)
from aibrix.batch.manifest import DeploymentManifestRenderer
from aibrix.context import InfrastructureContext
from aibrix.logger import init_logger

logger = init_logger(__name__)

_K8S_ENDPOINT_SOURCE_ENV = "AIBRIX_BATCH_K8S_ENDPOINT_SOURCE"
_ENDPOINT_SLICE_SOURCE = "endpointslice"


@dataclass
class DeploymentHandle:
    """What ``provision`` produces and the later phases consume/clean up."""

    namespace: str
    deployment_name: str
    service_name: str
    model_name: str
    base_url: str
    service_port: int
    replicas: int = 1
    source: Optional[EndpointSource] = None


class DeploymentRuntime(RuntimeBase):
    """Provision a Deployment+Service per job, reach it over HTTP, tear it down."""

    provisions = True

    def __init__(
        self,
        context: InfrastructureContext,
        renderer: Optional[DeploymentManifestRenderer] = None,
        ready_timeout_seconds: int = 300,
    ) -> None:
        super().__init__(context, ready_timeout_seconds)
        self._renderer = renderer or self._build_renderer(context)
        if context.apps_v1_api is None or context.core_v1_api is None:
            raise BatchJobError(
                BatchJobErrorCode.INVALID_DRIVER,
                "Kubernetes provider requires Kubernetes API clients; start "
                "metadata service with --enable-k8s-support",
            )
        self._apps_v1_api = context.apps_v1_api
        self._core_v1_api = context.core_v1_api
        self._active_handle: Optional[DeploymentHandle] = None

    @staticmethod
    def _build_renderer(context: InfrastructureContext) -> DeploymentManifestRenderer:
        return DeploymentManifestRenderer(
            context.template_registry, context.profile_registry
        )

    def _get_runtime_key(self, job: BatchJob) -> str:
        del job
        return "k8s-deployment"

    def _get_runtime_owner_ref(self, job: BatchJob) -> Optional[str]:
        del job
        if self._active_handle is None:
            return None
        return f"{self._active_handle.namespace}/{self._active_handle.deployment_name}"

    def _get_runtime_reconnect_payload(
        self,
        job: BatchJob,
    ) -> Optional[Dict[str, Any]]:
        payload = super()._get_runtime_reconnect_payload(job) or {}
        if self._active_handle is None:
            return None if not payload else payload
        payload.update(
            {
                "namespace": self._active_handle.namespace,
                "deploymentName": self._active_handle.deployment_name,
                "serviceName": self._active_handle.service_name,
                "modelName": self._active_handle.model_name,
                "baseUrl": self._active_handle.base_url,
                "servicePort": self._active_handle.service_port,
                "replicas": self._active_handle.replicas,
            }
        )
        return payload

    async def _reconnect(
        self, job: BatchJob, job_id: str, execution: JobRuntimeRef
    ) -> DeploymentHandle | None:
        del job
        reconnect_payload = execution.reconnect_payload or {}
        namespace = reconnect_payload.get("namespace")
        deployment_name = reconnect_payload.get("deploymentName")
        service_name = reconnect_payload.get("serviceName")
        model_name = reconnect_payload.get("modelName")
        base_url = reconnect_payload.get("baseUrl")
        service_port = reconnect_payload.get("servicePort")
        replicas = reconnect_payload.get("replicas")
        if not (
            isinstance(namespace, str)
            and namespace
            and isinstance(deployment_name, str)
            and deployment_name
            and isinstance(service_name, str)
            and service_name
            and isinstance(model_name, str)
            and model_name
            and isinstance(base_url, str)
            and base_url
            and isinstance(service_port, int)
            and isinstance(replicas, int)
        ):
            return None

        handle = DeploymentHandle(
            namespace=namespace,
            deployment_name=deployment_name,
            service_name=service_name,
            model_name=model_name,
            base_url=base_url,
            service_port=service_port,
            replicas=replicas,
        )
        self._active_handle = handle
        logger.info(
            "Reconnected Deployment runtime for batch job",
            job_id=job_id,
            namespace=handle.namespace,
            deployment=handle.deployment_name,
            service=handle.service_name,
        )  # type: ignore[call-arg]
        return handle

    # ── Runtime phases ───────────────────────────────────────────────────

    async def _provision(self, job: BatchJob, job_id: str) -> DeploymentHandle:
        if job.job_id is None:
            raise ValueError("job_id is required")
        if job.spec.aibrix is None or job.spec.model_template_name is None:
            raise ValueError("DeploymentRuntime requires spec.aibrix.model_template")
        resource_detail = ResourceDetail()
        resource_allocation = job.spec.aibrix.resource_allocation
        if resource_allocation and resource_allocation.resource_details:
            resource_detail = resource_allocation.resource_details[0]

        rendered = self._renderer.render(job.job_id, job.spec, resource_detail)
        deployment = rendered["deployment"]
        service = rendered["service"]
        model_name = (
            deployment.get("metadata", {})
            .get("labels", {})
            .get("model.aibrix.ai/name", service["metadata"]["name"])
        )
        namespace = deployment.get("metadata", {}).get("namespace", "default")
        handle = DeploymentHandle(
            namespace=namespace,
            deployment_name=deployment["metadata"]["name"],
            service_name=service["metadata"]["name"],
            model_name=model_name,
            base_url=self._service_base_url(namespace, service),
            service_port=int(service["spec"]["ports"][0]["port"]),
            replicas=int(deployment["spec"].get("replicas", 1)),
        )
        self._active_handle = handle

        await self._apply_deployment(namespace, deployment)
        try:
            await self._apply_service(namespace, service)
        except Exception:
            await self._teardown_runtime(handle)
            raise
        logger.info(
            "Provisioned Deployment+Service for batch job",
            job_id=job_id,
            namespace=handle.namespace,
            deployment=handle.deployment_name,
            service=handle.service_name,
            model_name=handle.model_name,
            in_cluster_base_url=handle.base_url,
            service_port=handle.service_port,
            replicas=handle.replicas,
        )  # type: ignore[call-arg]
        return handle

    async def _wait_ready(self, handle: DeploymentHandle) -> None:
        await self._wait_for_deployment_ready(
            namespace=handle.namespace,
            deployment_name=handle.deployment_name,
            replicas=handle.replicas,
        )

    async def _connect(self, handle: DeploymentHandle) -> Endpoint:
        handle.source = self._build_endpoint_source(handle)
        return Endpoint(source=handle.source, model_name=handle.model_name)

    async def _teardown(self, handle: Optional[DeploymentHandle]) -> None:
        if handle is not None:
            await self._teardown_runtime(handle)
            if handle.source is not None:
                await handle.source.aclose()
        self._active_handle = None

    # ── k8s helpers ──────────────────────────────────────────────────────

    @staticmethod
    def _service_base_url(namespace: str, service: dict) -> str:
        port = service["spec"]["ports"][0]["port"]
        service_name = service["metadata"]["name"]
        return f"http://{service_name}.{namespace}.svc.cluster.local:{port}"

    def _build_endpoint_source(self, handle: DeploymentHandle) -> EndpointSource:
        """Pick the endpoint source by known topology, not runtime probing.

        In-cluster control planes reach the Service ClusterIP directly; an
        out-of-cluster control plane tunnels via ``kubectl port-forward``.
        """
        if os.environ.get("KUBERNETES_SERVICE_HOST"):
            if self._use_endpoint_slice_source():
                logger.info(
                    "Dispatch endpoint: in-cluster EndpointSlice discovery",
                    job_id=self._active_job_id,
                    endpoint_source="DiscoveryEndpointSource",
                    namespace=handle.namespace,
                    service=handle.service_name,
                    service_port=handle.service_port,
                )  # type: ignore[call-arg]
                discovery = K8sEndpointSliceDiscovery(
                    self._discovery_v1_api(),
                    namespace=handle.namespace,
                    service_name=handle.service_name,
                    service_port=handle.service_port,
                )
                return DiscoveryEndpointSource(discovery, handle.model_name)
            logger.info(
                "Dispatch endpoint: in-cluster Service (control plane is in-cluster)",
                job_id=self._active_job_id,
                endpoint_source="InClusterEndpointSource",
                base_url=handle.base_url,
                replicas=handle.replicas,
            )  # type: ignore[call-arg]
            return InClusterEndpointSource(handle.base_url, capacity=handle.replicas)
        logger.info(
            "Dispatch endpoint: kubectl port-forward (control plane is out-of-cluster)",
            job_id=self._active_job_id,
            endpoint_source="PortForwardEndpointSource",
            namespace=handle.namespace,
            service=handle.service_name,
            service_port=handle.service_port,
        )  # type: ignore[call-arg]
        return PortForwardEndpointSource(
            handle.namespace, handle.service_name, handle.service_port
        )

    @staticmethod
    def _use_endpoint_slice_source() -> bool:
        value = os.environ.get(_K8S_ENDPOINT_SOURCE_ENV, "").strip().lower()
        return value in {_ENDPOINT_SLICE_SOURCE, "endpoint_slice", "endpoint-slice"}

    def _discovery_v1_api(self):
        discovery_v1_api = self._context.get("discovery_v1_api")
        if discovery_v1_api is not None:
            return discovery_v1_api

        from kubernetes import client as k8s_client

        return k8s_client.DiscoveryV1Api()

    async def _apply_deployment(self, namespace: str, deployment: dict) -> None:
        name = deployment.get("metadata", {}).get("name", "<unknown>")
        try:
            await asyncio.to_thread(
                self._apps_v1_api.create_namespaced_deployment,
                namespace=namespace,
                body=deployment,
            )
        except ApiException as ex:
            if ex.status != 409:
                raise RuntimeError(
                    f"Failed to create Deployment '{name}' in namespace "
                    f"'{namespace}': {ex}"
                ) from ex

    async def _apply_service(self, namespace: str, service: dict) -> None:
        name = service.get("metadata", {}).get("name", "<unknown>")
        try:
            await asyncio.to_thread(
                self._core_v1_api.create_namespaced_service,
                namespace=namespace,
                body=service,
            )
        except ApiException as ex:
            if ex.status != 409:
                raise RuntimeError(
                    f"Failed to create Service '{name}' in namespace "
                    f"'{namespace}': {ex}"
                ) from ex

    async def _wait_for_deployment_ready(
        self, namespace: str, deployment_name: str, replicas: int
    ) -> None:
        deadline = asyncio.get_running_loop().time() + self._ready_timeout_seconds
        while True:
            if self._delete_requested.is_set():
                raise asyncio.CancelledError
            try:
                deployment = await asyncio.to_thread(
                    self._apps_v1_api.read_namespaced_deployment_status,
                    name=deployment_name,
                    namespace=namespace,
                )
            except ApiException as ex:
                if ex.status != 404:
                    raise RuntimeError(
                        f"Failed to read Deployment '{deployment_name}' in "
                        f"namespace '{namespace}': {ex}"
                    ) from ex
                logger.warning(
                    "Deployment not found while waiting for readiness",
                    deployment=deployment_name,
                    namespace=namespace,
                    job_id=self._active_job_id,
                )  # type: ignore[call-arg]
                if asyncio.get_running_loop().time() >= deadline:
                    raise TimeoutError(
                        f"Timed out waiting for deployment '{deployment_name}' "
                        "to appear"
                    ) from ex
                await asyncio.sleep(1)
                continue
            available = deployment.status.available_replicas or 0
            if available >= replicas:
                return
            if asyncio.get_running_loop().time() >= deadline:
                raise TimeoutError(
                    f"Timed out waiting for deployment '{deployment_name}' to become ready"
                )
            await asyncio.sleep(1)

    async def _teardown_runtime(self, handle: DeploymentHandle) -> None:
        await self._delete_service(handle.namespace, handle.service_name)
        await self._delete_deployment(handle.namespace, handle.deployment_name)

    async def _delete_service(self, namespace: str, service_name: str) -> None:
        try:
            await asyncio.to_thread(
                self._core_v1_api.delete_namespaced_service,
                name=service_name,
                namespace=namespace,
            )
        except ApiException as ex:
            if ex.status != 404:
                raise

    async def _delete_deployment(self, namespace: str, deployment_name: str) -> None:
        try:
            await asyncio.to_thread(
                self._apps_v1_api.delete_namespaced_deployment,
                name=deployment_name,
                namespace=namespace,
            )
        except ApiException as ex:
            if ex.status != 404:
                raise


register_runtime(
    "Kubernetes",
    lambda *, context, **_: DeploymentRuntime(context),
)
