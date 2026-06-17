# Copyright 2024 The Aibrix Team.
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
import os
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone
from typing import List, Optional, Tuple
from urllib.parse import urlparse

import httpx

import aibrix.batch.constant as constant
from aibrix.batch.client import GatewayEndpointSource
from aibrix.batch.job_driver.base import BaseJobDriver
from aibrix.batch.job_driver.runtime import ExternalRuntime
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobError,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    Condition,
    ConditionStatus,
    ConditionType,
)
from aibrix.batch.state import JobMetaInfo
from aibrix.logger import init_logger

logger = init_logger(__name__)


class LLMHealthChecker:
    """Health checker for vLLM service readiness."""

    def __init__(
        self,
        health_url: str,
        check_interval: int = 1,
        timeout: int = 300,
    ):
        self.health_url = health_url
        self.check_interval = check_interval
        self.timeout = timeout

    async def wait_for_ready(self) -> bool:
        """Wait for vLLM service to become ready."""
        logger.info(
            "Waiting for vLLM service to become ready", health_url=self.health_url
        )  # type: ignore[call-arg]

        start_time = time.time()
        async with httpx.AsyncClient() as client:
            while time.time() - start_time < self.timeout:
                try:
                    response = await client.get(self.health_url, timeout=5.0)
                    if response.status_code == 200:
                        logger.info("vLLM service is ready")
                        return True
                except (httpx.RequestError, httpx.TimeoutException) as e:
                    logger.debug("vLLM not ready yet", error=str(e))  # type: ignore[call-arg]

                await asyncio.sleep(self.check_interval)

        logger.error(
            "LLM engine did not become ready within timeout",
            timeout_seconds=self.timeout,
        )  # type: ignore[call-arg]
        return False


class SingleJobRunner:
    """A minimal ``RunningJobs`` for executing ONE batch job in-process (the
    batch worker pod).

    Wraps a single ``JobMetaInfo`` (BatchJob + JobProgressTracker) and answers
    the driver's per-request progress calls directly — no scheduler, no job
    pools, no entity-manager persistence. It mirrors ``BatchManager``'s per-job
    semantics for one job, reusing ``JobMetaInfo`` / ``JobProgressTracker`` so
    the launch/complete bitmap is shared, not reimplemented. This lets the
    worker execute a job through ``BaseJobDriver`` without standing up the
    list-level orchestrator (``BatchDriver`` / scheduler / ``BatchManager``).
    """

    def __init__(self, job: BatchJob) -> None:
        self._meta = JobMetaInfo(job)

    async def get_job(self, job_id: str) -> Optional[BatchJob]:
        return self._meta if job_id == self._meta.job_id else None

    async def get_job_next_request(self, job_id: str) -> Tuple[BatchJob, int]:
        return self._meta, self._meta.next_request_id()

    async def complete_job_request(
        self, job_id: str, req_id: int, failed: bool = False
    ) -> BatchJob:
        async with self._meta._async_lock:
            total = self._meta.status.request_counts.total
            if req_id < 0 or req_id >= total:
                raise ValueError(f"invalid request_id: {req_id}")
            self._meta.complete_one_request(req_id, failed=failed)
            return self._meta

    async def mark_job_progress_and_get_next_request(
        self, job_id: str, req_id: int
    ) -> Tuple[BatchJob, int]:
        self._meta.complete_one_request(req_id)
        return self._meta, self._meta.next_request_id()

    async def mark_jobs_progresses(
        self, job_id: str, executed_requests: List[int]
    ) -> BatchJob:
        total = self._meta.status.request_counts.total
        for req_id in executed_requests:
            if req_id < 0 or req_id > total:
                logger.error(  # type: ignore[call-arg]
                    "Mark job progress failed - request index out of boundary",
                    job_id=job_id,
                    req_id=req_id,
                    total=total,
                )
                continue
            self._meta.complete_one_request(req_id)
        return self._meta

    async def mark_job_total(self, job_id: str, total_requests: int) -> BatchJob:
        # Mirrors BatchManager.mark_job_total -> mark_job_progress(total+1): the
        # (total+1) sentinel tells the tracker the total is now known.
        req_id = total_requests + 1
        if req_id < 0 or req_id > self._meta.status.request_counts.total:
            raise ValueError(f"invalid request_id: {req_id}")
        self._meta.complete_one_request(req_id)
        return self._meta

    async def mark_job_done(self, job_id: str) -> BatchJob:
        job = self._meta
        if job.status.state != BatchJobState.FINALIZING:
            raise RuntimeError(f"Job is not in finalizing state: {job.status.state}")
        job.status.completed_at = datetime.now(timezone.utc)
        job.status.finalized_at = job.status.completed_at
        if job.status.condition is None:
            job.status.add_condition(
                Condition(
                    type=ConditionType.COMPLETED,
                    status=ConditionStatus.TRUE,
                    lastTransitionTime=job.status.completed_at,
                )
            )
        job.status.state = BatchJobState.FINALIZED
        return job

    async def mark_job_failed(self, job_id: str, ex: BatchJobError) -> BatchJob:
        job = self._meta
        job.status.failed_at = datetime.now(timezone.utc)
        job.status.add_condition(
            Condition(
                type=ConditionType.FAILED,
                status=ConditionStatus.TRUE,
                lastTransitionTime=job.status.failed_at,
                reason=ex.code,
                message=ex.message,
            )
        )
        job.status.errors = [ex]
        if job.status.state == BatchJobState.IN_PROGRESS:
            job.status.finalizing_at = datetime.now(timezone.utc)
            job.status.state = BatchJobState.FINALIZING
        else:
            job.status.finalized_at = job.status.failed_at
            job.status.state = BatchJobState.FINALIZED
        return job


class BatchWorker:
    """Batch worker that processes jobs using the sidecar pattern."""

    def __init__(self) -> None:
        self.health_checker: Optional[LLMHealthChecker] = None
        self.llm_engine_base_url: Optional[str] = None

    def load_job_from_env(self) -> BatchJob:
        """Generate BatchJob from environment variables set by pod annotations."""
        logger.info("Loading job specification from environment variables...")

        # Get basic job information
        job_name = os.getenv("JOB_NAME")
        job_ns = os.getenv("JOB_NAMESPACE")
        job_id = os.getenv("JOB_UID")

        if not job_id:
            raise ValueError("JOB_UID environment variable is required")
        if not job_name:
            raise ValueError("JOB_NAME environment variable is required")
        if not job_ns:
            raise ValueError("JOB_NAMESPACE environment variable is required")

        # Get batch job metadata from environment variables
        input_file_id = os.getenv("BATCH_INPUT_FILE_ID")
        endpoint = os.getenv("BATCH_ENDPOINT")
        opts: dict[str, str] = {}
        if (
            failed_after_after_n_requests := os.getenv(
                "BATCH_OPTS_FAIL_AFTER_N_REQUESTS"
            )
        ) is not None:
            opts[constant.BATCH_OPTS_FAIL_AFTER_N_REQUESTS] = (
                failed_after_after_n_requests
            )
        # Expiration window is set on Job spec: activeDeadlineSeconds

        # Get file IDs
        output_file_id = os.getenv("BATCH_OUTPUT_FILE_ID")
        temp_output_file_id = os.getenv("BATCH_TEMP_OUTPUT_FILE_ID")
        error_file_id = os.getenv("BATCH_ERROR_FILE_ID")
        temp_error_file_id = os.getenv("BATCH_TEMP_ERROR_FILE_ID")

        if not input_file_id:
            raise ValueError("BATCH_INPUT_FILE_ID environment variable is required")
        if not endpoint:
            raise ValueError("BATCH_ENDPOINT environment variable is required")

        logger.info(
            "Confirmed Job",
            name=job_name,
            namespace=job_ns,
            job_id=job_id,
            input_file_id=input_file_id,
            endpoint=endpoint,
            opts=opts,
        )  # type: ignore[call-arg]

        try:
            # Initialize health checker
            health_url = os.getenv("LLM_READY_ENDPOINT", "http://localhost:8000/health")
            self.health_checker = LLMHealthChecker(health_url)
            logger.info("Set health checker", health_url=health_url)  # type: ignore[call-arg]

            # Try to construct base URL from health URL
            parsed = urlparse(health_url)
            base_url = f"{parsed.scheme}://{parsed.netloc}"

            self.llm_engine_base_url = base_url
            logger.info("Set LLM engine base URL", base_url=base_url)  # type: ignore[call-arg]

            # Create BatchJobSpec
            spec = BatchJobSpec.from_strings(
                input_file_id=input_file_id,
                endpoint=endpoint,
                metadata=None,
                opts=opts,
            )

            # Determine state based on file IDs (as in current transformer logic)
            validated = (
                output_file_id is not None
                and temp_output_file_id is not None
                and error_file_id is not None
                and temp_error_file_id is not None
            )
            state = BatchJobState.IN_PROGRESS if validated else BatchJobState.CREATED

            # Create BatchJobStatus
            status = BatchJobStatus(
                jobID=job_id,
                state=state,
                outputFileID=output_file_id,
                tempOutputFileID=temp_output_file_id,
                errorFileID=error_file_id,
                tempErrorFileID=temp_error_file_id,
                createdAt=datetime.now(timezone.utc),
            )

            # Create BatchJob
            batch_job = BatchJob.new_from_spec(job_name, job_ns, spec)
            batch_job.status = status

            logger.info(
                "Successfully generated BatchJob from environment variables",
                job_name=batch_job.metadata.name,
                job_namespace=batch_job.metadata.namespace,
                job_id=batch_job.job_id,
                state=state.value,
                validated=validated,
            )  # type: ignore[call-arg]

            return batch_job
        except Exception as e:
            raise RuntimeError(
                f"Error creating BatchJob from environment variables: {e}"
            ) from e

    async def wait_for_in_progress(
        self, job: BatchJob, max_wait: int = 300
    ) -> BatchJob:
        """Wait for job to reach IN_PROGRESS state by watching Kubernetes job updates.

        Raises:
            ReadTimeoutError if job does not reach IN_PROGRESS state within max_wait seconds.
        """
        if job.status.state != BatchJobState.CREATED:
            logger.info(
                "Job already in non-CREATED state",
                job_id=job.job_id,
                current_state=job.status.state.value,
                output_file_id=job.status.output_file_id,
                temp_output_file_id=job.status.temp_output_file_id,
                error_file_id=job.status.error_file_id,
                temp_error_file_id=job.status.temp_error_file_id,
            )  # type: ignore[call-arg]
            return job

        logger.info(
            "Waiting for job to reach IN_PROGRESS state",
            job_id=job.job_id,
            job_name=job.metadata.name,
            namespace=job.metadata.namespace,
            max_wait=max_wait,
        )  # type: ignore[call-arg]

        # [TODO][NOW] Use metestore to watch in_progress status.

        # Unlikely, should raise ReadTimeoutError
        return job

    async def run(self) -> int:
        """Main worker execution flow."""
        try:
            # Step 1: Load job specification from env (downward API).
            # The K8s API path was removed: it was off by default and
            # required job-reader-sa RBAC for a path that nothing in
            # the rendered manifest ever exercises. Annotations →
            # downward API → env vars is sufficient and zero-privilege.
            batch_job = self.load_job_from_env()

            logger.info(
                "Loaded job specification",
                input_file_id=batch_job.spec.input_file_id,
                endpoint=batch_job.spec.endpoint,
            )  # type: ignore[call-arg]

            # Wait for job to become IN_PROGRESS (metadata server will prepare the job)
            batch_job = await self.wait_for_in_progress(batch_job)
            if batch_job.status.state != BatchJobState.IN_PROGRESS:
                logger.error("Job failed to reach IN_PROGRESS state")
                return 1

            # Step 2: Wait for vLLM service to become ready
            if self.health_checker is None:
                raise RuntimeError("Health checker should be initialized")
            if not await self.health_checker.wait_for_ready():
                logger.error("vLLM service failed to become ready")
                return 1

            # Step 3: Execute the single job in-process through a JobDriver.
            # No BatchDriver / scheduler / BatchManager: a SingleJobRunner gives
            # the driver the per-job RunningJobs surface it needs, and the worker
            # dispatches against its own pod-local engine. execute runs
            # prepare(skipped — the metadata service already prepared the output
            # files) -> run_job -> finalize, and re-raises on failure
            # (BaseJobDriver._reraise_on_failure=True).
            job_id = batch_job.job_id
            if job_id is None:
                raise RuntimeError("BatchJob job_id is None")

            runner = SingleJobRunner(batch_job)
            if self.llm_engine_base_url is None:
                raise RuntimeError("engine base url not resolved")
            driver = BaseJobDriver(
                runner,
                ExternalRuntime(GatewayEndpointSource(self.llm_engine_base_url)),
            )
            logger.info("Executing batch job", job_id=job_id)  # type: ignore[call-arg]
            await driver.execute(job_id)
            logger.info("Batch worker completed successfully", job_id=job_id)  # type: ignore[call-arg]
            return 0

        except Exception as e:
            file, lineno, func_name = get_error_details(e)
            logger.error(
                "Batch worker failed",
                error=str(e),
                file=file,
                lineno=lineno,
                function=func_name,
            )  # type: ignore[call-arg]
            return 1


def get_error_details(ex: Exception) -> tuple[str, int | None, str]:
    import traceback

    """
    Must be called from within an 'except' block.

    Returns a tuple containing the filename, line number, and function name
    where the exception occurred.
    """
    # If the exception has a cause, that's the original error.
    # Otherwise, use the exception itself.
    target_exc = ex.__cause__ if ex.__cause__ else ex

    # Create a structured traceback from the target exception
    tb_exc = traceback.TracebackException.from_exception(target_exc)

    # The last frame in the stack is where the error originated
    last_frame = tb_exc.stack[-1]

    return (last_frame.filename, last_frame.lineno, last_frame.name)


def kill_llm_engine():
    """Kill the llm process identified by WORKER_VICTIM=1 environment variable."""
    logger.info("Looking for llm engine with WORKER_VICTIM=1 environment variable...")

    try:
        # Use grep and awk to find PID with WORKER_VICTIM=1 in environment
        result = subprocess.run(
            [
                "bash",
                "-c",
                "grep -zla 'WORKER_VICTIM=1' /proc/*/environ 2>/dev/null | awk -F/ '/\/proc\/[0-9]+\/environ/ {print $3}' | sort -n",
            ],
            capture_output=True,
            text=True,
        )

        pids = [pid.strip() for pid in result.stdout.strip().split("\n") if pid.strip()]

        if not pids:
            logger.info("No process found with WORKER_VICTIM=1 environment variable")
            # # Fallback to pgrep method
            # logger.info("Falling back to pgrep method...")
            # result = subprocess.run(
            #     ["pgrep", "-f", "python"], capture_output=True, text=True
            # )
            # pids = result.stdout.strip().split()

            # Filter out current process PID
            current_pid = os.getpid()
            pids = [pid for pid in pids if pid and int(pid) != current_pid]

        if not pids:
            logger.info("No server process found to terminate")
            return

        # Kill the first server process found
        server_pid = int(pids[0])
        logger.info(f"Found server process with PID: {server_pid}. Sending SIGTERM...")
        os.kill(server_pid, signal.SIGINT)
        logger.info("SIGTERM sent to server process")
    except subprocess.CalledProcessError as e:
        logger.warning(f"Process discovery command failed: {e}")
    except ProcessLookupError:
        logger.info("Server process already terminated")
    except Exception as e:
        logger.error(f"Error while terminating server process: {e}")


async def worker_main() -> int:
    """Main entry point for the batch worker."""
    loop = asyncio.get_running_loop()

    # --- Add Signal Handlers ---
    stop = loop.create_future()
    loop.add_signal_handler(signal.SIGTERM, stop.set_result, None)
    loop.add_signal_handler(signal.SIGINT, stop.set_result, None)

    # --- Run your main logic ---
    logger.info("Worker starting...")
    worker = BatchWorker()
    worker_task = asyncio.create_task(worker.run())

    # --- Wait for either the task to complete or a stop signal ---
    done, pending = await asyncio.wait(
        [stop, worker_task],
        return_when=asyncio.FIRST_COMPLETED,
    )

    if stop in done:
        logger.info("Shutdown signal received, cancelling tasks...")
        # Gracefully cancel pending tasks
        for task in pending:
            task.cancel()
        await asyncio.gather(*pending, return_exceptions=True)
        return 1  # Return a non-zero exit code for signal termination

    # If the worker task finished on its own
    logger.info("Worker finished normally.")

    kill_llm_engine()

    return worker_task.result()


def main():
    try:
        code = asyncio.run(worker_main())
        sys.exit(code)
    except asyncio.CancelledError:
        logger.info("Main task was cancelled.")
        sys.exit(1)


if __name__ == "__main__":
    main()
