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
import uuid
from typing import Any, Dict, Optional, Set

import aibrix.batch.constant as constant
import aibrix.batch.storage as storage
from aibrix.batch.client import DispatchEngine, InferenceError, InferenceRequest
from aibrix.batch.job_driver.runtime import Endpoint, NoopRuntime, Runtime
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobErrorCode,
    BatchJobState,
    BatchUsage,
    ConditionType,
)
from aibrix.batch.state import RunningJobs
from aibrix.logger import init_logger

logger = init_logger(__name__)


class BaseJobDriver:
    def __init__(
        self,
        progress_manager: RunningJobs,
        runtime: Optional[Runtime] = None,
        *,
        reraise_on_failure: Optional[bool] = None,
        aggregate_always: Optional[bool] = None,
        default_failure_code: Optional[BatchJobErrorCode] = None,
    ) -> None:
        self._progress_manager = progress_manager
        # The single seam. A provisioning runtime (Kubernetes / KubernetesJob /
        # cloud) is selected per-job; ``NoopRuntime`` is the prepare/finalize-
        # only default.
        self._runtime: Runtime = runtime or NoopRuntime()

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
        self._aggregate_always: bool = (
            provisions if aggregate_always is None else aggregate_always
        )
        self._default_failure_code: BatchJobErrorCode = default_failure_code or (
            BatchJobErrorCode.INTERNAL_ERROR
            if provisions
            else BatchJobErrorCode.INFERENCE_FAILED
        )

        # Engine is (re)built from the runtime's endpoint during a session; it
        # may stay None when the driver only prepares/finalizes (no inference).
        self._engine: Optional[DispatchEngine] = None
        self._active_model_name: Optional[str] = None
        # Whether finalize_job should aggregate outputs; the template sets this
        # per run from ``_aggregate_policy``.
        self._aggregate_on_finalize: bool = True

        # Per-job token usage accumulators. Populated by inference responses in
        # execute_worker. Idempotent on retry: each (job_id, custom_id) pair
        # contributes at most once.
        self._usage_by_job: Dict[str, BatchUsage] = {}
        self._usage_counted_ids: Dict[str, Set[str]] = {}

    # ── failure / error helpers ──────────────────────────────────────────

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

    def _aggregate_policy(self, i_prepared: bool) -> bool:
        """Whether finalize_job should aggregate outputs. A provisioning backend
        that owns the whole run always aggregates; the inline path defers
        aggregation to the coordinator when it did not create the temp files
        (parallel worker)."""
        return True if self._aggregate_always else i_prepared

    # ── usage accounting ─────────────────────────────────────────────────

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

        seen = self._usage_counted_ids.setdefault(job_id, set())
        if custom_id is not None:
            if custom_id in seen:
                return
            seen.add(custom_id)

        usage = self._usage_by_job.setdefault(job_id, BatchUsage())
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

    def get_accumulated_usage(self, job_id: str) -> Optional[BatchUsage]:
        """Return the running token usage for a job, or None if no successful
        inference has been recorded yet."""
        usage = self._usage_by_job.get(job_id)
        if usage is None:
            return None
        return usage.model_copy(deep=True)

    def _drop_usage_state(self, job_id: str) -> None:
        self._usage_by_job.pop(job_id, None)
        self._usage_counted_ids.pop(job_id, None)

    # BUG: This function is not thread-safe. Usage from multiple workers can overwrite each other.
    async def _snapshot_usage_to_status(self, job_id: str) -> None:
        """Push the current accumulator into the live BatchJob's status so
        downstream persistence reflects the latest tally."""
        accumulated = self.get_accumulated_usage(job_id)
        if accumulated is None:
            return
        current = await self._progress_manager.get_job(job_id)
        if current is not None:
            current.status.usage = accumulated

    # ── lifecycle template ───────────────────────────────────────────────

    async def execute(self, job_id) -> None:
        """Run the full lifecycle: session(provision/teardown) wraps
        prepare -> run_job -> finalize. Behavior matches the legacy inline
        driver; backends that need a different shape override this method.
        """
        job = await self._progress_manager.get_job(job_id)
        if job is None:
            logger.warning("Job not found", job_id=job_id)  # type: ignore[call-arg]
            return

        # "Did THIS driver create the temp files?" — drives both prepare-skip
        # (resume / parallel-worker) and finalize aggregation ownership.
        i_prepared = not (
            job.status.temp_output_file_id and job.status.temp_error_file_id
        )
        self._aggregate_on_finalize = self._aggregate_policy(i_prepared)

        try:
            async with self._runtime.session(job, job_id) as endpoint:
                self._engine = (
                    DispatchEngine(endpoint.source)
                    if endpoint.source is not None
                    else None
                )
                self._active_model_name = endpoint.model_name

                if i_prepared:
                    logger.debug("Temp files not created, creating...", job_id=job_id)  # type: ignore[call-arg]
                    job = await self.prepare_job(job)
                    # Release/start a self-hosting provisioned worker now that
                    # the files it writes to exist (e.g. un-suspend a k8s Job).
                    # NOOP for runtimes already serving by ``connect``.
                    await self._runtime.on_prepared()

                try:
                    job = await self.run_job(job_id, endpoint)
                except Exception as ex:  # noqa: BLE001 - finalize must still run
                    job = await self._progress_manager.mark_job_failed(
                        job_id, self._make_failure_error(ex)
                    )
                    await self._snapshot_usage_to_status(job_id)
                    self._drop_usage_state(job_id)

                if (
                    job.status.state == BatchJobState.FINALIZING
                    and not self._runtime.cancelled()
                ):
                    logger.debug("Finalizing job", job_id=job_id)  # type: ignore[call-arg]
                    job = await self.finalize_job(job)
        except asyncio.CancelledError:
            # A provisioning runtime cancels the run when its job is deleted;
            # teardown already ran via the session. Swallow only then.
            if self._runtime.cancelled():
                logger.info("Execution interrupted by job deletion", job_id=job_id)  # type: ignore[call-arg]
                return
            raise

        if self._reraise_on_failure and job.status.failed:
            failed_condition = job.status.get_condition(ConditionType.FAILED)
            if failed_condition is None:
                raise RuntimeError("Job failed but no failure condition was set")
            raise RuntimeError(
                failed_condition.message or "Job failed with an unspecified error"
            )

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

    async def validate_job(self, job: BatchJob):
        total, exists = await storage.read_job_input_info(job)
        if not exists:
            raise BatchJobError(
                code=BatchJobErrorCode.INVALID_INPUT_FILE,
                message="input file not found",
            )
        if not self._job_authentication(job):
            raise BatchJobError(
                code=BatchJobErrorCode.AUTHENTICATION_ERROR,
                message="authentication error",
            )

    def _job_authentication(self, job: BatchJob) -> bool:
        return True

    async def prepare_job(self, job: BatchJob) -> BatchJob:
        """Prepare job output files by creating multipart uploads."""
        logger.debug("Preparing job output files")  # type: ignore[call-arg]
        job = await storage.prepare_job_ouput_files(job)
        logger.debug("Job output files prepared")  # type: ignore[call-arg]
        return job

    async def execute_worker(self, job_id) -> BatchJob:
        """Process requests without file preparation or finalization.

        Sending one request is delegated to the dispatch engine (``send_one``):
        the engine owns endpoint resolution and failover. The metastore's
        streaming launch/complete protocol (resume, skip, total detection)
        stays here, request by request.
        """
        if self._engine is None:
            raise RuntimeError(
                "JobDriver was constructed without an engine; run_job / "
                "execute_worker require one. Build the driver with an "
                "EndpointSource-backed DispatchEngine (NoopEndpointSource for "
                "--dry-run, GatewayEndpointSource(url) for a real engine). "
                "(prepare_job / finalize_job do not need an engine.)"
            )

        job, line_no = await self._get_next_request(job_id)
        if line_no < 0:
            logger.warning(
                "Job has something wrong with metadata in job manager, nothing left to execute",
                job_id=job_id,
            )  # type: ignore[call-arg]
            return job

        if line_no == 0:
            logger.debug("Start processing job", job_id=job_id, opts=job.spec.opts)  # type: ignore[call-arg]
        else:
            logger.debug(
                "Resuming job", job_id=job_id, request_id=line_no, opts=job.spec.opts
            )  # type: ignore[call-arg]

        fail_after_n_requests = self._parse_fail_after_n_requests(job)

        processed_requests = 0
        last_line_no = line_no
        while line_no >= 0:
            async for request_input in storage.read_job_next_request(job, line_no):
                next_line_no = request_input.pop("_request_index", last_line_no)
                while last_line_no < next_line_no:
                    if await storage.is_request_done(job, last_line_no):
                        job, line_no = await self._sync_job_status_and_get_next_request(
                            job_id, last_line_no
                        )
                    else:
                        job, line_no = await self._get_next_request(job_id)
                    if line_no < last_line_no:
                        break
                    last_line_no = line_no

                if line_no < last_line_no:
                    break

                if line_no != next_line_no:
                    raise RuntimeError(
                        f"Metastore inconsistency: expected request index {line_no} but got {next_line_no}"
                    )

                custom_id = request_input.get("custom_id", "")

                if "body" not in request_input:
                    raise BatchJobError(
                        code=BatchJobErrorCode.INVALID_INPUT_FILE,
                        message="Request missing 'body' field",
                        line=line_no,
                    )

                request_output, last_error = await self._send_one(
                    job.spec.endpoint, request_input["body"], line_no
                )

                if last_error is None and isinstance(request_output, dict):
                    self._accumulate_usage(
                        job_id, custom_id, request_output.get("usage")
                    )

                response = self._build_response(
                    custom_id, job_id, line_no, request_output, last_error
                )
                await storage.write_job_output_data(job, line_no, response)

                assert last_line_no == line_no

                if fail_after_n_requests is not None:
                    processed_requests += 1
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
                job, line_no = await self._sync_job_status_and_get_next_request(
                    job_id, last_line_no
                )
                await self._snapshot_usage_to_status(job_id)
                if line_no < last_line_no:
                    break
                last_line_no = line_no

            if last_line_no == line_no:
                job = await self._sync_job_status(job_id, total=line_no)
                job, line_no = await self._get_next_request(job_id)

        logger.debug(
            "Worker completed, job state:",
            job_id=job_id,
            total=job.status.request_counts.total if job else None,
            state=job.status.state.value if job else None,
        )  # type: ignore[call-arg]
        return job

    async def _send_one(
        self, endpoint: str, body: Dict[str, Any], request_id: int
    ) -> tuple[Any, Optional[Exception]]:
        """Dispatch one request via the engine, returning (output, error)."""
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

    @staticmethod
    def _parse_fail_after_n_requests(job: BatchJob) -> Optional[int]:
        if not job.spec.opts or constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS not in (
            job.spec.opts
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

    async def finalize_job(self, job: BatchJob) -> BatchJob:
        """Aggregate outputs (when this driver owns aggregation) and mark done."""
        assert job.status.state == BatchJobState.FINALIZING

        if self._aggregate_on_finalize:
            await storage.finalize_job_output_data(job)

        job_id = job.status.job_id
        await self._snapshot_usage_to_status(job_id)
        logger.debug("Finalized job", job_id=job_id)  # type: ignore[call-arg]
        synced = await self._sync_job_status(job_id)
        self._drop_usage_state(job_id)
        return synced

    def _build_response(
        self,
        custom_id: str,
        job_id: str,
        request_id: int,
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
            response["response"] = {
                "status_code": 200,
                "request_id": f"{job_id}-{request_id}",
                "body": request_output,
            }

        return response

    async def _sync_job_status(self, job_id, request_id=-1, total=0) -> BatchJob:
        if total > 0:
            return await self._progress_manager.mark_job_total(job_id, total)
        elif request_id < 0:
            return await self._progress_manager.mark_job_done(job_id)
        else:
            return await self._progress_manager.mark_jobs_progresses(
                job_id, [request_id]
            )

    async def _get_next_request(self, job_id: str) -> tuple[BatchJob, int]:
        return await self._progress_manager.get_job_next_request(job_id)

    async def _sync_job_status_and_get_next_request(
        self, job_id: str, request_id: int
    ) -> tuple[BatchJob, int]:
        return await self._progress_manager.mark_job_progress_and_get_next_request(
            job_id, request_id
        )
