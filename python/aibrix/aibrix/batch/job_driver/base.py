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
"""BaseJobDriver: the common job driver.

One lifecycle template, parameterized by a single seam — a ``Runtime``
The explicit phases are:

    validate -> [provision -> wait_ready -> connect] -> prepare -> run_job
             -> finalize -> [teardown]

The bracketed phases belong to the Runtime's ``session``; everything else —
``prepare`` / ``run_job`` / ``finalize`` plus the per-request bookkeeping
(resume, usage accounting, output writing, status sync) — lives here in
``BaseJobDriver`` and is shared by every backend.

This one template also covers the self-hosting shape (e.g. a k8s Job whose
worker dispatches inside its own compute) without a driver subclass. Two seams
make that work: when ``connect`` returns ``Endpoint(source=None)`` on a
provisioning runtime, ``run_job`` waits for the runtime to finish instead of
dispatching requests itself; and ``on_prepared`` lets the runtime start its
worker only after the output files it writes to exist.
"""

import asyncio
import contextlib
import os
import uuid
from datetime import datetime, timezone
from math import isfinite
from typing import Any, Dict, Iterable, Optional, Set, Tuple

import aibrix.batch.constant as constant
import aibrix.batch.storage as storage
from aibrix.batch.client import (
    DispatchEngine,
    DispatchStats,
    DispatchStatsSnapshot,
    InferenceError,
    InferenceRequest,
    RetryConfig,
)
from aibrix.batch.job_driver.driver import TerminateResult
from aibrix.batch.job_driver.running_jobs import RunningJobs
from aibrix.batch.job_driver.runtime import Endpoint, NoopRuntime, Runtime
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobEndpoint,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobSpec,
    BatchJobState,
    BatchUsage,
    Condition,
    ConditionStatus,
    ConditionType,
    RequestCountStats,
)
from aibrix.logger import init_logger

logger = init_logger(__name__)

_ADAPTIVE_MAX_FACTOR_ENV = "AIBRIX_BATCH_ADAPTIVE_MAX_FACTOR"
_TELEMETRY_INTERVAL_ENV = "AIBRIX_BATCH_TELEMETRY_INTERVAL_SECONDS"
_INFERENCE_MAX_RETRIES_ENV = "AIBRIX_BATCH_INFERENCE_MAX_RETRIES"
_NO_ENDPOINT_MAX_RETRIES_ENV = "AIBRIX_BATCH_NO_ENDPOINT_MAX_RETRIES"
_RETRY_BASE_DELAY_ENV = "AIBRIX_BATCH_RETRY_BASE_DELAY_SECONDS"
_RETRY_MAX_DELAY_ENV = "AIBRIX_BATCH_RETRY_MAX_DELAY_SECONDS"
_DEFAULT_ADAPTIVE_MAX_FACTOR = 8.0
_DEFAULT_TELEMETRY_INTERVAL_SECONDS = 5.0
_DEFAULT_INFERENCE_MAX_RETRIES = 120
_DEFAULT_NO_ENDPOINT_MAX_RETRIES = 120
_DEFAULT_RETRY_BASE_DELAY_SECONDS = 0.5
_DEFAULT_RETRY_MAX_DELAY_SECONDS = 5.0
_DONE_RECONCILE_CHUNK_SIZE = 256


def _adaptive_max_factor() -> float:
    return max(
        _float_env(_ADAPTIVE_MAX_FACTOR_ENV, _DEFAULT_ADAPTIVE_MAX_FACTOR),
        1.0,
    )


def _telemetry_interval_seconds() -> float:
    return max(
        _float_env(_TELEMETRY_INTERVAL_ENV, _DEFAULT_TELEMETRY_INTERVAL_SECONDS),
        0.0,
    )


def _no_endpoint_max_retries() -> int:
    return max(
        _int_env(
            _NO_ENDPOINT_MAX_RETRIES_ENV,
            _DEFAULT_NO_ENDPOINT_MAX_RETRIES,
        ),
        0,
    )


def _inference_max_retries() -> int:
    return max(
        _int_env(
            _INFERENCE_MAX_RETRIES_ENV,
            _DEFAULT_INFERENCE_MAX_RETRIES,
        ),
        0,
    )


def _retry_base_delay_seconds() -> float:
    return max(
        _float_env(_RETRY_BASE_DELAY_ENV, _DEFAULT_RETRY_BASE_DELAY_SECONDS),
        0.0,
    )


def _retry_max_delay_seconds() -> float:
    return max(
        _float_env(_RETRY_MAX_DELAY_ENV, _DEFAULT_RETRY_MAX_DELAY_SECONDS),
        0.0,
    )


def _float_env(name: str, default: float) -> float:
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    try:
        value = float(raw)
    except ValueError:
        logger.warning(
            "Invalid float environment value; using default",
            name=name,
            value=raw,
            default=default,
        )  # type: ignore[call-arg]
        return default
    if not isfinite(value):
        logger.warning(
            "Invalid float environment value; using default",
            name=name,
            value=raw,
            default=default,
        )  # type: ignore[call-arg]
        return default
    return value


def _int_env(name: str, default: int) -> int:
    raw = os.environ.get(name)
    if raw is None or raw == "":
        return default
    try:
        value = int(raw)
    except ValueError:
        logger.warning(
            "Invalid int environment value; using default",
            name=name,
            value=raw,
            default=default,
        )  # type: ignore[call-arg]
        return default
    return value


def _round_optional(value: Optional[float]) -> Optional[float]:
    if value is None:
        return None
    return round(value, 6)


class BaseJobDriver:
    def __init__(
        self,
        progress_manager: RunningJobs,
        runtime: Optional[Runtime] = None,
        job: Optional[BatchJob] = None,
        *,
        reraise_on_failure: Optional[bool] = None,
        worker_mode: bool = False,
        default_failure_code: Optional[BatchJobErrorCode] = None,
    ) -> None:
        self._progress_manager = progress_manager
        # The single seam. A provisioning runtime (Kubernetes / KubernetesJob /
        # cloud) is selected per-job; ``NoopRuntime`` is the prepare/finalize-
        # only default.
        self._runtime: Runtime = runtime or NoopRuntime()
        self._job_id = job.job_id if job is not None else None
        self._worker_token = uuid.uuid4().hex[:8]
        self._worker_id = self._worker_token

        # Three behaviors differ across backends and are derived from whether
        # the runtime provisions compute (a provisioning runtime owns the whole
        # run), with explicit overrides for tests / special cases:
        #   * reraise: the scheduler-driven inline path (no provisioning)
        #     re-raises so the scheduler loop observes the failure; a
        #     provisioning backend does not.
        #   * aggregate_always: a provisioning backend that owns the run always
        #     aggregates; the inline path defers aggregation to the coordinator
        #     when it did not create the temp files (a parallel worker).
        #   * default_failure_code: an unclassified failure is infra/internal
        #     for a provisioning backend, an inference error for inline dispatch.
        provisions = self._runtime.provisions
        self._reraise_on_failure: bool = (
            (not provisions) if reraise_on_failure is None else reraise_on_failure
        )
        self._worker_mode: bool = worker_mode
        self._default_failure_code: BatchJobErrorCode = default_failure_code or (
            BatchJobErrorCode.INTERNAL_ERROR
            if provisions
            else BatchJobErrorCode.INFERENCE_FAILED
        )

        # Engine is (re)built from the runtime's endpoint during a session; it
        # may stay None when the driver only prepares/finalizes (no inference).
        self._engine: Optional[DispatchEngine] = None
        self._active_model_name: Optional[str] = None

        # Driver-local execution state. Each driver instance only tracks one
        # job at a time, so usage/count accumulators no longer need per-job
        # maps after the job-bound driver refactor.
        self._state_job_id: Optional[str] = self._job_id
        self._usage: Optional[BatchUsage] = None
        self._usage_counted_ids: Set[str] = set()
        self._request_counts: Optional[RequestCountStats] = None
        self._launched_request_ids: Set[int] = set()
        self._usage_lock = asyncio.Lock()

    # ── protocol methods ──────────────────────────────────────────

    async def validate_job(self, job: BatchJob):
        logger.debug(
            "Validate local driver",
            job_id=job.job_id,
            input_file_id=job.spec.input_file_id,
        )  # type: ignore[call-arg]
        if not self._job_authentication(job):
            raise BatchJobError(
                code=BatchJobErrorCode.AUTHENTICATION_ERROR,
                message="authentication error",
            )

        _, exists = await storage.read_job_input_info(job)
        if not exists:
            raise BatchJobError(
                code=BatchJobErrorCode.INVALID_INPUT_FILE,
                message="input file not found",
            )

        total, validation_error = await storage.validate_job_input_file(
            job.spec.input_file_id,
            (
                job.spec.endpoint.value
                if isinstance(job.spec.endpoint, BatchJobEndpoint)
                else str(job.spec.endpoint)
            ),
        )
        if validation_error:
            raise BatchJobError(
                code=BatchJobErrorCode.VALIDATION_ERROR,
                message=validation_error,
            )
        if total == 0:
            raise BatchJobError(
                code=BatchJobErrorCode.EMPTY_INPUT_FILE,
                message="input file is empty",
            )
        validated_status = job.status.model_copy(deep=True)
        validated_status.request_counts.total = total
        await self._progress_manager.mark_job_validated(job.job_id, validated_status)

    async def execute(self, job_id) -> None:
        """Run the full lifecycle: session(provision/teardown) wraps
        prepare -> run_job -> finalize. Behavior matches the legacy inline
        driver; backends that need a different shape override this method.
        """
        job_id = self._resolve_job_id(job_id)
        job = await self._progress_manager.get_job(job_id)
        if job is None:
            self._log_missing_job(job_id)
            return

        if self._should_resume_finalizing_job(job):
            job = await self._resume_finalizing_job(job)
            self._log_completed(job)
            return

        if job.status.state == BatchJobState.IN_PROGRESS:
            self._reset_execution_state(job)

        try:
            job = await self._execute_job_in_runtime(self._runtime, job)
        except asyncio.CancelledError:
            # A provisioning runtime cancels the run when its job is deleted;
            # teardown already ran via the session. Conclude the accepted cancel
            # here only after reloading the shared job state.
            if self._runtime.cancelled():
                latest = await self._progress_manager.get_job(job_id)
                if latest is None:
                    return
                await self._finish_stopped_job(latest)
                self._log_cancelled(job_id)
                return
            raise
        except Exception as e:
            # Guard mark_job_failed: if it raises (e.g. the job is no
            # longer in_progress) the exception must not escape and tear
            # down the loop, stranding all future jobs.
            try:
                job = await self._progress_manager.mark_job_failed(
                    job_id,
                    self._make_failure_error(e),
                )
                state = job.status.state.value
            except Exception as me:
                state = "unknown"
                logger.error(
                    "Failed to mark job failed",
                    job_id=job_id,
                    error=str(me),
                )  # type: ignore[call-arg]
            logger.error(
                "Failed to execute job",
                job_id=job_id,
                status=state,
                error=str(e),
            )  # type: ignore[call-arg]

        # [TODO][NEXT] This is legacy error handling. It is confusing and may be suitable for worker only.
        if self._reraise_on_failure and job.status.failed:
            failed_condition = job.status.get_condition(ConditionType.FAILED)
            if failed_condition is None:
                raise RuntimeError("Job failed but no failure condition was set")
            raise RuntimeError(
                failed_condition.message or "Job failed with an unspecified error"
            )

    async def terminate(self, deleted_job: BatchJob) -> TerminateResult:
        if (
            deleted_job.status.finished
            or deleted_job.status.state == BatchJobState.FINALIZING
        ):
            return TerminateResult.REJECTED

        result = TerminateResult.ACCEPTED
        if self._runtime is not None:
            result = await self._runtime.terminate(deleted_job)

        # `deleted_job` carries the manager's optimistic cancelling status so
        # the live path can observe cancel intent early and, if accepted,
        # be persisted by the runtime directly. Once the runtime rejects
        # termination, that conflicts with the optimistic assumption and
        # `deleted_job` becomes obsolete.
        # and must not be reinterpreted as acceptance from the driver layer.
        return result

    # ── overridable options ──────────────────────────────────────────

    def _should_resume_finalizing_job(self, job: BatchJob) -> bool:
        """Return True when a recovered job should jump straight to finalizing.

        Some runtimes persist enough metadata that, after restart, replaying
        worker execution is wrong and the driver should continue from the
        finalization phase instead.
        """
        return (
            job.status.state == BatchJobState.FINALIZING
            and self._can_finalize_job(job)
            and self._should_finalize()
        )

    def _can_finalize_job(self, job: BatchJob) -> bool:
        """Return True when finalize is safe to run for the current job state.

        This exists because runtime creation may fail before temp files or
        other finalize prerequisites are ready. Subclasses can narrow or relax
        the gate when their runtime lifecycle has different requirements.
        """
        return (
            job.status.temp_output_file_id is not None
            and job.status.temp_error_file_id is not None
        )

    async def _get_next_pass_start(self, job: BatchJob) -> int:
        if self._should_stop_before_proceed(job):
            return -1
        request_counts = job.status.request_counts
        if request_counts.total <= 0:
            return 0
        elif request_counts.completed + request_counts.failed >= request_counts.total:
            return -1

        for start in range(0, request_counts.total, _DONE_RECONCILE_CHUNK_SIZE):
            request_ids = range(
                start,
                min(start + _DONE_RECONCILE_CHUNK_SIZE, request_counts.total),
            )
            done_flags = await asyncio.gather(
                *[
                    storage.is_request_done(job, request_id)
                    for request_id in request_ids
                ]
            )
            for request_id, done in zip(request_ids, done_flags):
                if not done:
                    return request_id
        return -1

    # ── failure / error / log helpers ──────────────────────────────────────────

    @staticmethod
    def _ensure_batch_job_error(
        error: Exception,
        default_code: BatchJobErrorCode = BatchJobErrorCode.INTERNAL_ERROR,
    ) -> BatchJobError:
        if isinstance(error, BatchJobError):
            return error
        return BatchJobError(code=default_code, message=str(error))

    @staticmethod
    def _error_code_from_reason(reason: Optional[str]) -> BatchJobErrorCode:
        if reason is None:
            return BatchJobErrorCode.INTERNAL_ERROR
        try:
            return BatchJobErrorCode(reason)
        except ValueError:
            return BatchJobErrorCode.INTERNAL_ERROR

    def _make_failure_error(self, error: Exception) -> BatchJobError:
        """Classify an uncaught run_job() failure. Preserve an already-classified
        BatchJobError (e.g. RESOURCE_CREATION_ERROR); default an unclassified one
        to the backend's default code (inference for inline dispatch, internal
        for a provisioning backend)."""
        return self._ensure_batch_job_error(
            error, default_code=self._default_failure_code
        )

    def _should_finalize(self) -> bool:
        """Whether this driver instance is responsible for finalization.

        Worker-style execution can defer finalization to a coordinating driver,
        so subclasses override this when ownership rules differ from the base
        executor-role model.
        """
        return not self._worker_mode

    def _resolve_job_id(self, job_id: Optional[str]) -> str:
        if job_id is None:
            raise ValueError("job_id is required")
        return job_id

    def _assign_worker_id(self, runtime_key: Optional[str]) -> str:
        self._worker_id = (
            f"{runtime_key}-{self._worker_token}"
            if runtime_key is not None
            else self._worker_token
        )
        return self._worker_id

    def _log_missing_job(self, job_id: str) -> None:
        logger.warning("Job not found", job_id=job_id)  # type: ignore[call-arg]

    def _log_cancelled(self, job_id: str) -> None:
        logger.info("Execution interrupted by job deletion", job_id=job_id)  # type: ignore[call-arg]

    def _log_failed(self, job_id: str, error: BatchJobError) -> None:
        logger.error(
            "Failed to execute job",
            job_id=job_id,
            error_code=error.code,
            error=error.message,
        )  # type: ignore[call-arg]

    def _log_completed(self, job: BatchJob) -> None:
        logger.debug(
            "Completed job",
            job_id=job.job_id,
            status=job.status.state.value,
        )  # type: ignore[call-arg]

    def _should_stop_before_proceed(self, job: BatchJob, reload: bool = False) -> bool:
        """Check for interruption signal"""
        return (
            job.status.state == BatchJobState.CANCELLING
            or job.status.cancelled
            or job.status.finished
        )

    async def _is_job_stopped(self, job_id: str) -> Tuple[BatchJob, bool]:
        """Check for interruption signal using reloaded job"""
        job = await self._progress_manager.get_job(job_id)
        if job is None:
            raise RuntimeError(f"Job metadata missing for {job_id}")

        return job, self._should_stop_before_proceed(job)

    async def _finish_stopped_job(
        self, job: BatchJob, require_finalize: bool = False
    ) -> BatchJob:
        """Conclude a cancelled job before request execution if this driver owns it."""
        if job.status.finished:
            return job

        if (
            (require_finalize or job.status.is_finalizing_required())
            and self._can_finalize_job(job)
            and self._should_finalize()
        ):
            logger.debug("Finalizing job", job_id=job.job_id)  # type: ignore[call-arg]
            return await self.finalize_job(job)

        # If finalizing is not needed, we still need to fill state gap between
        # in-progross and finalized manually
        # Keep using the finalizing snapshot returned by the progress manager.
        # Passing the pre-finalizing `job` object into mark_job_done() can race
        # with manager/store updates and lose newer cancel conditions or counts.
        job = await self._progress_manager.mark_job_finalizing(job.job_id)
        synced = await self._progress_manager.mark_job_done(job)
        self._log_completed(synced)
        return synced

    def _mark_job_completed_when_fully_processed(self, job: BatchJob) -> BatchJob:
        """Add the completed condition once all requests are accounted for.

        Runtime-backed drivers share the same request-count semantics as the
        pure local driver, so the condition synthesis lives here instead of in
        only one execute_job implementation.
        """
        if (
            job.status.request_counts.total > 0
            and job.status.request_counts.completed + job.status.request_counts.failed
            == job.status.request_counts.total
            and job.status.condition is None
        ):
            job.status.add_condition(
                Condition(
                    type=ConditionType.COMPLETED,
                    status=ConditionStatus.TRUE,
                    lastTransitionTime=datetime.now(timezone.utc),
                )
            )
        return job

    async def _after_execute_worker(self, job: BatchJob) -> BatchJob:
        return job

    async def _prepare_execution_job(self, job: BatchJob) -> BatchJob:
        """Prepare the job immediately before worker execution starts.

        The default skips preparation when temp output files already exist, but
        runtime-backed drivers can override this if they need extra runtime
        bootstrap or resume-specific setup before worker execution.
        """
        has_temp_files = (
            job.status.temp_output_file_id is not None
            and job.status.temp_error_file_id is not None
        )
        if not has_temp_files:
            logger.debug("Temp files not created, creating...", job_id=job.job_id)  # type: ignore[call-arg]
            job = await self.prepare_job(job)
            # Release/start a self-hosting provisioned worker now that
            # the files it writes to exist (e.g. un-suspend a k8s Job).
            # NOOP for runtimes already serving by ``connect``.
            await self._runtime.on_prepared()

        return job

    def _reset_execution_state(self, job: BatchJob) -> None:
        self._ensure_local_job_state(job.job_id)
        self._request_counts = None
        self._usage = None
        self._usage_counted_ids.clear()
        self._launched_request_ids = set()

    # ── usage accounting ─────────────────────────────────────────────────

    def _ensure_local_job_state(self, job_id: str) -> None:
        """Ensure local states get reset if JobDriver get reused."""
        if self._state_job_id == job_id:
            return
        if self._state_job_id is not None:
            # This path should not be touched if _drop_usage_state is called at the end of Job.
            # It is here for safety.
            self._drop_usage_state(self._state_job_id)
        self._state_job_id = job_id

    def _get_local_request_counts(
        self,
        job_id: str,
    ) -> RequestCountStats:
        """Return lazy initialized local RequestCountStats
        Note that local RequestCountStats.total is also used as a fallback
        "largest request id seen + 1" tracker. Validation should pre-seed the
        authoritative total, but if it did not, the driver still needs a moving
        upper bound so round-based worker execution can stop once all seen
        requests are drained instead of looping forever.
        """
        self._ensure_local_job_state(job_id)
        if self._request_counts is None:
            self._request_counts = RequestCountStats(total=0)
        return self._request_counts

    def _accumulate_dispatched_request(self, job_id: str, request_id: int) -> None:
        """Track dispatched requests and the largest request id seen so far."""
        request_counts = self._get_local_request_counts(job_id)
        if request_id in self._launched_request_ids:
            return
        self._launched_request_ids.add(request_id)
        request_counts.launched += 1
        if request_id >= request_counts.total:
            # When validation did not fix total up front, treat total as the
            # highest dispatched request id + 1. The progress tracker uses this
            # fallback bound to decide whether another execution round is still
            # needed for requests that have been seen but not yet completed.
            request_counts.total = request_id + 1

    def _accumulate_done_requests(
        self,
        job_id: str,
        completed_request_ids: list[int],
        failed_request_ids: Optional[list[int]] = None,
    ) -> None:
        """Track finished request counts for this job."""
        request_counts = self._get_local_request_counts(job_id)
        request_counts.completed += len(completed_request_ids)
        request_counts.failed += len(failed_request_ids or [])

    def _accumulate_usage(
        self, job_id: str, custom_id: Optional[str], raw_usage: Optional[dict]
    ) -> None:
        """Add a single response's usage to the running per-job total.

        Maps OpenAI Completions naming (``prompt_tokens`` /
        ``completion_tokens``) to OpenAI Batch naming (``input_tokens`` /
        ``output_tokens``). Responses without a ``usage`` block are skipped;
        a duplicate ``custom_id`` within the job (retry) is skipped too.
        """
        if not raw_usage or not isinstance(raw_usage, dict):
            return

        self._ensure_local_job_state(job_id)
        seen = self._usage_counted_ids
        if custom_id is not None:
            if custom_id in seen:
                return
            seen.add(custom_id)

        if self._usage is None:
            self._usage = BatchUsage()
        usage = self._usage
        prompt = int(raw_usage.get("prompt_tokens") or 0)
        completion = int(raw_usage.get("completion_tokens") or 0)
        usage.input_tokens += prompt
        usage.output_tokens += completion
        usage.total_tokens += prompt + completion

        prompt_details = raw_usage.get("prompt_tokens_details") or {}
        if isinstance(prompt_details, dict):
            usage.input_tokens_details.cached_tokens += int(
                prompt_details.get("cached_tokens") or 0
            )
        completion_details = raw_usage.get("completion_tokens_details") or {}
        if isinstance(completion_details, dict):
            usage.output_tokens_details.reasoning_tokens += int(
                completion_details.get("reasoning_tokens") or 0
            )

    def _get_accumulated_usage(self, job_id: str) -> Optional[BatchUsage]:
        """Return the running token usage for a job, or None if no successful
        inference has been recorded yet."""
        if self._state_job_id != job_id or self._usage is None:
            return None
        # Hand out a copy so callers can't mutate the accumulator in place.
        return self._usage.model_copy(deep=True)

    def _retry_config_for_job(self, job: BatchJob) -> RetryConfig:
        policy = (
            job.spec.aibrix.client.retry_policy
            if job.spec.aibrix
            and job.spec.aibrix.client
            and job.spec.aibrix.client.retry_policy
            else None
        )
        return RetryConfig(
            max_retries=(
                policy.max_retries
                if policy is not None and policy.max_retries is not None
                else _inference_max_retries()
            ),
            base_delay_seconds=(
                policy.base_delay_seconds
                if policy is not None and policy.base_delay_seconds is not None
                else _retry_base_delay_seconds()
            ),
            max_delay_seconds=(
                policy.max_delay_seconds
                if policy is not None and policy.max_delay_seconds is not None
                else _retry_max_delay_seconds()
            ),
            no_endpoint_max_retries=(
                policy.no_endpoint_max_retries
                if policy is not None and policy.no_endpoint_max_retries is not None
                else _no_endpoint_max_retries()
            ),
        )

    def _dispatch_run_kwargs_for_job(self, job: BatchJob) -> Dict[str, Any]:
        client = job.spec.aibrix.client if job.spec.aibrix else None
        adaptive = (
            client.adaptive_concurrency
            if client is not None and client.adaptive_concurrency is not None
            else True
        )
        adaptive_max_factor = (
            client.adaptive_max_factor
            if client is not None and client.adaptive_max_factor is not None
            else _adaptive_max_factor()
        )
        max_concurrency = client.max_concurrency if client is not None else None
        kwargs: Dict[str, Any] = {
            "adaptive_concurrency": adaptive,
            "adaptive_max_factor": adaptive_max_factor,
        }
        if max_concurrency is not None:
            if adaptive:
                kwargs["adaptive_max_concurrency"] = max_concurrency
            else:
                kwargs["max_concurrency"] = max_concurrency
        return kwargs

    def _drop_usage_state(self, job_id: str) -> None:
        """Release per-job usage state after terminal persistence.
        Subclasses override this when they aggregate usage differently or need
        to keep extra per-job in-memory state beyond the base token counters.
        """
        if self._state_job_id != job_id:
            return
        self._state_job_id = None
        self._usage = None
        self._usage_counted_ids.clear()
        self._request_counts = None
        self._launched_request_ids.clear()

    async def _persist_worker_status(self, job: BatchJob) -> BatchJob:
        """Persist per-worker status after progress or failure updates.

        Subclasses override this when worker status copies need custom
        execution-ref handling or extra runtime-specific fields beyond usage.
        """
        status = job.status.model_copy(deep=True)
        status.request_counts = self._get_local_request_counts(job.job_id).model_copy(
            deep=True
        )
        status.usage = self._get_accumulated_usage(job.job_id)
        return await self._progress_manager.update_job_local_status(
            job.job_id, self._worker_id, status
        )

    async def _sync_completed_request_tasks(
        self,
        job: BatchJob,
        completed_request_results: Iterable[tuple[int, bool]],
    ) -> tuple[BatchJob, int]:
        """Compatibility seam for tests and drivers that sync completions after
        a pass or request batch."""
        completed_results = list(completed_request_results)
        completed_request_ids = [
            request_id for request_id, failed in completed_results if not failed
        ]
        failed_request_ids = [
            request_id for request_id, failed in completed_results if failed
        ]
        self._accumulate_done_requests(
            job.job_id,
            completed_request_ids,
            failed_request_ids,
        )
        job = await self._persist_worker_status(job)
        return job, len(completed_results)

    async def _execute_request(
        self,
        job: BatchJob,
        request_input: dict[str, Any],
    ) -> tuple[int, bool]:
        """Execute one request and persist its output record."""
        request_id = request_input.pop("_request_index")
        self._accumulate_dispatched_request(job.job_id, request_id)
        custom_id = request_input.get("custom_id", "")

        if "body" not in request_input:
            raise BatchJobError(
                code=BatchJobErrorCode.INVALID_INPUT_FILE,
                message="Request missing 'body' field",
                line=request_id,
            )

        request_output, last_error = await self._send_one(
            job.spec.endpoint, request_input["body"], request_id
        )

        if last_error is None and isinstance(request_output, dict):
            self._accumulate_usage(job.job_id, custom_id, request_output.get("usage"))

        response = self._build_response(
            custom_id, job.job_id, request_id, job.spec, request_output, last_error
        )
        await storage.write_job_output_data(job, request_id, response)
        return request_id, last_error is not None

    # ── lifecycle template ───────────────────────────────────────────────

    async def _resume_finalizing_job(self, job: BatchJob) -> BatchJob:
        """Resume a recovered job that is already in the finalizing phase.

        Subclasses override this when finalizing requires runtime-specific
        reattachment or cleanup before delegating to the normal finalize path.
        """
        await self._runtime.cleanup(job)
        job = await self.finalize_job(job)
        logger.info(
            "Runtime-backed job resumed directly into finalization.",
            job_id=job.job_id,
            state=job.status.state.value,
        )  # type: ignore[call-arg]
        return job

    async def _execute_job_in_runtime(
        self, runtime: Runtime, job: BatchJob
    ) -> BatchJob:
        if self._should_stop_before_proceed(job):
            return await self._finish_stopped_job(job)

        async with runtime.session(
            job,
            job.job_id,
            progress_manager=self._progress_manager,
            worker_id_generator=self._assign_worker_id,
        ) as endpoint:
            self._engine = (
                DispatchEngine(
                    endpoint.source,
                    retry=self._retry_config_for_job(job),
                )
                if endpoint.source is not None
                else None
            )
            self._active_model_name = endpoint.model_name

            try:
                job = await self._prepare_execution_job(job)

                if self._should_stop_before_proceed(job):
                    return await self._finish_stopped_job(job)
                job = await self.run_job(job.job_id, endpoint)
            except asyncio.CancelledError:
                raise
            except Exception as ex:  # noqa: BLE001 - finalize must still run
                normalized_error = self._make_failure_error(ex)
                self._log_failed(job.job_id, normalized_error)
                job = await self._progress_manager.mark_job_failed(
                    job.job_id, normalized_error
                )

            job = self._mark_job_completed_when_fully_processed(job)
            job = await self._after_execute_worker(job)

        return await self._finish_stopped_job(job)

    async def run_job(self, job_id: str, endpoint: Endpoint) -> BatchJob:
        """Run the job once provisioning is done. Two shapes:

        * dispatch (an endpoint exists): drive the per-request loop against it.
        * self-hosting (``endpoint.source is None`` and the runtime provisions):
          the worker dispatches inside its own compute (a k8s Job), so the
          control plane sends nothing and instead awaits the runtime to finish,
          then moves to FINALIZING for output aggregation.
        """
        if endpoint.source is None and self._runtime.provisions:
            return await self._await_self_hosted_run(job_id)
        return await self.execute_worker(job_id)

    async def _await_self_hosted_run(self, job_id: str) -> BatchJob:
        completion = await self._runtime.await_completion()
        job = await self._progress_manager.get_job(job_id)
        if job is None:
            raise BatchJobError(
                code=BatchJobErrorCode.INTERNAL_ERROR,
                message="job no longer exists after execution",
            )
        if not completion.succeeded:
            raise BatchJobError(
                code=BatchJobErrorCode.INFERENCE_FAILED,
                message=completion.message
                or completion.reason
                or "self-hosted job failed",
            )
        job.status.state = BatchJobState.FINALIZING
        return job

    # ── shared phases ────────────────────────────────────────────────────

    def _job_authentication(self, job: BatchJob) -> bool:
        return True

    async def prepare_job(self, job: BatchJob) -> BatchJob:
        """Prepare job output files by creating multipart uploads."""
        logger.debug("Preparing job output files")  # type: ignore[call-arg]
        job, stopped = await self._is_job_stopped(job.job_id)
        if stopped:
            # Simply return and leave caller to handle interruption.
            return job

        # Handle preparation failure injection
        self._maybe_fail_preparation(job)
        job = await storage.prepare_job_ouput_files(job)
        job = await self._progress_manager.update_job_status(job.job_id, job.status)
        logger.debug("Job output files prepared")  # type: ignore[call-arg]
        return job

    async def execute_worker(self, job_id) -> BatchJob:
        """Process requests without file preparation or finalization.

        Sending requests is delegated to the dispatch engine: the engine owns
        endpoint resolution, failover, and concurrency. Jobs with a known total
        use the concurrent engine loop only when the engine advertises parallel
        capacity and there is no serial-only compatibility option enabled.
        Unknown-total jobs keep the serial cursor because input EOF discovery is
        part of that protocol.
        """
        if self._engine is None:
            raise RuntimeError(
                "JobDriver was constructed without an engine; run_job / "
                "execute_worker require one. Build the driver with an "
                "EndpointSource-backed DispatchEngine (NoopEndpointSource for "
                "--dry-run, GatewayEndpointSource(url) for a real engine). "
                "(prepare_job / finalize_job do not need an engine.)"
            )

        job, stopped = await self._is_job_stopped(job_id)
        if stopped:
            return await self._finish_stopped_job(job)

        if (
            job.status.request_counts.total > 0
            and (await self._engine.capacity()).count > 1
        ):
            return await self._execute_worker_concurrent(job)

        return await self._execute_worker_serial(job)

    async def _execute_worker_serial(self, job: BatchJob) -> BatchJob:
        """Compatibility path for unknown-total inputs.

        It is serial only because the old cursor protocol discovers EOF while
        advancing one request at a time. Transport still goes through
        DispatchEngine, so this is not a second inference client.
        """
        assert self._engine is not None  # guaranteed by execute_worker
        job_id = job.job_id
        assert job_id is not None

        line_no = await self._get_next_pass_start(job)
        if line_no < 0:
            logger.warning(
                "Job has something wrong with metadata in batch manager, nothing left to execute",
                job_id=job_id,
            )  # type: ignore[call-arg]

            # There could be request count leaks. We leave finalize to calibrate the counters.
            return await self._finish_stopped_job(job, require_finalize=True)

        if line_no == 0:
            logger.debug("Start processing job", job_id=job_id, opts=job.spec.opts)  # type: ignore[call-arg]
        else:
            logger.debug(
                "Resuming job", job_id=job_id, request_id=line_no, opts=job.spec.opts
            )  # type: ignore[call-arg]

        fail_after_n_requests = self._parse_fail_after_n_requests(job)

        processed_requests = 0
        # The loop is necessary in cases of job driver lock a request but unable to process it (complete or fail), e.g., server crash.
        while line_no >= 0:
            async for request_input in storage.read_job_next_request(job, line_no):
                # Before-task job interruption check, will reload job's up-to-date status..
                job, stopped = await self._is_job_stopped(job.job_id)
                if stopped:
                    return await self._finish_stopped_job(job)

                # Execute task with concurrency-ready helpers
                completed_task = asyncio.create_task(
                    self._execute_request(job, request_input)
                )
                await completed_task
                job, completed = await self._sync_completed_request_tasks(
                    job, [completed_task.result()]
                )
                processed_requests += completed

                # fail_after_n_requests injection check
                if fail_after_n_requests is not None:
                    if processed_requests >= fail_after_n_requests:
                        logger.info(
                            "Triggering artificial failure due to fail_after_n_requests",
                            job_id=job_id,
                            processed_requests=processed_requests,
                            fail_after_n_requests=fail_after_n_requests,
                        )  # type: ignore[call-arg]
                        raise RuntimeError(
                            f"Artificial failure triggered after processing {processed_requests} requests "
                            f"(fail_after_n_requests={fail_after_n_requests})"
                        )

                # After-task job interruption check
                # Since job is reload during _sync_completed_request_tasks, no reloading neeaded.
                if self._should_stop_before_proceed(job):
                    return await self._finish_stopped_job(job)
            # End of ond round input scan

            reloaded_job = await self._progress_manager.get_job(job_id)
            if reloaded_job is None:
                raise RuntimeError(f"Job metadata missing for {job_id}")
            job = reloaded_job
            line_no = await self._get_next_pass_start(job)
            if line_no < 0 and job.status.request_counts.total > 0:
                logger.info(
                    "Job completed, total requests are processed",
                    job_id=job_id,
                    total=job.status.request_counts.total,
                )  # type: ignore[call-arg]
                return job

        logger.debug(
            "Worker completed, job state:",
            job_id=job_id,
            total=job.status.request_counts.total if job else None,
            state=job.status.state.value if job else None,
        )  # type: ignore[call-arg]
        return job

    async def _execute_worker_concurrent(self, job: BatchJob) -> BatchJob:
        """Known-total worker path backed by DispatchEngine.run().

        The request stream is storage-backed. Because DispatchEngine acquires a
        concurrency slot before pulling the next item, the worker only reads
        requests it can dispatch immediately.
        """
        assert self._engine is not None  # guaranteed by execute_worker
        job_id = job.job_id
        assert job_id is not None

        completed_request_ids: asyncio.Queue[Optional[tuple[int, bool]]] = (
            asyncio.Queue()
        )
        start_index = await self._get_next_pass_start(job)
        fail_after_n_requests = self._parse_fail_after_n_requests(job)
        processed_requests = 0
        fail_after_triggered = False
        if start_index < 0:
            logger.warning(
                "Job has something wrong with metadata in batch manager, nothing left to execute",
                job_id=job_id,
            )  # type: ignore[call-arg]

            # There could be request count leaks. We leave finalize to calibrate the counters.
            return await self._finish_stopped_job(job, require_finalize=True)

        pending_in_round = 0
        round_drained = asyncio.Event()
        round_drained.set()
        if start_index == 0:
            logger.debug(
                "Start processing job concurrently",
                job_id=job_id,
                total=job.status.request_counts.total,
                opts=job.spec.opts,
            )  # type: ignore[call-arg]
        else:
            logger.debug(
                "Resuming job concurrently",
                job_id=job_id,
                request_id=start_index,
                total=job.status.request_counts.total,
                opts=job.spec.opts,
            )  # type: ignore[call-arg]

        async def feed():
            nonlocal job, start_index, pending_in_round
            # The loop is necessary in cases of job driver lock a request but unable to process it (complete or fail), e.g., server crash.
            while start_index >= 0:
                async for request_input in storage.read_job_next_request(
                    job, start_index
                ):
                    request_id = request_input.pop("_request_index", -1)
                    if request_id < 0:
                        continue
                    self._accumulate_dispatched_request(job_id, request_id)
                    pending_in_round += 1
                    round_drained.clear()

                    if "body" not in request_input:
                        raise BatchJobError(
                            code=BatchJobErrorCode.INVALID_INPUT_FILE,
                            message="Request missing 'body' field",
                            line=request_id,
                        )

                    custom_id = request_input.get("custom_id", "")
                    yield InferenceRequest(
                        path=job.spec.endpoint,
                        payload=self._shape_payload(request_input["body"]),
                        ref=(request_id, custom_id),
                    )

                await round_drained.wait()
                reloaded_job = await self._progress_manager.get_job(job_id)
                if reloaded_job is None:
                    raise RuntimeError(f"Job metadata missing for {job_id}")
                job = reloaded_job
                start_index = await self._get_next_pass_start(job)
                local_request_counts = self._get_local_request_counts(job_id)
                if start_index < 0 and local_request_counts.total > 0:
                    logger.info(
                        "Job completed, total requests are processed",
                        job_id=job_id,
                        total=local_request_counts.total,
                    )  # type: ignore[call-arg]
                    return

        async def sync_completed_requests() -> None:
            nonlocal job, pending_in_round

            async def sync_batch(request_results: list[tuple[int, bool]]) -> None:
                nonlocal job, pending_in_round
                if not request_results:
                    return
                job, _ = await self._sync_completed_request_tasks(
                    job,
                    request_results,
                )
                pending_in_round = max(0, pending_in_round - len(request_results))
                if pending_in_round == 0:
                    round_drained.set()

            while True:
                request_result = await completed_request_ids.get()
                if request_result is None:
                    return

                batch = [request_result]
                while True:
                    try:
                        next_request_result = completed_request_ids.get_nowait()
                    except asyncio.QueueEmpty:
                        break
                    if next_request_result is None:
                        await sync_batch(batch)
                        return
                    batch.append(next_request_result)

                await sync_batch(batch)

        async def on_result(
            request: InferenceRequest,
            response: Optional[dict[str, Any]],
            error: Optional[InferenceError],
        ) -> None:
            nonlocal job, processed_requests, fail_after_triggered
            request_id, custom_id = request.ref
            if error is None and isinstance(response, dict):
                async with self._usage_lock:
                    self._accumulate_usage(job_id, custom_id, response.get("usage"))

            record = self._build_response(
                custom_id, job_id, request_id, job.spec, response, error
            )
            await storage.write_job_output_data(job, request_id, record)
            await completed_request_ids.put((request_id, error is not None))
            processed_requests += 1
            if (
                not fail_after_triggered
                and fail_after_n_requests is not None
                and processed_requests >= fail_after_n_requests
            ):
                fail_after_triggered = True
                raise RuntimeError(
                    f"Artificial failure triggered after processing {processed_requests} requests "
                    f"(fail_after_n_requests={fail_after_n_requests})"
                )

        stats = DispatchStats()
        telemetry_interval = _telemetry_interval_seconds()
        sync_task = asyncio.create_task(sync_completed_requests())
        telemetry_task = (
            asyncio.create_task(
                self._log_dispatch_telemetry(
                    job_id,
                    stats,
                    telemetry_interval,
                )
            )
            if telemetry_interval > 0
            else None
        )
        try:
            run_kwargs = self._dispatch_run_kwargs_for_job(job)
            await self._engine.run(
                feed(),
                on_result,
                stats=stats,
                **run_kwargs,
            )
        finally:
            await completed_request_ids.put(None)
            with contextlib.suppress(asyncio.CancelledError):
                await sync_task
            if telemetry_task is not None:
                telemetry_task.cancel()
                with contextlib.suppress(asyncio.CancelledError):
                    await telemetry_task
            self._emit_dispatch_telemetry(
                job_id,
                stats.snapshot(reset_window=True),
                telemetry_interval if telemetry_interval > 0 else 1.0,
                final=True,
            )

        reloaded_job = await self._progress_manager.get_job(job_id)
        if reloaded_job is None:
            raise RuntimeError(f"Job metadata missing for {job_id}")
        job = reloaded_job

        logger.debug(
            "Worker completed, job state:",
            job_id=job_id,
            total=job.status.request_counts.total if job else None,
            state=job.status.state.value if job else None,
        )  # type: ignore[call-arg]
        return job

    async def _log_dispatch_telemetry(
        self,
        job_id: str,
        stats: DispatchStats,
        interval: float,
    ) -> None:
        while True:
            started = asyncio.get_running_loop().time()
            await asyncio.sleep(interval)
            elapsed = max(asyncio.get_running_loop().time() - started, 1e-9)
            self._emit_dispatch_telemetry(
                job_id,
                stats.snapshot(reset_window=True),
                elapsed,
                final=False,
            )

    def _emit_dispatch_telemetry(
        self,
        job_id: str,
        snapshot: DispatchStatsSnapshot,
        window_seconds: float,
        *,
        final: bool,
    ) -> None:
        window_seconds = max(window_seconds, 1e-9)
        logger.info(
            "Batch dispatch telemetry",
            job_id=job_id,
            final=final,
            started_qps=round(snapshot.window_started / window_seconds, 3),
            completed_qps=round(snapshot.window_completed / window_seconds, 3),
            failed_qps=round(snapshot.window_failed / window_seconds, 3),
            inflight=snapshot.inflight,
            concurrency_limit=snapshot.limit,
            max_inflight=snapshot.max_inflight,
            started=snapshot.started,
            completed=snapshot.completed,
            failed=snapshot.failed,
            window_started=snapshot.window_started,
            window_completed=snapshot.window_completed,
            window_failed=snapshot.window_failed,
            avg_latency_seconds=_round_optional(snapshot.avg_latency_seconds),
            p95_latency_seconds=_round_optional(snapshot.p95_latency_seconds),
        )  # type: ignore[call-arg]

    async def _send_one(
        self, endpoint: str, body: Dict[str, Any], request_id: int
    ) -> tuple[Any, Optional[Exception]]:
        """Serial fallback adapter around DispatchEngine.send_one."""
        assert self._engine is not None  # guaranteed by execute_worker
        try:
            output = await self._engine.send_one(
                InferenceRequest(
                    path=endpoint, payload=self._shape_payload(body), ref=request_id
                )
            )
            return output, None
        except InferenceError as exc:
            logger.warning(
                f"Inference request failed: {exc}",
                request_id=request_id,
            )  # type: ignore[call-arg]
            return None, exc

    def _opt_enabled(self, job: BatchJob, opt_key: str) -> bool:
        """Parse a boolean-style batch option from job opts.

        This helper remains overridable because some backends may want custom
        truthiness or option parsing semantics.
        """
        if not job.spec.opts or opt_key not in job.spec.opts:
            return False
        value = job.spec.opts[opt_key]
        normalized = str(value).strip().lower()
        return normalized not in {"", "0", "false", "no", "off"}

    def _maybe_fail_preparation(self, job: BatchJob) -> None:
        """Inject a preparation failure when test opts request it.

        This is overridable because subclasses may prepare outputs differently
        or need backend-specific failure codes for preparation-stage faults.
        """
        if not self._opt_enabled(job, constant.BATCH_OPTS_FAIL_PREPARATION):
            return
        raise BatchJobError(
            code=BatchJobErrorCode.PREPARE_OUTPUT_ERROR,
            message=(
                "Artificial preparation failure triggered "
                f"({constant.BATCH_OPTS_FAIL_PREPARATION})"
            ),
        )

    def _parse_fail_after_n_requests(self, job: BatchJob) -> Optional[int]:
        """Parse the test-only fail-after counter from job opts.

        Subclasses override this when request accounting or failure injection
        should key off different option names or counting semantics.
        """
        if (
            not job.spec.opts
            or constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS not in job.spec.opts
        ):
            return None
        try:
            return int(job.spec.opts[constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS])
        except (ValueError, TypeError):
            logger.warning(
                "Invalid fail_after_n_requests value, ignoring",
                job_id=job.job_id,
                value=job.spec.opts[constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS],
            )  # type: ignore[call-arg]
            return None

    def _shape_payload(self, payload: Dict[str, Any]) -> Dict[str, Any]:
        """Adjust a request body before dispatch. Pins the request to the
        runtime's served model when one is known (single-model backends like a
        provisioned Deployment), so the input's ``model`` field can't misroute;
        identity otherwise."""
        if self._active_model_name is None:
            return payload
        shaped = dict(payload)
        shaped["model"] = self._active_model_name
        return shaped

    def _shape_output_payload(
        self, payload: Dict[str, Any], jobSpec: BatchJobSpec
    ) -> Dict[str, Any]:
        """Adjust a request body before dispatch. Pins the request to the
        runtime's served model when one is known (single-model backends like a
        provisioned Deployment), so the input's ``model`` field can't misroute;
        identity otherwise."""
        if jobSpec.model is None:
            return payload
        shaped = dict(payload)
        shaped["model"] = jobSpec.model
        return shaped

    async def finalize_job(self, job: BatchJob) -> BatchJob:
        """Aggregate outputs (when this driver owns aggregation) and mark done."""

        try:
            job = await self._progress_manager.mark_job_finalizing(job.job_id)

            await storage.finalize_job_output_data(job)
            synced = await self._progress_manager.mark_job_done(job)
            logger.debug("Finalized job", job_id=job.job_id)  # type: ignore[call-arg]
            self._log_completed(synced)
        except Exception as e:
            logger.error("Error finalizing job output data: %s", e)  # type: ignore[call-arg]
            raise BatchJobError(
                BatchJobErrorCode.FINALIZING_ERROR,
                message=str(e),
            )
        finally:
            # Memory hygiene: drop the per-job accumulator now that the
            # final usage is on the status object.
            self._drop_usage_state(job.job_id)

        return synced

    def _build_response(
        self,
        custom_id: str,
        job_id: str,
        request_id: int,
        job_spec: BatchJobSpec,
        request_output: Any = None,
        error: Optional[Exception] = None,
    ) -> dict[str, Any]:
        response: dict[str, Any] = {
            "id": uuid.uuid4().hex[:5],
            "error": None,
            "response": None,
            "custom_id": custom_id,
        }

        if error is not None:
            logger.error(
                f"All inference attempts failed after retries: {error}",
                job_id=job_id,
                request_id=request_id,
            )  # type: ignore[call-arg]
            response["error"] = BatchJobError(
                code=BatchJobErrorCode.INFERENCE_FAILED, message=str(error)
            )
        else:
            if isinstance(request_output, dict):
                request_output = self._shape_output_payload(request_output, job_spec)
            response["response"] = {
                "status_code": 200,
                "request_id": f"{job_id}-{request_id}",
                "body": request_output,
            }

        return response
