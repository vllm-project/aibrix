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
import argparse
import asyncio
import os
import signal
import subprocess
import sys
import time
from datetime import datetime, timezone
from typing import Optional
from urllib.parse import urlparse

import httpx

import aibrix.batch.constant as constant
from aibrix.batch.driver import BatchDriver
from aibrix.batch.job_entity import (
    BatchJob,
    BatchJobSpec,
    BatchJobState,
    BatchJobStatus,
    BatchJobTransformer,
)
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


class BatchWorker:
    """Batch worker that processes jobs using the sidecar pattern."""

    def __init__(self) -> None:
        self.health_checker: Optional[LLMHealthChecker] = None
        self.driver: Optional[BatchDriver] = None
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

    def load_job_from_k8s(self, llm_engine_container_name: str) -> BatchJob:
        """Load and transform the parent Kubernetes Job to BatchJob."""
        logger.info("Loading job specification from Kubernetes API...")

        # Load k8s batch api client
        from kubernetes import client, config

        try:
            config.load_incluster_config()
        except config.ConfigException:
            logger.warning("Failed to load in-cluster config, trying local config...")
            config.load_kube_config()
        batch_api_client = client.BatchV1Api()

        # Get the Job name and namespace from environment variables
        # Get basic job information
        job_name = os.getenv("JOB_NAME")
        namespace = os.getenv("JOB_NAMESPACE")
        if not job_name:
            raise ValueError("JOB_NAME environment variable is required")
        if not namespace:
            raise ValueError("JOB_NAMESPACE environment variable is required")

        logger.info("Confirmed Job", name=job_name, namespace=namespace)  # type: ignore[call-arg]

        try:
            # Fetch the Job object
            logger.info("Fetching Job spec from the Kubernetes API...")
            k8s_job = batch_api_client.read_namespaced_job(
                name=job_name, namespace=namespace
            )

            # Extract LLM engine information and initialize health checker
            health_url = os.getenv("LLM_READY_ENDPOINT", "http://localhost:8000/health")
            self.health_checker = LLMHealthChecker(health_url)

            # Transform k8s Job to BatchJob using BatchJobTransformer
            batch_job = BatchJobTransformer.from_k8s_job(k8s_job)
            logger.info(
                "Successfully transformed k8s Job to BatchJob", job_id=batch_job.job_id
            )  # type: ignore[call-arg]

            return batch_job

        except client.ApiException as e:
            raise RuntimeError(f"Error fetching Job from Kubernetes API: {e}") from e
        except Exception as e:
            raise RuntimeError(f"Error transforming Job to BatchJob: {e}") from e

    def extract_llm_engine_info(self, k8s_job, llm_engine_container_name: str):
        """Extract LLM engine health check information from k8s job object."""
        logger.info(
            "Extracting LLM engine info for container",
            container_name=llm_engine_container_name,
        )  # type: ignore[call-arg]

        # Extract health check endpoint from livenessProbe or readinessProbe
        ready_url = "http://localhost:8000/health"  # default
        check_interval = 5  # default
        try:
            # Navigate to pod template containers
            pod_template = k8s_job.spec.template
            containers = pod_template.spec.containers

            # Find the LLM engine container
            llm_container = None
            for container in containers:
                if container.name == llm_engine_container_name:
                    llm_container = container
                    break

            if not llm_container:
                logger.warning(
                    "LLM engine container not found, using defaults",
                    container_name=llm_engine_container_name,
                )  # type: ignore[call-arg]
                return ready_url, check_interval

            probe = None
            probe_type = None

            # Prefer livenessProbe, fallback to readinessProbe
            if llm_container.readiness_probe:
                probe = llm_container.readiness_probe
                probe_type = "readiness"
            elif llm_container.readiness_probe:
                probe = llm_container.readiness_probe
                probe_type = "readiness"

            if probe and probe.http_get:
                # Extract interval from periodSeconds
                if probe.period_seconds:
                    check_interval = probe.period_seconds

                # Extract health endpoint from HTTP get action
                http_get = probe.http_get
                scheme = http_get.scheme or "http"
                host = http_get.host or "localhost"
                port = http_get.port or 8000
                path = http_get.path or "/health"

                ready_url = f"{scheme.lower()}://{host}:{port}{path}"
                logger.info(
                    "Found health probe, extracting health check info",
                    probe_type=probe_type,
                    endpoint=ready_url,
                    interval=check_interval,
                )  # type: ignore[call-arg]
            else:
                logger.info(
                    "No liveness or readiness probe found, using default health check"
                )

            # Try to construct base URL from health URL
            from urllib.parse import urlparse

            parsed = urlparse(ready_url)
            base_url = f"{parsed.scheme}://{parsed.netloc}"

            self.llm_engine_base_url = base_url
            logger.info("Set LLM engine base URL", base_url=base_url)  # type: ignore[call-arg]

            return ready_url, check_interval

        except Exception as e:
            logger.warning(
                "Error extracting LLM engine info, using defaults",
                error=str(e),
                endpoint=ready_url,
                interval=check_interval,
            )  # type: ignore[call-arg]
            return ready_url, check_interval

    async def execute_batch_job(self, batch_job: BatchJob) -> str:
        """Execute the provided batch job."""
        assert (
            self.driver is not None
        ), "Driver must be initialized before executing jobs"

        job_id = batch_job.job_id
        if job_id is None:
            raise RuntimeError("BatchJob job_id is None")

        logger.info(
            "Executing batch job",
            job_id=job_id,
            input_file_id=batch_job.spec.input_file_id,
            endpoint=batch_job.spec.endpoint,
        )  # type: ignore[call-arg]

        # Commit job to job manager
        await self.driver.job_manager.job_committed_handler(batch_job)
        logger.info("Job committed to manager", job_id=job_id)  # type: ignore[call-arg]

        # Wait until job reaches FINALIZING state
        await self.wait_for_finalizing(job_id)

        return job_id

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

    async def wait_for_finalizing(self, job_id: str, max_wait: int = 600):
        """Wait for job to reach FINALIZING state."""
        assert (
            self.driver is not None
        ), "Driver must be initialized before waiting for jobs"

        start_time = time.time()

        while time.time() - start_time < max_wait:
            job = await self.driver.job_manager.get_job(job_id)
            if job and job.status.finished:
                logger.info(
                    "Job reached final state",
                    job_id=job_id,
                    state=job.status.state.value,
                )  # type: ignore[call-arg]
                return

            await asyncio.sleep(1)

        raise TimeoutError(
            f"Job {job_id} did not reach final state within {max_wait} seconds"
        )

    async def run(self, args: argparse.Namespace) -> int:
        """Main worker execution flow."""
        try:
            # Step 1: Load job specification
            batchJob: Optional[BatchJob] = None
            try:
                if args.load_job_from_api:
                    batch_job = self.load_job_from_k8s(args.llm_engine_container_name)
            except Exception:
                pass

            # Use env as a fallback
            if batchJob is None:
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
            assert (
                self.health_checker is not None
            ), "Health checker should be initialized"
            if not await self.health_checker.wait_for_ready():
                logger.error("vLLM service failed to become ready")
                return 1

            # Step 3: Initialize BatchDriver
            self.driver = BatchDriver(llm_engine_endpoint=self.llm_engine_base_url)
            await self.driver.start()
            logger.info("BatchDriver initialized successfully")

            # Step 4: Execute batch job
            job_id = await self.execute_batch_job(batch_job)
            logger.info("Batch worker completed successfully", job_id=job_id)  # type: ignore[call-arg]

            await self.driver.stop()
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

        finally:
            # Cleanup driver if initialized
            if self.driver:
                logger.info("Cleaning up BatchDriver...")
                await self.driver.stop()


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

    parser = argparse.ArgumentParser()
    parser.add_argument(
        "--load-job-from-api",
        action="store_true",
        help="load job spec from api server",
    )
    parser.add_argument(
        "--llm-engine-container-name",
        type=str,
        default="llm-engine",
        help="container name of the llm engine",
    )
    args = parser.parse_args()

    # --- Run your main logic ---
    logger.info("Worker starting...")
    worker = BatchWorker()
    worker_task = asyncio.create_task(worker.run(args))

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
