from __future__ import annotations

from types import SimpleNamespace
from typing import Any

from kubernetes import client

import aibrix.batch.job_driver.runtime.k8s_job as k8s_job_runtime_module
from aibrix.batch.client.sources import NoopEndpointSource
from aibrix.batch.job_driver.base import BaseJobDriver
from aibrix.batch.job_driver.runtime import ExternalRuntime
from aibrix.batch.job_entity import BatchJob
from aibrix.batch.manifest import JobManifestRenderer
from aibrix.batch.worker import SingleJobRunner
from aibrix.context.infra import InfrastructureContext


class FakeBatchV1Api:
    """In-process stand-in for ``kubernetes.client.BatchV1Api`` used by the
    local ``KubernetesJob`` backend. A Job stays without a terminal condition
    until ``mark_complete`` flips it to ``Complete``; the fake worker run does
    that once it has written the job's output, so the runtime's poll loop
    observes completion exactly as it would against a real cluster."""

    def __init__(self):
        self.created: list[tuple[str, dict[str, Any]]] = []
        self.deleted: list[tuple[str, str]] = []
        self._conditions: dict[tuple[str, str], list[Any]] = {}

    def create_namespaced_job(self, namespace: str, body: dict[str, Any]):
        name = body["metadata"]["name"]
        self.created.append((namespace, body))
        self._conditions[(namespace, name)] = []

    def patch_namespaced_job(self, name: str, namespace: str, body: dict[str, Any]):
        del body
        if (namespace, name) not in self._conditions:
            raise client.ApiException(status=404)

    def read_namespaced_job_status(self, name: str, namespace: str):
        conditions = self._conditions.get((namespace, name))
        if conditions is None:
            raise client.ApiException(status=404)
        return SimpleNamespace(status=SimpleNamespace(conditions=conditions))

    def delete_namespaced_job(
        self, name: str, namespace: str, propagation_policy: str | None = None
    ):
        del propagation_policy
        if (namespace, name) not in self._conditions:
            raise client.ApiException(status=404)
        self.deleted.append((namespace, name))
        self._conditions.pop((namespace, name))

    def mark_complete(self, namespace: str, name: str):
        self._conditions[(namespace, name)] = [
            SimpleNamespace(type="Complete", status="True", reason="", message="")
        ]


def configure_local_metastore_k8s_job_backend(app, monkeypatch) -> None:
    context = app.state.batch_driver._context
    fake_api = FakeBatchV1Api()
    context.values["k8s_job_batch_v1_api"] = fake_api
    context.values["k8s_job_teardown_calls"] = []

    # K8sJobRuntime reads batch_v1_api off the context, but the slotted
    # InfrastructureContext has no such field, so the runtime falls back to
    # constructing a real BatchV1Api(). Patch the constructor to hand every
    # runtime the fake instead — no cluster is reachable in the local backend.
    monkeypatch.setattr(
        k8s_job_runtime_module.k8s_client,
        "BatchV1Api",
        lambda *args, **kwargs: fake_api,
    )

    # A real render resolves template/profile ConfigMaps that the local
    # backend never applies, so return a minimal suspended-Job manifest.
    def _render(self, session_id, spec, prepared_job=None, **kwargs):
        del self, spec, kwargs
        job_id = getattr(prepared_job, "job_id", None) or session_id
        job_name = f"batch-{job_id[:8]}-worker"
        return {
            "metadata": {"name": job_name, "namespace": "default"},
            "spec": {"suspend": True},
        }

    monkeypatch.setattr(JobManifestRenderer, "render", _render)

    original_on_prepared = k8s_job_runtime_module.K8sJobRuntime.on_prepared

    async def _run_self_hosted_worker(self):
        # Un-suspend first (the real hook), then stand in for the Job's pod:
        # run the worker in worker_mode so it writes the output parts and
        # per-request done markers but leaves finalization to the coordinator,
        # exactly as the real KubernetesJob worker does. The coordinator then
        # aggregates the surviving markers in finalize_job.
        await original_on_prepared(self)
        job_id = self._active_job_id
        job = await self._progress_manager.get_job(job_id)
        worker_job = BatchJob(
            sessionID=job.session_id,
            typeMeta=job.type_meta,
            metadata=job.metadata,
            spec=job.spec,
            status=job.status.model_copy(deep=True),
        )
        worker = BaseJobDriver(
            InfrastructureContext(),
            SingleJobRunner(worker_job),
            ExternalRuntime(
                NoopEndpointSource(
                    delay=context.values.get("endpoint_source_delay_seconds", 0.0)
                )
            ),
            worker_mode=True,
        )
        await worker.execute(job_id)
        handle = self.active_handle
        fake_api.mark_complete(handle.namespace, handle.job_name)

    monkeypatch.setattr(
        k8s_job_runtime_module.K8sJobRuntime,
        "on_prepared",
        _run_self_hosted_worker,
    )

    original_teardown = k8s_job_runtime_module.K8sJobRuntime._teardown

    async def _recording_teardown(self, handle):
        if handle is not None:
            context.values["k8s_job_teardown_calls"].append(
                {"job_id": self._active_job_id, "job_name": handle.job_name}
            )
        return await original_teardown(self, handle)

    monkeypatch.setattr(
        k8s_job_runtime_module.K8sJobRuntime,
        "_teardown",
        _recording_teardown,
    )
