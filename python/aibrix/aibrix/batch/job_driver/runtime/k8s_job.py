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
"""K8sJobRuntime: run a batch job as a Kubernetes Job, driven in-process.

This is the execution-runtime that lets the metadata service drive k8s-Job
execution in-process, with no separate operator. k8s-Job is a *fire-and-wait*
runtime: the worker runs inside the pod and dispatches the requests itself (so
the endpoint is ``None`` — the control plane sends nothing), and the control
plane just creates the Job, polls it to completion, and tears it down.

Phases (the ``Runtime`` contract, plus the self-hosting hooks the base driver
calls — ``on_prepared`` and ``await_completion``):
  * provision   -> render the Job manifest + create_namespaced_job (suspended)
  * on_prepared -> un-suspend the Job once output files exist (worker starts)
  * await_completion -> POLL read_namespaced_job_status until a terminal
                        condition (Complete / Failed)
  * teardown    -> delete_namespaced_job

The manifest renders ``spec.suspend=True``; un-suspending happens in
``on_prepared`` after the driver's prepare phase. Reuses ``JobManifestRenderer``
verbatim. Registers under the ``KubernetesJob`` provider key.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
from typing import Any, Dict, Optional

from kubernetes import client as k8s_client
from kubernetes.client.rest import ApiException

from aibrix.batch.job_driver.runtime import (
    Completion,
    RuntimeBase,
    register_runtime,
)
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
)
from aibrix.batch.manifest import JobManifestRenderer
from aibrix.context import InfrastructureContext
from aibrix.logger import init_logger

logger = init_logger(__name__)

_TERMINAL_CONDITIONS = ("Complete", "Failed")


@dataclass
class K8sJobHandle:
    """What ``provision`` produces and the later phases consume/clean up."""

    namespace: str
    job_name: str


@dataclass
class K8sJobCompletion:
    """Terminal outcome polled from the k8s Job's conditions."""

    succeeded: bool
    reason: str = ""
    message: str = ""


class K8sJobRuntime(RuntimeBase):
    """Provision a k8s Job per batch job, poll it to completion, tear it down.

    ``connect`` inherits the base NOOP (``Endpoint(source=None)``): the worker
    self-hosts inference in sidecar, so the driver dispatches nothing and
    instead awaits ``wait_until_done``.
    """

    provisions = True

    def __init__(
        self,
        context: InfrastructureContext,
        renderer: Optional[JobManifestRenderer] = None,
        done_timeout_seconds: float = 24 * 3600,
        poll_interval_seconds: float = 5.0,
    ) -> None:
        super().__init__(context)
        self._renderer = renderer or JobManifestRenderer(
            context.template_registry, context.profile_registry
        )
        self._batch_v1_api = (
            getattr(context, "batch_v1_api", None) or k8s_client.BatchV1Api()
        )
        self._done_timeout = done_timeout_seconds
        self._poll_interval = poll_interval_seconds
        self._active_handle: Optional[K8sJobHandle] = None

    @property
    def active_handle(self) -> Optional[K8sJobHandle]:
        """The currently-provisioned job's handle, used by the self-hosting hooks."""
        return self._active_handle

    def _get_runtime_key(self, job: BatchJob) -> str:
        del job
        return "k8s-job"

    def _get_runtime_owner_ref(self, job: BatchJob) -> Optional[str]:
        del job
        if self._active_handle is None:
            return None
        return f"{self._active_handle.namespace}/{self._active_handle.job_name}"

    def _get_runtime_reconnect_payload(
        self,
        job: BatchJob,
    ) -> Optional[Dict[str, Any]]:
        del job
        if self._active_handle is None:
            return None
        return {
            "namespace": self._active_handle.namespace,
            "jobName": self._active_handle.job_name,
        }

    # ── Runtime phases ───────────────────────────────────────────────────

    async def _provision(self, job: BatchJob, job_id: str) -> K8sJobHandle:
        # session_id is a transient pre-job_id correlation token (set at create,
        # not preserved across the JobStore->metastore round-trip), so it is
        # None here. job_id is the durable identifier the deployment runtime
        # already keys off; fall back to it so render's SESSION_ID annotation is
        # always populated. It is only an annotation — the Job name is independent.
        session_id = job.session_id or job_id
        if not session_id:
            raise BatchJobError(
                BatchJobErrorCode.INVALID_DRIVER,
                "k8s Job render requires a session_id or job_id",
            )
        manifest = self._renderer.render(session_id, job.spec, prepared_job=job)
        meta = manifest["metadata"]
        handle = K8sJobHandle(
            namespace=meta.get("namespace") or "default",
            job_name=meta["name"],
        )
        self._active_handle = handle
        await asyncio.to_thread(
            self._batch_v1_api.create_namespaced_job,
            namespace=handle.namespace,
            body=manifest,
        )
        logger.info(  # type: ignore[call-arg]
            "Created k8s Job", job_id=job_id, job_name=handle.job_name
        )
        return handle

    async def _unsuspend(self, handle: K8sJobHandle) -> None:
        """Un-suspend the Job so its pods start — the driver's prepare phase,
        after output files are ready. Under DB-always the worker reads the
        prepared file IDs from the metastore, so only the suspend flag is
        patched here."""
        await asyncio.to_thread(
            self._batch_v1_api.patch_namespaced_job,
            name=handle.job_name,
            namespace=handle.namespace,
            body={"spec": {"suspend": False}},
        )

    async def _wait_until_done(self, handle: K8sJobHandle) -> K8sJobCompletion:
        """Poll the Job's status until a terminal condition appears. Raises
        CancelledError on deletion, TimeoutError on deadline."""
        loop = asyncio.get_running_loop()
        deadline = loop.time() + self._done_timeout
        while True:
            if self._delete_requested.is_set():
                raise asyncio.CancelledError("k8s job deleted")
            try:
                k8s_job = await asyncio.to_thread(
                    self._batch_v1_api.read_namespaced_job_status,
                    name=handle.job_name,
                    namespace=handle.namespace,
                )
            except ApiException as exc:
                if exc.status == 404:
                    # Deleted out from under us — treat as a cancellation.
                    raise asyncio.CancelledError("k8s job not found") from exc
                raise
            cond = _terminal_condition(k8s_job)
            if cond is not None:
                return K8sJobCompletion(
                    succeeded=(cond.type == "Complete"),
                    reason=getattr(cond, "reason", "") or cond.type,
                    message=getattr(cond, "message", "") or "",
                )
            if loop.time() >= deadline:
                raise TimeoutError(f"k8s job {handle.job_name} did not finish in time")
            await asyncio.sleep(self._poll_interval)

    # ── self-hosting driver hooks ────────────────────────────────────────

    async def on_prepared(self) -> None:
        """Un-suspend the Job after the driver prepared output files, so the
        worker pod starts writing into files that now exist."""
        handle = self._active_handle
        if handle is not None:
            await self._unsuspend(handle)

    async def await_completion(self) -> Completion:
        """Poll the provisioned Job to a terminal condition (the base driver's
        run_job phase awaits this instead of dispatching requests)."""
        handle = self._active_handle
        if handle is None:
            raise BatchJobError(
                code=BatchJobErrorCode.INTERNAL_ERROR,
                message="k8s job was not provisioned",
            )
        outcome = await self._wait_until_done(handle)
        return Completion(
            succeeded=outcome.succeeded,
            reason=outcome.reason,
            message=outcome.message,
        )

    async def _teardown(self, handle: Optional[K8sJobHandle]) -> None:
        if handle is None:
            return
        try:
            await asyncio.to_thread(
                self._batch_v1_api.delete_namespaced_job,
                name=handle.job_name,
                namespace=handle.namespace,
                propagation_policy="Foreground",
            )
        except ApiException as exc:
            if exc.status != 404:
                raise


register_runtime(
    "KubernetesJob",
    lambda *, context, **_: K8sJobRuntime(context),
)


def _terminal_condition(k8s_job: Any) -> Optional[Any]:
    """The first Complete/Failed condition with status True, or None. k8s
    guarantees conditions only grow, so a terminal one, once seen, stays."""
    status = getattr(k8s_job, "status", None)
    conditions = getattr(status, "conditions", None) or []
    for cond in conditions:
        if cond.type in _TERMINAL_CONDITIONS and str(cond.status) == "True":
            return cond
    return None
