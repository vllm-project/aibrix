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
from typing import Optional

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
    ResourceDetail,
)
from aibrix.batch.manifest import DeploymentManifestRenderer
from aibrix.batch.state import JobEntityManager
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
        entity_manager: JobEntityManager,
        renderer: Optional[DeploymentManifestRenderer] = None,
        ready_timeout_seconds: int = 300,
    ) -> None:
        if entity_manager is None:
            # The factory forwards entity_manager unconditionally; a
            # provisioning runtime needs it for delete-driven cancellation.
            # Fail clearly here instead of an opaque NoneType error below.
            raise BatchJobError(
                BatchJobErrorCode.INVALID_DRIVER,
                "Kubernetes provider requires a job entity manager",
            )
        self._context = context
        self._entity_manager = entity_manager
        self._renderer = renderer or self._build_renderer(context)
        if context.apps_v1_api is None or context.core_v1_api is None:
            raise BatchJobError(
                BatchJobErrorCode.INVALID_DRIVER,
                "Kubernetes provider requires Kubernetes API clients; start "
                "metadata service with --enable-k8s-support",
            )
        self._apps_v1_api = context.apps_v1_api
        self._core_v1_api = context.core_v1_api
        self._ready_timeout_seconds = ready_timeout_seconds
        self._mgr_deleted_handler = entity_manager.on_job_deleted(
            self._job_deleted_handler
        )
        self._active_job_id: Optional[str] = None
        self._active_task: Optional[asyncio.Task] = None
        self._active_handle: Optional[DeploymentHandle] = None
        self._delete_requested = asyncio.Event()

    def cancelled(self) -> bool:
        return self._delete_requested.is_set()

    @staticmethod
    def _build_renderer(context: InfrastructureContext) -> DeploymentManifestRenderer:
        return DeploymentManifestRenderer(
            context.template_registry, context.profile_registry
        )

    async def _job_deleted_handler(self, deleted_job: BatchJob) -> bool:
        deleted_job_id = deleted_job.job_id
        if deleted_job_id and deleted_job_id == self._active_job_id:
            self._delete_requested.set()
            if self._active_task is not None and not self._active_task.done():
                self._active_task.cancel()

        if self._mgr_deleted_handler is None:
            return True
        return await self._mgr_deleted_handler(deleted_job)

    # ── Runtime phases ───────────────────────────────────────────────────

    async def _provision(self, job: BatchJob, job_id: str) -> DeploymentHandle:
        self._active_job_id = job_id
        self._active_task = asyncio.current_task()
        self._delete_requested.clear()

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
        self._active_job_id = None
        self._active_task = None
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
    lambda *, context, entity_manager, **_: DeploymentRuntime(context, entity_manager),
)
