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
"""Runtime: the single axis of the job lifecycle — *where compute lives*.

A ``Runtime`` provisions compute into a reachable ``Endpoint`` and tears it
down. The job driver owns the phase template (validate / prepare / run_job /
finalize); the runtime only owns provision/wait-ready/connect/teardown. A new
backend (k8s-deployment, k8s-job-sidecar, Lambda, RunPod, OpenAI, local) is a
new ``Runtime`` — never a new driver.

The four sub-phases are explicit (rather than hidden in one ``session`` body) so
that an SSH-launch backend can express "launch vLLM" in ``provision`` and "poll
/health" in ``wait_ready`` as first-class steps instead of an opaque blob.
``RuntimeBase`` implements ``session`` as the four-phase bracket with guaranteed
teardown; subclasses override only the sub-phases they need and inherit a NOOP
for the rest.

NOTE (resource-health watch, deferred): a runtime does NOT continuously monitor
the compute it provisioned. ``wait_ready`` is startup-readiness only. If the
backend dies mid-job, failure surfaces through inference errors (the dispatch
engine) or the job's completion window. A cross-provider ``monitor()`` seam
(k8s watch / EC2 describe / lambda poll) is a documented future extension, not
built here.
"""

from __future__ import annotations

import asyncio
import copy
from contextlib import asynccontextmanager
from dataclasses import dataclass
from datetime import datetime, timezone
from typing import (
    Any,
    AsyncIterator,
    Callable,
    Dict,
    List,
    Optional,
    Protocol,
    runtime_checkable,
)

from aibrix import envs
from aibrix.batch.client import EndpointSource
from aibrix.batch.job_driver.driver import TerminateResult
from aibrix.batch.job_driver.error_injection import (
    BREAKPOINT_AFTER_FINALIZE_SHUTDOWN,
    BREAKPOINT_BEFORE_FINALIZE_SHUTDOWN,
    BREAKPOINT_RUNTIME_INITIALIZATION,
    JobDriverErrorInjector,
)
from aibrix.batch.job_driver.running_jobs import RunningJobs
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobStatus,
    ConditionType,
    JobRuntimeRef,
)
from aibrix.context import InfrastructureContext
from aibrix.logger import init_logger

logger = init_logger(__name__)

RUNTIME_WAIT_MODE_PROVISION = "provision"
RUNTIME_WAIT_MODE_RECONNECT = "reconnect"


class RuntimeOwnershipConflictError(BatchJobError):
    def __init__(
        self,
        *,
        job_id: str,
        runtime_key: str,
        owner_worker_id: Optional[str],
        current_worker_id: str,
        retry_after_s: float = 0.0,
    ) -> None:
        owner_label = owner_worker_id if owner_worker_id is not None else "<unknown>"
        super().__init__(
            code=BatchJobErrorCode.INTERNAL_ERROR,
            message=(
                "Runtime is still owned by another active worker; retry later "
                f"(runtime={runtime_key}, owner_worker_id={owner_label}, "
                f"current_worker_id={current_worker_id}, retry_after_s={retry_after_s:.2f})"
            ),
        )
        self.job_id = job_id
        self.runtime_key = runtime_key
        self.owner_worker_id = owner_worker_id
        self.current_worker_id = current_worker_id
        self.retry_after_s = retry_after_s

    def __deepcopy__(self, memo):
        new_copy = self.__class__(
            job_id=self.job_id,
            runtime_key=self.runtime_key,
            owner_worker_id=self.owner_worker_id,
            current_worker_id=self.current_worker_id,
            retry_after_s=self.retry_after_s,
        )
        memo[id(self)] = new_copy
        return new_copy


class RuntimeDeleteInProgressError(BatchJobError):
    def __init__(
        self,
        *,
        job_id: str,
        runtime_key: str,
        retry_after_s: float = 0.0,
    ) -> None:
        super().__init__(
            code=BatchJobErrorCode.INTERNAL_ERROR,
            message=(
                "Runtime teardown is still in progress; retry later "
                f"(runtime={runtime_key}, retry_after_s={retry_after_s:.2f})"
            ),
        )
        self.job_id = job_id
        self.runtime_key = runtime_key
        self.retry_after_s = retry_after_s

    def __deepcopy__(self, memo):
        new_copy = self.__class__(
            job_id=self.job_id,
            runtime_key=self.runtime_key,
            retry_after_s=self.retry_after_s,
        )
        memo[id(self)] = new_copy
        return new_copy


@dataclass(slots=True)
class Completion:
    """Terminal outcome of a self-hosting runtime (Endpoint(source=None) +
    provisions): the worker ran inference itself and the control plane only
    waited. Returned by ``await_completion`` and consumed by the base driver's
    run_job phase."""

    succeeded: bool
    reason: str = ""
    message: str = ""


@dataclass(slots=True)
class Endpoint:
    """What a provisioned runtime hands back to the driver's run_job phase.

    ``source`` is the reachable endpoint set the dispatch engine sends to.
    ``source is None`` means the control plane has no endpoint — the worker
    self-hosts inference (the sidecar case), so the driver sends nothing and
    instead waits for the worker to finish.

    ``model_name`` lets a single-model backend pin requests to the model it
    actually serves (the deployment case), so an input's ``model`` field can't
    misroute.
    """

    source: Optional[EndpointSource]
    model_name: Optional[str] = None


@runtime_checkable
class Runtime(Protocol):
    """Axis A of the job lifecycle. Implementations are registered by key."""

    #: Whether this runtime stands up compute (False for local/openai/sidecar).
    provisions: bool

    @property
    def context(self) -> InfrastructureContext:
        """Return runtime context"""
        ...

    def cancelled(self) -> bool:
        """True once delete-triggered teardown has fully finished."""
        ...

    def session(
        self,
        job: BatchJob,
        job_id: str,
        *,
        progress_manager: Optional[RunningJobs] = None,
        worker_id_generator: Optional[Callable[[Optional[str]], str]] = None,
        error_injection: Optional[JobDriverErrorInjector] = None,
    ) -> "AsyncRuntimeSession":
        """Async context manager: provision -> yield Endpoint -> teardown."""
        ...

    async def on_prepared(self) -> None:
        """Driver hook fired after output files are prepared, before run_job; a
        self-hosting runtime starts its provisioned worker here. NOOP for most.
        (See RuntimeBase for the default.)"""
        ...

    async def await_completion(self) -> Completion:
        """Block until the provisioned worker finishes, for the self-hosting
        shape (``Endpoint(source=None)`` + ``provisions``). The base driver
        calls this instead of dispatching; non-self-hosting runtimes need not
        implement it."""
        ...

    async def terminate(self, deleted_job: BatchJob) -> TerminateResult:
        """Terminate a job execution, no more job_entity_manager hijacks."""
        ...

    async def cleanup(self, job: BatchJob) -> None:
        """Best-effort cleanup for a recovered job before finalization."""
        ...

    def touch_runtime_ref_heartbeat(
        self,
        job: BatchJob,
        status: BatchJobStatus,
        *,
        worker_id: Optional[str],
        now: Optional[datetime] = None,
    ) -> None:
        """Refresh the active runtime ref heartbeat inside a copied status."""
        ...


class AsyncRuntimeSession(Protocol):
    async def __aenter__(self) -> Endpoint: ...

    async def __aexit__(self, *exc: Any) -> Optional[bool]: ...


class RuntimeBase:
    """Default runtime: implements ``session`` as the four-phase bracket.

    The public methods here are exactly the ``Runtime`` contract (``provisions``
    / ``cancelled`` / ``session`` / ``on_prepared`` / ``await_completion``); the
    ``_``-prefixed ``_provision`` / ``_wait_ready`` / ``_connect`` / ``_teardown``
    are internal template hooks that only ``session`` calls. A subclass overrides
    the hooks it needs (single underscore keeps them overridable) and inherits a
    NOOP for the rest. The default is a no-op runtime that yields no endpoint
    (the sidecar shape).
    """

    provisions: bool = False
    session_retry_attempts: int = 5
    session_retry_base_delay_s: float = 2.0
    session_liveness_check_interval_s: float = 30.0
    session_liveness_failure_threshold: Optional[int] = None

    def __init__(
        self,
        context: InfrastructureContext,
        ready_timeout_seconds: int = 300,
    ) -> None:
        self._context = context
        self._ready_timeout_seconds = ready_timeout_seconds
        configured_liveness_failure_threshold = type(
            self
        ).session_liveness_failure_threshold
        if configured_liveness_failure_threshold is None:
            configured_liveness_failure_threshold = (
                envs.BATCH_SESSION_LIVENESS_FAILURE_THRESHOLD
            )
        self.session_liveness_failure_threshold = max(
            1, int(configured_liveness_failure_threshold)
        )
        self._active_job_id: Optional[str] = None
        self._active_task: Optional[asyncio.Task[None]] = None
        self._active_runtime: Any | None = None
        self._progress_manager: Optional[RunningJobs] = None
        self._stop_requested = asyncio.Event()
        self._stopped = asyncio.Event()
        self._stop_job_id: Optional[str] = None
        self._stopped_job_id: Optional[str] = None

    @property
    def context(self) -> InfrastructureContext:
        """Return runtime context"""
        return self._context

    def cancelled(self) -> bool:
        return self._stopped.is_set()

    def _stop_matches_job(self, job_id: Optional[str]) -> bool:
        return job_id is not None and (
            self._stop_job_id == job_id or self._stopped_job_id == job_id
        )

    async def _load_handle(
        self, job: BatchJob, job_id: str, runtimeRef: JobRuntimeRef
    ) -> Any | None:
        del job, job_id, runtimeRef
        return None

    async def _provision(self, job: BatchJob, job_id: str) -> Any:
        """Create/lease/launch compute. Returns an opaque handle. NOOP default."""
        return None

    async def _wait_ready(
        self, handle: Any, wait_mode: str = RUNTIME_WAIT_MODE_PROVISION
    ) -> None:
        """Poll until runtime startup is usable for this session attempt.

        ``wait_mode`` distinguishes a fresh provision from a reconnect startup
        check, because some runtimes need stronger validation on reconnect than
        a lightweight liveness probe.
        """
        del handle, wait_mode
        return None

    async def _check_liveness(self, handle: Any, reason: str = "unspecified") -> None:
        """Best-effort liveness probe for reconnect/recovery and live sessions."""
        del handle, reason
        return None

    async def _wait_teared_down(
        self,
        handle: Any,
        *,
        job: Optional[BatchJob] = None,
        progress_manager: Optional[RunningJobs] = None,
        worker_id: Optional[str] = None,
    ) -> bool:
        """Poll until runtime confirm to be deleted"""
        loop = asyncio.get_running_loop()
        deadline = loop.time() + float(self._ready_timeout_seconds)
        while True:
            if self._stop_requested.is_set():
                raise asyncio.CancelledError
            try:
                await asyncio.wait_for(
                    self._check_liveness(handle, reason="wait_teared_down"),
                    timeout=1.0,
                )
            except TimeoutError:
                pass
            except Exception as exc:
                if self._is_not_found_error(exc):
                    if (
                        job is not None
                        and progress_manager is not None
                        and worker_id is not None
                    ):
                        job = await self._clear_runtime_ref(
                            job,
                            progress_manager=progress_manager,
                            worker_id=worker_id,
                        )
                    return True
                raise
            if loop.time() >= deadline:
                return False
            await asyncio.sleep(1)

    async def _connect(self, handle: Any) -> Endpoint:
        """Build the reachable endpoint. Default: no endpoint (worker self-hosts)."""
        return Endpoint(source=None)

    async def _teardown(self, handle: Any) -> None:
        """Release the compute. NOOP default."""
        return None

    async def on_prepared(self) -> None:
        """Driver hook fired right after output files are prepared, before
        run_job. A self-hosting runtime starts/releases its provisioned worker
        here (e.g. un-suspend a k8s Job, once the files it writes to exist).
        NOOP default — most runtimes are already serving by ``connect``."""
        return None

    async def await_completion(self) -> Completion:
        """Block until the provisioned worker finishes, for the self-hosting
        shape (``connect`` returned ``Endpoint(source=None)`` and
        ``provisions`` is True). The base driver calls this instead of
        dispatching requests. Only self-hosting runtimes implement it."""
        raise NotImplementedError(
            "await_completion is only valid for self-hosting runtimes "
            "(Endpoint(source=None) + provisions=True)"
        )

    async def terminate(self, deleted_job: BatchJob) -> TerminateResult:
        """Handle job deletion events, no more job_entity_manager hijacks."""
        # Runtime lifecycle state is mutated only by tasks scheduled onto the
        # same event loop as the driver/runtime. Under that single-loop model,
        # `terminate()` and `session()` cannot run concurrently in parallel, so
        # a lock is intentionally not required here. Cross-loop/thread callers
        # are unsupported and must synchronize externally before touching this
        # runtime instance.
        if self._stop_matches_job(deleted_job.job_id) and (
            self._stop_requested.is_set() or self._stopped.is_set()
        ):
            return TerminateResult.ALREADY_REQUESTED

        if self._active_task is None:
            if self._active_job_id not in (None, deleted_job.job_id):
                return TerminateResult.REJECTED
            self._stop_job_id = deleted_job.job_id
            self._stop_requested.set()
            return TerminateResult.ACCEPTED

        if deleted_job.job_id == self._active_job_id:
            if (
                self._progress_manager is not None
                and deleted_job.status.check_condition(ConditionType.CANCELLED)
            ):
                # Persist cancelling before the stop signal is raised so the
                # driver reload path can observe the shared cancellation state.
                await self._progress_manager.update_job_status(
                    deleted_job.job_id,
                    deleted_job.status,
                )
            self._stop_job_id = deleted_job.job_id
            self._stop_requested.set()
            return TerminateResult.ACCEPTED
        return TerminateResult.REJECTED

    def _reset_runtime_state(self) -> None:
        self._active_job_id = None
        self._active_task = None
        self._active_runtime = None
        self._progress_manager = None
        self._stop_requested.clear()
        self._stopped.clear()
        self._stop_job_id = None
        self._stopped_job_id = None

    def _bind_active_session(
        self, job_id: str, progress_manager: Optional[RunningJobs] = None
    ) -> None:
        self._active_job_id = job_id
        self._active_task = asyncio.current_task()
        if not self._stop_matches_job(job_id):
            self._stop_requested.clear()
            self._stopped.clear()
            self._stop_job_id = None
            self._stopped_job_id = None
        self._progress_manager = progress_manager

    def _unbind_active_session(self) -> None:
        self._active_job_id = None
        self._active_task = None
        self._progress_manager = None

    def _get_runtime_key(self, job: BatchJob) -> str:
        """Execution-key to locate execution ref."""
        del job
        return "base"

    def _get_runtime_owner_ref(self, job: BatchJob) -> Optional[str]:
        """Get runtime owner id to identify the runtime provisioning."""
        return self._get_runtime_key(job)

    def _get_runtime_reconnect_payload(
        self,
        job: BatchJob,
    ) -> Optional[Dict[str, Any]]:
        del job
        return None

    def _build_runtime_ref(
        self,
        job: BatchJob,
    ) -> Optional[JobRuntimeRef]:
        if not self.provisions:
            return None

        existing = self._load_runtime_ref(job)
        owner_ref = self._get_runtime_owner_ref(job)
        reconnect_payload = self._get_runtime_reconnect_payload(job)
        if owner_ref is None or reconnect_payload is None:
            raise ValueError("owner_ref and reconnect_payload must be provided")

        now = datetime.now(timezone.utc)
        return JobRuntimeRef(
            driverType=self._get_runtime_key(job),
            attempt=existing.attempt if existing is not None else 1,
            ownerRef=owner_ref,
            reconnectPayload=reconnect_payload,
            connectedAt=existing.connected_at if existing is not None else now,
            heartbeatAt=now,
        )

    def _load_runtime_ref(self, job: BatchJob) -> Optional[JobRuntimeRef]:
        return job.status.get_runtime_ref(self._get_runtime_key(job))

    def _runtime_ref_lease_seconds(self) -> float:
        if self.session_liveness_check_interval_s > 0:
            return max(self.session_liveness_check_interval_s * 2, 1.0)
        return 30.0

    def _runtime_ref_remaining_lease_seconds(
        self,
        execution_ref: JobRuntimeRef,
        *,
        now: Optional[datetime] = None,
    ) -> float:
        heartbeat_at = execution_ref.heartbeat_at
        if heartbeat_at is None:
            return 0.0
        reference_time = now or datetime.now(timezone.utc)
        elapsed = (reference_time - heartbeat_at).total_seconds()
        return max(0.0, self._runtime_ref_lease_seconds() - elapsed)

    def _runtime_ref_owned_by_other_live_worker(
        self,
        execution_ref: Optional[JobRuntimeRef],
        worker_id: Optional[str],
    ) -> tuple[Optional[str], float, bool]:
        if execution_ref is None or worker_id is None:
            return None, 0.0, False
        owner_worker_id = execution_ref.owner_worker_id
        # `None` means a legacy runtime_ref written before owner_worker_id
        # existed. Treat it as "unknown owner": preserve the lease implied by
        # heartbeat_at for backward compatibility, but report no concrete owner
        # identity. This prevents fresh workers from reclaiming a runtime that
        # older metadata services may still be actively using.
        #
        # Empty string is different: it is an explicit "currently unowned"
        # marker, so a new worker may reclaim the runtime immediately.
        if owner_worker_id == "" or owner_worker_id == worker_id:
            return owner_worker_id, 0.0, False

        retry_after_s = self._runtime_ref_remaining_lease_seconds(execution_ref)
        return owner_worker_id, retry_after_s, retry_after_s > 0

    @staticmethod
    def _get_runtime_key_from_execution_ref(execution_ref: JobRuntimeRef) -> str:
        return execution_ref.driver_type

    async def _reload_job_runtime_ref(
        self,
        job: BatchJob,
        *,
        progress_manager: Optional[RunningJobs],
    ) -> tuple[BatchJob, Optional[JobRuntimeRef]]:
        latest_job = (
            await progress_manager.get_job(job.job_id)
            if progress_manager is not None
            else None
        )
        effective_job = latest_job if latest_job is not None else job
        return effective_job, self._load_runtime_ref(effective_job)

    async def _refresh_runtime_ref_heartbeat(
        self,
        job_id: str,
        *,
        progress_manager: Optional[RunningJobs],
        worker_id: Optional[str],
    ) -> None:
        if progress_manager is None or worker_id is None:
            return
        job = await progress_manager.get_job(job_id)
        if job is None:
            return
        execution_ref = self._load_runtime_ref(job)
        if (
            execution_ref is None
            or execution_ref.owner_worker_id != worker_id
            or execution_ref.delete_started()
        ):
            return
        now = datetime.now(timezone.utc)
        heartbeat_at = execution_ref.heartbeat_at
        if heartbeat_at is not None and self.session_liveness_check_interval_s > 0:
            elapsed = (now - heartbeat_at).total_seconds()
            if elapsed < (self.session_liveness_check_interval_s / 2):
                return
        refreshed = copy.deepcopy(execution_ref)
        refreshed.heartbeat_at = now
        status = self._execution_update_status(job)
        status.set_runtime_ref(self._get_runtime_key(job), refreshed)
        await progress_manager.update_job_local_status(
            job.job_id,
            worker_id,
            status,
            update_keys={"execution"},
        )

    async def _should_teardown_runtime_handle(
        self,
        job: BatchJob,
        *,
        progress_manager: Optional[RunningJobs],
        worker_id: Optional[str],
    ) -> bool:
        latest_job, execution_ref = await self._reload_job_runtime_ref(
            job,
            progress_manager=progress_manager,
        )
        owner_worker_id, _, owned_by_other_live_worker = (
            self._runtime_ref_owned_by_other_live_worker(execution_ref, worker_id)
        )
        if owned_by_other_live_worker:
            assert execution_ref is not None
            logger.info(
                "Skipping runtime teardown because ownership moved to another active worker",
                job_id=latest_job.job_id,
                runtime_key=self._get_runtime_key(latest_job),
                current_worker_id=worker_id,
                owner_worker_id=owner_worker_id,
                heartbeat_at=execution_ref.heartbeat_at.isoformat()
                if execution_ref.heartbeat_at is not None
                else None,
            )  # type: ignore[call-arg]
            return False
        return True

    async def _mark_runtime_ref_delete_started(
        self,
        job: BatchJob,
        *,
        progress_manager: Optional[RunningJobs],
        worker_id: Optional[str],
    ) -> BatchJob:
        """Best-effort marker for runtime teardown.

        This helper must not fail. Session cleanup should still teardown the
        provisioned runtime even if execution metadata can no longer be updated
        (for example, the job already left the in-progress bucket).
        """
        if progress_manager is None or worker_id is None:
            return job
        try:
            latest_job, execution_ref = await self._reload_job_runtime_ref(
                job,
                progress_manager=progress_manager,
            )
            if execution_ref is None:
                return latest_job
            # At this stage, we do not check owner_worker_id.
            if execution_ref.delete_started_at is not None:
                return latest_job
            marked_ref = copy.deepcopy(execution_ref)
            now = datetime.now(timezone.utc)
            marked_ref.delete_started_at = now
            marked_ref.heartbeat_at = now
            status = self._execution_update_status(latest_job)
            status.set_runtime_ref(self._get_runtime_key(latest_job), marked_ref)
            return await progress_manager.update_job_local_status(
                latest_job.job_id,
                worker_id,
                status,
                update_keys={"execution"},
            )
        except Exception as exc:
            logger.warning(
                "Failed to mark runtime_ref delete-started; continuing with teardown",
                job_id=job.job_id,
                worker_id=worker_id,
                error=str(exc),
            )  # type: ignore[call-arg]
            return job

    async def _teardown_handle_for_retry_recovery(
        self,
        job: BatchJob,
        *,
        progress_manager: Optional[RunningJobs],
        worker_id: Optional[str],
        handle: Any,
        job_id: str,
        phase: str,
    ) -> BatchJob:
        """Best-effort teardown for startup failures before retrying."""
        try:
            if worker_id is not None:
                job = await self._mark_runtime_ref_delete_started(
                    job,
                    progress_manager=progress_manager,
                    worker_id=worker_id,
                )
            await self._teardown(handle)
        except Exception as teardown_exc:
            logger.warning(
                "Runtime teardown failed during retry recovery; continuing with retry",
                job_id=job_id,
                phase=phase,
                handle=repr(handle),
                error=str(teardown_exc),
            )  # type: ignore[call-arg]
        return job

    async def _teardown_handle_during_session_cleanup(
        self,
        job: BatchJob,
        *,
        progress_manager: Optional[RunningJobs],
        worker_id: Optional[str],
        handle: Any,
        job_id: str,
        error: Optional[BaseException],
        error_injection: JobDriverErrorInjector,
    ) -> tuple[BatchJob, Optional[BaseException]]:
        """Run final session teardown while preserving the primary error."""
        await error_injection.raise_for_breakpoint(
            self._context, job, BREAKPOINT_BEFORE_FINALIZE_SHUTDOWN
        )
        if error_injection.should_skip_session_cleanup_teardown(error):
            return job, error
        try:
            if await self._should_teardown_runtime_handle(
                job,
                progress_manager=progress_manager,
                worker_id=worker_id,
            ):
                if worker_id is not None:
                    job = await self._mark_runtime_ref_delete_started(
                        job,
                        progress_manager=progress_manager,
                        worker_id=worker_id,
                    )
                await self._teardown(handle)
                # Do not clear persisted runtime_ref on session exit.
                #
                # Without an atomic compare-and-swap in `progress_manager`, a
                # late cleanup write from the old worker can erase a newer
                # owner_worker_id already persisted by a restarted worker.
                # Leaving the old runtime_ref in place is safer: ownership and
                # heartbeat-based lease expiry will naturally make stale refs
                # reclaimable without risking overwrite of a newer active owner.
        except BaseException as teardown_exc:
            if error is None:
                error = teardown_exc
            else:
                logger.warning(
                    "Runtime teardown failed during session cleanup; preserving original error",
                    job_id=job_id,
                    handle=repr(handle),
                    original_error=str(error),
                    teardown_error=str(teardown_exc),
                )  # type: ignore[call-arg]
        await error_injection.raise_for_breakpoint(
            self._context, job, BREAKPOINT_AFTER_FINALIZE_SHUTDOWN
        )
        return job, error

    async def cleanup(self, job: BatchJob) -> None:
        """Best-effort cleanup for restart recovery before finalization.

        This is needed for the crash window where the driver has already moved
        the job into ``FINALIZING`` but the runtime teardown has not finished
        yet. If the system restarts in that gap, the recovered driver should
        reconnect to any still-live provisioned runtime and tear it down before
        aggregating outputs. If teardown already completed before restart, this
        method should fall through quickly and let finalization continue.
        """
        if not self.provisions or job.job_id is None:
            return

        runtime_ref = self._load_runtime_ref(job)
        if runtime_ref is None:
            return

        saved_active_job_id = self._active_job_id
        saved_active_task = self._active_task
        saved_active_runtime = self._active_runtime
        saved_progress_manager = self._progress_manager
        saved_stop_requested = self._stop_requested.is_set()
        saved_stopped = self._stopped.is_set()
        saved_stop_job_id = self._stop_job_id
        saved_stopped_job_id = self._stopped_job_id

        # Recovery cleanup does not re-enter a live runtime session; it only
        # reconnects long enough to tear the backend down. Keep the tracked job
        # id for teardown/debugging. Restore any pre-existing live-session state
        # afterward so callers may also use cleanup as a best-effort runtime
        # interruption helper during in-progress execution.
        self._active_job_id = job.job_id
        self._stop_requested.clear()
        self._stopped.clear()
        self._stop_job_id = None
        self._stopped_job_id = None
        handle = await self._load_handle(job, job.job_id, runtime_ref)
        if handle is None:
            self._active_job_id = None
            return

        try:
            try:
                # Recovered FINALIZING cleanup only needs a quick liveness probe.
                # If the runtime was already deleted before restart, avoid the
                # full readiness wait loop and let finalization continue.
                # Keep a hard timeout here because liveness semantics answer
                # "what to check", not "how long it may block"; cleanup must
                # remain a fast recovery path even if a runtime-specific probe
                # stalls on remote control-plane calls.
                await asyncio.wait_for(
                    self._check_liveness(handle, reason="recover_finalizing_cleanup"),
                    timeout=1.0,
                )
            except TimeoutError:
                return
            except Exception as exc:
                if self._is_not_found_error(exc):
                    return
                raise
            await self._teardown(handle)
        finally:
            self._active_job_id = saved_active_job_id
            self._active_task = saved_active_task
            self._active_runtime = saved_active_runtime
            self._progress_manager = saved_progress_manager
            if saved_stop_requested:
                self._stop_requested.set()
            else:
                self._stop_requested.clear()
            if saved_stopped:
                self._stopped.set()
            else:
                self._stopped.clear()
            self._stop_job_id = saved_stop_job_id
            self._stopped_job_id = saved_stopped_job_id

    def touch_runtime_ref_heartbeat(
        self,
        job: BatchJob,
        status: BatchJobStatus,
        *,
        worker_id: Optional[str],
        now: Optional[datetime] = None,
    ) -> None:
        if worker_id is None or status.execution is None:
            return
        runtime_key = self._get_runtime_key(job)
        execution_ref = status.get_runtime_ref(runtime_key)
        if execution_ref is None:
            return
        if execution_ref.owner_worker_id != worker_id:
            return
        if execution_ref.delete_started():
            return
        execution_ref.heartbeat_at = now or datetime.now(timezone.utc)

    async def _persist_runtime_ref(
        self,
        job: BatchJob,
        *,
        progress_manager: Optional[RunningJobs],
        worker_id_generator: Optional[Callable[[Optional[str]], str]],
    ) -> tuple[BatchJob, Optional[str]]:
        # This helper is used after both reconnect and fresh provision. The
        # name is historical: on a fresh provision it persists the current
        # runtime ref, but on reconnect it also serves to rebind execution to
        # the worker-local status slot derived from owner_ref. That local
        # rebinding must happen even when the persisted runtime ref itself is
        # logically unchanged.
        execution_ref = self._build_runtime_ref(job)
        if execution_ref is None:
            return job, None
        if progress_manager is None or worker_id_generator is None:
            raise RuntimeError(
                "Execution tracking is not configured for session(); provide "
                "progress_manager and worker_id_generator when starting a runtime session "
                "for a batch job"
            )
        worker_id = worker_id_generator(execution_ref.owner_ref)
        execution_ref.owner_worker_id = worker_id
        if execution_ref.connected_at is None:
            execution_ref.connected_at = datetime.now(timezone.utc)
        execution_ref.heartbeat_at = datetime.now(timezone.utc)
        execution_ref.delete_started_at = None
        status = self._execution_update_status(job)
        status.set_runtime_ref(
            self._get_runtime_key(job),
            execution_ref,
        )
        # This update include execution only
        return (
            await progress_manager.update_job_local_status(
                job.job_id, worker_id, status, update_keys={"execution"}
            ),
            worker_id,
        )

    def _execution_update_status(self, job: BatchJob) -> BatchJobStatus:
        return BatchJobStatus(
            jobID=job.job_id,
            state=job.status.state,
            createdAt=job.status.created_at,
            execution=copy.deepcopy(job.status.execution),
        )

    async def _clear_runtime_ref(
        self,
        job: BatchJob,
        *,
        progress_manager: Optional[RunningJobs],
        worker_id: str,
    ) -> BatchJob:
        if progress_manager is None:
            return job
        try:
            latest_job, execution_ref = await self._reload_job_runtime_ref(
                job,
                progress_manager=progress_manager,
            )
            if execution_ref is None:
                return latest_job
            if execution_ref.deleted_at is not None:
                return latest_job
            marked_ref = copy.deepcopy(execution_ref)
            now = datetime.now(timezone.utc)
            if marked_ref.delete_started_at is None:
                marked_ref.delete_started_at = now
            marked_ref.deleted_at = now
            marked_ref.heartbeat_at = now
            status = self._execution_update_status(latest_job)
            status.set_runtime_ref(self._get_runtime_key(latest_job), marked_ref)
            return await progress_manager.update_job_local_status(
                latest_job.job_id,
                worker_id,
                status,
                update_keys={"execution"},
            )
        except Exception as exc:
            logger.warning(
                "Failed to mark runtime_ref deleted; continuing recovery",
                job_id=job.job_id,
                worker_id=worker_id,
                error=str(exc),
            )  # type: ignore[call-arg]
            return job

    @staticmethod
    def _opt_enabled(job: BatchJob, opt_key: str) -> bool:
        if not job.spec.opts or opt_key not in job.spec.opts:
            return False
        value = job.spec.opts[opt_key]
        normalized = str(value).strip().lower()
        return normalized not in {"", "0", "false", "no", "off"}

    @staticmethod
    def _is_not_found_error(exc: Exception) -> bool:
        # Runtime backends surface "resource disappeared" through a few shapes:
        # batch-domain errors, HTTP/K8s-style 404s, filesystem not-found, and
        # provider-specific exception names.
        if isinstance(exc, BatchJobError):
            return exc.code == BatchJobErrorCode.RESOURCE_NOTFOUND_ERROR.value
        status = getattr(exc, "status", None)
        if status in (404, "404"):
            return True
        status_code = getattr(exc, "status_code", None)
        if status_code in (404, "404"):
            return True
        if isinstance(exc, FileNotFoundError):
            return True
        name = type(exc).__name__.lower()
        return "notfound" in name or "not_found" in name

    def _should_teardown_failed_wait_ready(self, exc: Exception) -> bool:
        # If readiness proves the resource no longer exists, there is nothing
        # meaningful left to tear down; retry by provisioning a fresh resource.
        return not self._is_not_found_error(exc)

    async def _sleep_before_session_retry(self, attempt: int) -> None:
        await asyncio.sleep(self.session_retry_base_delay_s * (2**attempt))

    async def _sleep_before_session_error_retry(
        self, attempt: int, exc: Exception
    ) -> None:
        delay = self.session_retry_base_delay_s * (2**attempt)
        if isinstance(
            exc, (RuntimeOwnershipConflictError, RuntimeDeleteInProgressError)
        ):
            delay = max(delay, exc.retry_after_s)
        await asyncio.sleep(delay)

    def _should_run_session_liveness_checks(self, handle: Any) -> bool:
        return handle is not None and self.session_liveness_check_interval_s > 0

    async def _session_liveness_loop(
        self,
        *,
        handle: Any,
        job_id: str,
        progress_manager: Optional[RunningJobs],
        worker_id: Optional[str],
        liveness_failure_threshold: int,
        current_task: asyncio.Task[Any],
        error_sink: dict[str, Optional[BaseException]],
    ) -> None:
        consecutive_failures = 0
        try:
            while True:
                await asyncio.sleep(self.session_liveness_check_interval_s)
                if self._stop_requested.is_set():
                    return

                try:
                    await self._check_liveness(handle, reason="session_liveness_loop")
                    consecutive_failures = 0
                except BaseException as exc:
                    if isinstance(exc, asyncio.CancelledError):
                        raise
                    consecutive_failures += 1
                    if consecutive_failures < liveness_failure_threshold:
                        logger.warning(
                            "Runtime liveness check failed; retrying",
                            job_id=job_id,
                            consecutive_failures=consecutive_failures,
                            abort_after_failures=liveness_failure_threshold,
                            error=str(exc),
                        )  # type: ignore[call-arg]
                        continue
                    error_sink["error"] = exc
                    current_task.cancel()
                    return

                try:
                    await self._refresh_runtime_ref_heartbeat(
                        job_id,
                        progress_manager=progress_manager,
                        worker_id=worker_id,
                    )
                except Exception as exc:
                    logger.warning(
                        "Failed to refresh runtime ref heartbeat",
                        job_id=job_id,
                        error=str(exc),
                    )  # type: ignore[call-arg]
        except asyncio.CancelledError:
            raise

    @asynccontextmanager
    async def session(
        self,
        job: BatchJob,
        job_id: str,
        *,
        progress_manager: Optional[RunningJobs] = None,
        worker_id_generator: Optional[Callable[[Optional[str]], str]] = None,
        error_injection: Optional[JobDriverErrorInjector] = None,
    ) -> AsyncIterator[Endpoint]:
        runtimeRef = self._load_runtime_ref(job)
        error_injection = error_injection or JobDriverErrorInjector(
            job,
            progress_manager=progress_manager,
        )
        session_worker_id: Optional[str] = None
        handle = None
        max_attempts = self.session_retry_attempts + 1
        self._bind_active_session(job_id, progress_manager)
        error: BaseException | None = None
        body_error: BaseException | None = None
        liveness_state: dict[str, Optional[BaseException]] = {"error": None}
        liveness_task: asyncio.Task[None] | None = None
        try:
            if self._stop_requested.is_set() or self._stopped.is_set():
                raise asyncio.CancelledError(
                    f"runtime session already stopped for job {job_id}"
                )
            for attempt in range(max_attempts):
                try:
                    if attempt > 0:
                        job, runtimeRef = await self._reload_job_runtime_ref(
                            job,
                            progress_manager=progress_manager,
                        )
                    phase = "reconnect" if runtimeRef is not None else "provision"
                    if phase == "reconnect":
                        assert runtimeRef is not None
                        if (
                            session_worker_id is None
                            and worker_id_generator is not None
                        ):
                            session_worker_id = worker_id_generator(
                                runtimeRef.owner_ref
                            )
                        if not runtimeRef.deleted():
                            handle = await self._load_handle(job, job_id, runtimeRef)
                        if handle is None or runtimeRef.delete_started():
                            # A previous worker has already started tearing this
                            # runtime down. Do not trust or reuse that runtime_ref
                            # until the replacement worker verifies the old runtime
                            # is actually gone; otherwise a slow delete can race a
                            # new provision against the same backend identity.
                            if handle is None or await self._wait_teared_down(
                                handle,
                                job=job,
                                progress_manager=progress_manager,
                                worker_id=session_worker_id,
                            ):
                                runtimeRef = None
                                handle = None
                                phase = "provision"
                            else:
                                raise RuntimeDeleteInProgressError(
                                    job_id=job_id,
                                    runtime_key=self._get_runtime_key_from_execution_ref(
                                        runtimeRef
                                    ),
                                    retry_after_s=1.0,
                                )
                    # Check for handle timeout
                    if handle is not None:
                        assert runtimeRef is not None
                        if worker_id_generator is None:
                            raise RuntimeError(
                                "Execution tracking is not configured for session(); provide "
                                "worker_id_generator when starting a runtime session "
                                "for a batch job"
                            )
                        session_worker_id = worker_id_generator(runtimeRef.owner_ref)
                        owner_worker_id, retry_after_s, owned_by_other_live_worker = (
                            self._runtime_ref_owned_by_other_live_worker(
                                runtimeRef,
                                session_worker_id,
                            )
                        )
                        if owned_by_other_live_worker:
                            # We do not wait for timeout but leave session retry to handle.
                            # So other changes (e.g., delete flag) may shortcut the wait.
                            raise RuntimeOwnershipConflictError(
                                job_id=job_id,
                                runtime_key=self._get_runtime_key_from_execution_ref(
                                    runtimeRef
                                ),
                                owner_worker_id=owner_worker_id,
                                current_worker_id=session_worker_id,
                                retry_after_s=retry_after_s,
                            )
                    # Provision
                    if handle is None:
                        await error_injection.raise_for_breakpoint(
                            self._context,
                            job,
                            BREAKPOINT_RUNTIME_INITIALIZATION,
                        )
                        handle = await self._provision(job, job_id)
                    # Run after both reconnect and provision. On provision this
                    # persists the active runtime ref; on reconnect it refreshes
                    # the worker-local execution binding so the recovered/live
                    # session continues updating the correct owner_ref slot.
                    job, session_worker_id = await self._persist_runtime_ref(
                        job,
                        progress_manager=progress_manager,
                        worker_id_generator=worker_id_generator,
                    )
                    wait_mode = (
                        RUNTIME_WAIT_MODE_PROVISION
                        if phase == "provision"
                        else RUNTIME_WAIT_MODE_RECONNECT
                    )
                    phase = "wait_ready"
                    await self._wait_ready(
                        handle,
                        wait_mode=wait_mode,
                    )
                    # Only yield a connected endpoint after both startup phases
                    # succeed within the same attempt.
                    break
                except Exception as exc:
                    # Any startup failure, including readiness checks run as
                    # part of reconnect/provision, consumes an attempt and
                    # falls back to provisioning fresh compute on retry.
                    should_retry = attempt + 1 < max_attempts
                    should_teardown = handle is not None
                    if isinstance(
                        exc,
                        (RuntimeOwnershipConflictError, RuntimeDeleteInProgressError),
                    ):
                        # Reconnect found a live runtime that is still owned by
                        # another worker or is already being deleted. Do not
                        # tear that runtime down from the replacement worker;
                        # wait/retry and either reclaim it or observe that the
                        # original delete completed.
                        should_teardown = False
                    if phase == "wait_ready":
                        should_teardown = self._should_teardown_failed_wait_ready(exc)
                        if (
                            not should_teardown
                            and progress_manager is not None
                            and session_worker_id is not None
                        ):
                            job = await self._clear_runtime_ref(
                                job,
                                progress_manager=progress_manager,
                                worker_id=session_worker_id,
                            )

                    if should_teardown and handle is not None:
                        job = await self._teardown_handle_for_retry_recovery(
                            job,
                            progress_manager=progress_manager,
                            worker_id=session_worker_id,
                            handle=handle,
                            job_id=job_id,
                            phase=phase,
                        )
                    elif not should_retry:
                        handle = None
                    if not should_retry:
                        raise
                    handle = None
                    runtimeRef = None
                    await self._sleep_before_session_error_retry(attempt, exc)

            endpoint = await self._connect(handle)
            current_task = asyncio.current_task()
            if current_task is None:
                raise RuntimeError("session() requires a running asyncio task")
            configured_liveness_failure_threshold = (
                self.session_liveness_failure_threshold
            )
            if configured_liveness_failure_threshold is None:
                raise RuntimeError(
                    "session_liveness_failure_threshold must be configured"
                )
            liveness_failure_threshold = max(1, configured_liveness_failure_threshold)

            if self._should_run_session_liveness_checks(handle):
                liveness_task = asyncio.create_task(
                    self._session_liveness_loop(
                        handle=handle,
                        job_id=job_id,
                        progress_manager=progress_manager,
                        worker_id=session_worker_id,
                        liveness_failure_threshold=liveness_failure_threshold,
                        current_task=current_task,
                        error_sink=liveness_state,
                    )
                )
            try:
                yield endpoint
            except BaseException as ex:
                body_error = ex
                raise
        except BaseException as ex:
            # Capture any exception that happens during the session lifecycle.
            # Will raise it and be propagated to the caller after teardown.
            error = ex
        finally:
            if liveness_task is not None:
                liveness_task.cancel()
                try:
                    await liveness_task
                except asyncio.CancelledError:
                    pass
            if (
                isinstance(body_error, asyncio.CancelledError)
                and liveness_state["error"] is not None
            ):
                error = liveness_state["error"]
            elif error is None and liveness_state["error"] is not None:
                error = liveness_state["error"]
            if handle is not None:
                job, error = await self._teardown_handle_during_session_cleanup(
                    job,
                    progress_manager=progress_manager,
                    worker_id=session_worker_id,
                    handle=handle,
                    job_id=job_id,
                    error=error,
                    error_injection=error_injection,
                )
            if self._stop_requested.is_set():
                self._stopped_job_id = job_id
                self._stopped.set()
            self._unbind_active_session()

        if error is not None:
            raise error


class ExternalRuntime(RuntimeBase):
    """No provisioning: the endpoint already exists (an injected source).

    Used by the standalone/in-process path and by OpenAI-style direct-API
    backends — anything where the engine endpoint is known up front.
    """

    provisions = False

    def __init__(
        self,
        source: Optional[EndpointSource],
        context: Optional[InfrastructureContext] = None,
    ) -> None:
        super().__init__(context or InfrastructureContext())
        self._source = source

    async def _connect(self, handle: Any) -> Endpoint:
        return Endpoint(source=self._source)


class NoopRuntime(RuntimeBase):
    """No control-plane endpoint at all: the worker provisions and runs
    inference itself (the colocated sidecar / distributed case). ``connect``
    inherits the base NOOP that yields ``Endpoint(source=None)``."""

    provisions = False

    def __init__(self, context: Optional[InfrastructureContext] = None) -> None:
        super().__init__(context or InfrastructureContext())


# --- Registry (downstream extension point) -------------------------------
#
# Each runtime backend registers under a string key. Upstream registers the
# OSS backends; downstream registers its own (e.g. "k8s-deployment-cr") in its
# own module — no edit to an upstream if/elif or enum, so rebasing upstream
# does not conflict. Selection is by configured string -> registry lookup.

_RUNTIME_FACTORIES: Dict[str, Callable[..., Runtime]] = {}


def register_runtime(key: str, factory: Callable[..., Runtime]) -> None:
    """Register a runtime factory under ``key``. Idempotent overwrite allowed
    so a downstream module can shadow an upstream default if it must."""
    _RUNTIME_FACTORIES[key] = factory


def create_runtime(key: str, *args: Any, **kwargs: Any) -> Runtime:
    """Build a runtime by key. Raises KeyError with the known keys on miss."""
    try:
        factory = _RUNTIME_FACTORIES[key]
    except KeyError:
        raise KeyError(
            f"unknown runtime '{key}'; registered: {registered_runtimes()}"
        ) from None
    return factory(*args, **kwargs)


def registered_runtimes() -> List[str]:
    return sorted(_RUNTIME_FACTORIES)


# OSS built-in runtimes that need no extra dependencies. Provisioning backends
# (Kubernetes / KubernetesJob / LambdaCloud / RunPod) register from their own
# modules, which pull in kubernetes / cloud SDKs. Keys match RuntimeTarget.
# Factories take a uniform keyword bag and pick what they need.
register_runtime(
    "External",
    lambda *, endpoint_source=None, context=None, **_: ExternalRuntime(
        endpoint_source,
        context=context,
    ),
)
register_runtime("noop", lambda *, context=None, **_: NoopRuntime(context=context))
