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
import json
import re
import subprocess
import time
from dataclasses import dataclass
from typing import Optional
from urllib.parse import urljoin

import httpx
from kubernetes import client as k8s_client
from kubernetes.client import ApiException

from aibrix.batch.job_entity import BatchJob, BatchJobState, JobEntityManager
from aibrix.batch.manifest import DeploymentManifestRenderer
from aibrix.context import InfrastructureContext
from aibrix.logger import init_logger

from .local_driver import LocalJobDriver
from .progress_manager import JobProgressManager

logger = init_logger(__name__)


@dataclass
class DeploymentRuntime:
    namespace: str
    deployment_name: str
    service_name: str
    model_name: str
    base_url: str
    service_port: int


class KubernetesServiceInferenceClient:
    _gateway_base_url = "http://127.0.0.1:8888"
    _PORT_FORWARD_URL_RE = re.compile(r"(?:127\.0\.0\.1|localhost):(\d+)")

    def __init__(
        self,
        core_v1_api: k8s_client.CoreV1Api,
        namespace: str,
        service_name: str,
        model_name: str,
        service_port: int,
        base_url: str,
    ) -> None:
        self._core_v1_api = core_v1_api
        self._namespace = namespace
        self._service_name = service_name
        self._model_name = model_name
        self._service_port = service_port
        self.base_url = base_url
        self._port_forward_process: Optional[subprocess.Popen[str]] = None
        self._fallback_base_url: Optional[str] = None

    async def inference_request(self, endpoint: str, request_data):
        request_payload = dict(request_data)
        request_payload["model"] = self._model_name
        attempt_failures: list[str] = []
        try:
            return await asyncio.to_thread(
                self._proxy_inference_request,
                endpoint,
                request_payload,
            )
        except Exception as ex:
            attempt_failures.append(f"service proxy failed: {ex}")
            try:
                result = await self._gateway_inference_request(
                    endpoint, request_payload
                )
                logger.warning(
                    "Inference request succeeded via gateway after fallback",
                    service_name=self._service_name,
                    namespace=self._namespace,
                    succeeded_via="gateway",
                    attempts_failed=attempt_failures,
                )  # type: ignore[call-arg]
                return result
            except Exception as gateway_ex:
                attempt_failures.append(f"gateway failed: {gateway_ex}")
                result = await self._fallback_inference_request(
                    endpoint, request_payload
                )
                logger.warning(
                    "Inference request succeeded via port-forward after fallback",
                    service_name=self._service_name,
                    namespace=self._namespace,
                    succeeded_via="port-forward",
                    attempts_failed=attempt_failures,
                )  # type: ignore[call-arg]
                return result

    def close(self) -> None:
        if self._port_forward_process is None:
            return
        if self._port_forward_process.poll() is None:
            self._port_forward_process.terminate()
            try:
                self._port_forward_process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._port_forward_process.kill()
                self._port_forward_process.wait(timeout=5)
        self._port_forward_process = None
        self._fallback_base_url = None

    def _proxy_inference_request(self, endpoint: str, request_data):
        payload = self._core_v1_api.api_client.call_api(
            "/api/v1/namespaces/{namespace}/services/{name}/proxy/{path}",
            "POST",
            path_params={
                "namespace": self._namespace,
                "name": self._service_name,
                "path": endpoint.lstrip("/"),
            },
            header_params={
                "Accept": "application/json",
                "Content-Type": "application/json",
            },
            body=request_data,
            response_type="str",
            auth_settings=["BearerToken"],
            _return_http_data_only=True,
        )
        if isinstance(payload, bytes):
            payload = payload.decode("utf-8")
        return json.loads(payload)

    async def _fallback_inference_request(self, endpoint: str, request_data):
        if self._fallback_base_url is None:
            self._fallback_base_url = await asyncio.to_thread(self._start_port_forward)
        async with httpx.AsyncClient() as client:
            response = await client.post(
                urljoin(self._fallback_base_url, endpoint),
                json=request_data,
                timeout=30.0,
            )
            response.raise_for_status()
            return response.json()

    async def _gateway_inference_request(self, endpoint: str, request_data):
        async with httpx.AsyncClient() as client:
            response = await client.post(
                urljoin(self._gateway_base_url, endpoint),
                json=request_data,
                timeout=30.0,
            )
            response.raise_for_status()
            return response.json()

    def _start_port_forward(self) -> str:
        if (
            self._port_forward_process is not None
            and self._fallback_base_url is not None
        ):
            return self._fallback_base_url
        process = subprocess.Popen(
            [
                "kubectl",
                "-n",
                self._namespace,
                "port-forward",
                f"service/{self._service_name}",
                f":{self._service_port}",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        deadline = time.time() + 20
        while time.time() < deadline:
            if process.poll() is not None:
                output = process.stdout.read() if process.stdout is not None else ""
                raise RuntimeError(
                    f"port-forward exited early for {self._service_name}: {output}"
                )
            line = process.stdout.readline() if process.stdout is not None else ""
            if match := self._PORT_FORWARD_URL_RE.search(line):
                local_port = int(match.group(1))
                self._port_forward_process = process
                return f"http://127.0.0.1:{local_port}"
        process.terminate()
        raise TimeoutError(
            f"Timed out waiting for port-forward for {self._service_name}"
        )


class DeploymentJobDriver(LocalJobDriver):
    def __init__(
        self,
        context: InfrastructureContext,
        progress_manager: JobProgressManager,
        entity_manager: JobEntityManager,
        renderer: Optional[DeploymentManifestRenderer] = None,
        ready_timeout_seconds: int = 300,
    ):
        super().__init__(progress_manager)
        self._context = context
        self._entity_manager = entity_manager
        self._renderer = renderer or self._build_renderer(context)
        self._apps_v1_api = context.apps_v1_api or k8s_client.AppsV1Api()
        self._core_v1_api = context.core_v1_api or k8s_client.CoreV1Api()
        self._ready_timeout_seconds = ready_timeout_seconds
        self._mgr_deleted_handler = entity_manager.on_job_deleted(
            self._job_deleted_handler
        )
        self._active_job_id: Optional[str] = None
        self._active_task: Optional[asyncio.Task[None]] = None
        self._active_runtime: Optional[DeploymentRuntime] = None
        self._delete_requested = asyncio.Event()

    @staticmethod
    def _build_renderer(context: InfrastructureContext) -> DeploymentManifestRenderer:
        if context.template_registry is None:
            raise ValueError("DeploymentJobDriver requires template registry")
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

    async def _create_runtime(self, job: BatchJob) -> DeploymentRuntime:
        if job.job_id is None:
            raise ValueError("job_id is required")
        if job.spec.aibrix is None or job.spec.model_template_name is None:
            raise ValueError("DeploymentJobDriver requires spec.aibrix.model_template")

        deployment_name = self._deployment_name(job.job_id)
        service_name = self._service_name(job.job_id)
        model_name = service_name
        rendered = self._renderer.render(
            template_name=job.spec.model_template_name,
            profile_name=job.spec.aibrix.profile_name if job.spec.aibrix else None,
            template_version=job.spec.aibrix.model_template.version
            if job.spec.aibrix.model_template
            else None,
            deployment_name=deployment_name,
        )
        deployment = rendered["deployment"]
        service = rendered["service"]
        namespace = deployment["metadata"].get("namespace", "default")

        self._apply_job_identity(
            deployment,
            service,
            job.job_id,
            service_name,
            model_name,
        )

        await self._apply_deployment(namespace, deployment)
        try:
            await self._apply_service(namespace, service)
        except Exception:
            await self._teardown_runtime(
                DeploymentRuntime(
                    namespace=namespace,
                    deployment_name=deployment["metadata"]["name"],
                    service_name=service["metadata"]["name"],
                    model_name=model_name,
                    base_url=self._service_base_url(namespace, service),
                    service_port=int(service["spec"]["ports"][0]["port"]),
                )
            )
            raise

        await self._wait_for_deployment_ready(
            namespace=namespace,
            deployment_name=deployment["metadata"]["name"],
            replicas=int(deployment["spec"].get("replicas", 1)),
        )
        return DeploymentRuntime(
            namespace=namespace,
            deployment_name=deployment["metadata"]["name"],
            service_name=service["metadata"]["name"],
            model_name=model_name,
            base_url=self._service_base_url(namespace, service),
            service_port=int(service["spec"]["ports"][0]["port"]),
        )

    @staticmethod
    def _deployment_name(job_id: str) -> str:
        return f"batch-{job_id[:12]}-engine"

    @staticmethod
    def _service_name(job_id: str) -> str:
        return f"batch-{job_id[:12]}-svc"

    @staticmethod
    def _apply_job_identity(
        deployment: dict,
        service: dict,
        job_id: str,
        service_name: str,
        model_name: str,
    ) -> None:
        job_label = {"batch.job.aibrix.ai/job-id": job_id}
        model_label = {
            "model.aibrix.ai/name": model_name,
            "model.aibrix.ai/port": str(service["spec"]["ports"][0]["port"]),
        }
        deployment.setdefault("metadata", {}).setdefault("labels", {}).update(job_label)
        deployment["metadata"]["labels"].update(model_label)
        deployment["spec"]["selector"]["matchLabels"].update(job_label)
        deployment["spec"]["selector"]["matchLabels"].update(model_label)
        deployment["spec"]["template"]["metadata"].setdefault("labels", {}).update(
            job_label
        )
        deployment["spec"]["template"]["metadata"]["labels"].update(model_label)
        service["metadata"]["name"] = service_name
        service.setdefault("metadata", {}).setdefault("labels", {}).update(model_label)
        service["spec"]["selector"] = dict(
            deployment["spec"]["selector"]["matchLabels"]
        )

    @staticmethod
    def _service_base_url(namespace: str, service: dict) -> str:
        port = service["spec"]["ports"][0]["port"]
        service_name = service["metadata"]["name"]
        return f"http://{service_name}.{namespace}.svc.cluster.local:{port}"

    async def _apply_deployment(self, namespace: str, deployment: dict) -> None:
        try:
            await asyncio.to_thread(
                self._apps_v1_api.create_namespaced_deployment,
                namespace=namespace,
                body=deployment,
            )
        except ApiException as ex:
            if ex.status != 409:
                raise

    async def _apply_service(self, namespace: str, service: dict) -> None:
        try:
            await asyncio.to_thread(
                self._core_v1_api.create_namespaced_service,
                namespace=namespace,
                body=service,
            )
        except ApiException as ex:
            if ex.status != 409:
                raise

    async def _wait_for_deployment_ready(
        self, namespace: str, deployment_name: str, replicas: int
    ) -> None:
        deadline = asyncio.get_running_loop().time() + self._ready_timeout_seconds
        while True:
            if self._delete_requested.is_set():
                raise asyncio.CancelledError
            deployment = await asyncio.to_thread(
                self._apps_v1_api.read_namespaced_deployment_status,
                name=deployment_name,
                namespace=namespace,
            )
            available = deployment.status.available_replicas or 0
            if available >= replicas:
                return
            if asyncio.get_running_loop().time() >= deadline:
                raise TimeoutError(
                    f"Timed out waiting for deployment '{deployment_name}' to become ready"
                )
            await asyncio.sleep(1)

    async def _teardown_runtime(self, runtime: DeploymentRuntime) -> None:
        await self._delete_service(runtime.namespace, runtime.service_name)
        await self._delete_deployment(runtime.namespace, runtime.deployment_name)

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

    async def execute_job(self, job_id):  # type: ignore[override]
        try:
            return await self._execute_job_inner(job_id)
        finally:
            if self._active_runtime is not None:
                await self._teardown_runtime(self._active_runtime)
            if self._inference_client is not None and hasattr(
                self._inference_client, "close"
            ):
                await asyncio.to_thread(self._inference_client.close)
            self._inference_client = None
            self._active_job_id = None
            self._active_task = None
            self._active_runtime = None

    async def _execute_job_inner(self, job_id):
        job = await self._progress_manager.get_job(job_id)
        if job is None:
            logger.warning("Job not found, skip deployment execution.", job_id=job_id)  # type: ignore[call-arg]
            return

        self._active_job_id = job_id
        self._active_task = asyncio.current_task()
        self._delete_requested.clear()
        runtime = await self._create_runtime(job)
        self._active_runtime = runtime
        self._inference_client = KubernetesServiceInferenceClient(
            self._core_v1_api,
            runtime.namespace,
            runtime.service_name,
            runtime.model_name,
            runtime.service_port,
            runtime.base_url,
        )

        try:
            has_temp_files = bool(job.status.temp_output_file_id) and bool(
                job.status.temp_error_file_id
            )
            if not has_temp_files:
                job = await self.prepare_job(job)

            job = await self.execute_worker(job_id)
        except asyncio.CancelledError:
            if self._delete_requested.is_set():
                logger.info(
                    "DeploymentJobDriver execution interrupted by job deletion.",
                    job_id=job_id,
                )  # type: ignore[call-arg]
                return
            raise
        except Exception as ex:
            logger.error(
                "Execute deployment-backed job failed.",
                job_id=job_id,
                exception=str(ex),
                exc_info=True,
            )  # type: ignore[call-arg]
            job = await self._progress_manager.mark_job_failed(job_id, str(ex))
            await self._snapshot_usage_to_status(job_id)
            self._drop_usage_state(job_id)

        if (
            job.status.state == BatchJobState.FINALIZING
            and not self._delete_requested.is_set()
        ):
            job = await self.finalize_job(job)

        logger.info("Execute deployment-backed job successfully.", job_id=job_id)  # type: ignore[call-arg]
        await self._snapshot_usage_to_status(job_id)
        self._drop_usage_state(job_id)
        return job
