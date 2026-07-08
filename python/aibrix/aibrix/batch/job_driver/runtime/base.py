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
    session_retry_attempts: int = 3
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

    def cancelled(self) -> bool:
        return self._stopped.is_set()

    def _stop_matches_job(self, job_id: Optional[str]) -> bool:
        return job_id is not None and (
            self._stop_job_id == job_id or self._stopped_job_id == job_id
        )

    async def _reconnect(
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

    async def _check_liveness(self, handle: Any) -> None:
        """Best-effort liveness probe for reconnect/recovery and live sessions."""
        del handle
        return None

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
        handle = await self._reconnect(job, job.job_id, runtime_ref)
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
                await asyncio.wait_for(self._check_liveness(handle), timeout=1.0)
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
        status = self._execution_update_status(job)
        status.set_runtime_ref(
            self._get_runtime_key(job),
            execution_ref,
        )
        worker_id = worker_id_generator(execution_ref.owner_ref)
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
        execution_ref = self._load_runtime_ref(job)
        if execution_ref is None:
            return job
        if progress_manager is None:
            raise RuntimeError(
                "Execution tracking is not configured for session(); provide "
                "progress_manager and worker_id_generator when starting a runtime session "
                "for a batch job"
            )
        status = self._execution_update_status(job)
        status.remove_runtime_ref(self._get_runtime_key(job))
        return await progress_manager.update_job_local_status(
            job.job_id,
            worker_id,
            status,
            update_keys={"execution"},
        )

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

    def _should_run_session_liveness_checks(self, handle: Any) -> bool:
        return handle is not None and self.session_liveness_check_interval_s > 0

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
        error_injection = error_injection or JobDriverErrorInjector(job)
        session_worker_id: Optional[str] = None
        handle = None
        max_attempts = self.session_retry_attempts + 1
        self._bind_active_session(job_id, progress_manager)
        error: BaseException | None = None
        body_error: BaseException | None = None
        liveness_error: BaseException | None = None
        liveness_task: asyncio.Task[None] | None = None
        try:
            if self._stop_requested.is_set() or self._stopped.is_set():
                raise asyncio.CancelledError(
                    f"runtime session already stopped for job {job_id}"
                )
            for attempt in range(max_attempts):
                try:
                    phase = (
                        "reconnect"
                        if attempt == 0 and runtimeRef is not None
                        else "provision"
                    )
                    if phase == "reconnect":
                        assert runtimeRef is not None
                        handle = await self._reconnect(job, job_id, runtimeRef)
                        if handle is None:
                            phase = "provision"
                    if handle is None:
                        error_injection.raise_for_breakpoint(
                            BREAKPOINT_RUNTIME_INITIALIZATION
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
                    phase = "wait_ready"
                    await self._wait_ready(
                        handle,
                        wait_mode=RUNTIME_WAIT_MODE_PROVISION
                        if phase == "provision"
                        else RUNTIME_WAIT_MODE_RECONNECT,
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
                    if phase == "wait_ready":
                        should_teardown = self._should_teardown_failed_wait_ready(exc)

                    if should_teardown and handle is not None:
                        try:
                            await self._teardown(handle)
                        except Exception as teardown_exc:
                            logger.warning(
                                "Runtime teardown failed during retry recovery; continuing with retry",
                                job_id=job_id,
                                phase=phase,
                                handle=repr(handle),
                                error=str(teardown_exc),
                            )  # type: ignore[call-arg]
                    elif not should_retry:
                        handle = None
                    if not should_retry:
                        raise
                    handle = None
                    runtimeRef = None
                    await self._sleep_before_session_retry(attempt)

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

            async def _session_liveness_loop() -> None:
                nonlocal liveness_error
                consecutive_failures = 0
                try:
                    while True:
                        await asyncio.sleep(self.session_liveness_check_interval_s)
                        if self._stop_requested.is_set():
                            return
                        try:
                            await self._check_liveness(handle)
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
                            liveness_error = exc
                            current_task.cancel()
                            return
                except asyncio.CancelledError:
                    raise

            if self._should_run_session_liveness_checks(handle):
                liveness_task = asyncio.create_task(_session_liveness_loop())
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
                and liveness_error is not None
            ):
                error = liveness_error
            elif error is None and liveness_error is not None:
                error = liveness_error
            if handle is not None:
                await self._teardown(handle)
            if session_worker_id is not None:
                job = await self._clear_runtime_ref(
                    job,
                    progress_manager=progress_manager,
                    worker_id=session_worker_id,
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
